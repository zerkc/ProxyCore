package configuration

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zerkc/ProxyCore/apps/api/internal/acme"
)

const (
	internalCAID                = "default"
	enrollmentStateID           = "default"
	EnrollmentLeafKeyPurpose    = "internal-ca-enrollment-leaf-private-key"
	enrollmentLeafValidityDays  = 365
	enrollmentLeafRenewalWindow = 30 * 24 * time.Hour
)

type EnrollmentTLSMaterial struct {
	CertificatePEM string
	PrivateKeyPEM  string
	ExpiresAt      time.Time
}

var enrollmentTLSMu sync.Mutex

// EnsureInternalCACertificatePEM returns the ProxyCore internal CA certificate,
// creating the CA on first use.
func (s *Store) EnsureInternalCACertificatePEM(ctx context.Context) (string, error) {
	certPEM, _, err := s.ensureInternalCA(ctx)
	return certPEM, err
}

func (s *Store) ensureInternalCA(ctx context.Context) (certPEM, keyPEM string, err error) {
	if s.secrets == nil {
		return "", "", errors.New("internal CA requires a master key")
	}
	certPEM, keyPEM, err = s.loadInternalCA(ctx)
	if err != nil {
		return "", "", err
	}
	if certPEM != "" && keyPEM != "" {
		return certPEM, keyPEM, nil
	}
	material, err := acme.CreateInternalCA(0)
	if err != nil {
		return "", "", err
	}
	secretID, err := s.secrets.Put(ctx, "internal-ca-private-key", material.PrivateKeyPEM)
	if err != nil {
		return "", "", err
	}
	if _, err = s.pool.Exec(ctx, `insert into internal_ca (id, certificate_pem, key_secret_id) values ($1, $2, $3) on conflict (id) do nothing`, internalCAID, material.CertificatePEM, secretID); err != nil {
		return "", "", fmt.Errorf("persist internal CA: %w", err)
	}
	return s.loadInternalCA(ctx)
}

func (s *Store) loadInternalCA(ctx context.Context) (certPEM, keyPEM string, err error) {
	var secretID string
	if err = s.pool.QueryRow(ctx, `select certificate_pem, key_secret_id::text from internal_ca where id = $1`, internalCAID).Scan(&certPEM, &secretID); errors.Is(err, pgx.ErrNoRows) {
		return "", "", nil
	}
	if err != nil {
		return "", "", err
	}
	if strings.TrimSpace(certPEM) == "" || strings.TrimSpace(secretID) == "" {
		return "", "", errors.New("established internal CA material is incomplete")
	}
	keyPEM, err = s.secrets.Get(ctx, secretID)
	if err != nil {
		return "", "", err
	}
	if strings.TrimSpace(keyPEM) == "" {
		return "", "", errors.New("internal CA private key secret is missing")
	}
	if err := acme.ValidateInternalCAMaterial(certPEM, keyPEM); err != nil {
		return "", "", fmt.Errorf("validate established internal CA: %w", err)
	}
	return certPEM, keyPEM, nil
}

func (s *Store) EnsureEnrollmentTLSMaterial(ctx context.Context) (EnrollmentTLSMaterial, error) {
	if s.secrets == nil {
		return EnrollmentTLSMaterial{}, errors.New("enrollment TLS requires a master key")
	}
	configured, err := s.GetEnrollmentHostnames(ctx)
	if err != nil {
		return EnrollmentTLSMaterial{}, err
	}
	if !configured.Configured {
		return EnrollmentTLSMaterial{}, errors.New("enrollment hostnames must be configured before issuing TLS material")
	}
	enrollmentTLSMu.Lock()
	defer enrollmentTLSMu.Unlock()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return EnrollmentTLSMaterial{}, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock(hashtextextended('proxycore/enrollment-tls', 0))`); err != nil {
		return EnrollmentTLSMaterial{}, fmt.Errorf("lock enrollment TLS identity: %w", err)
	}
	established, err := enrollmentEstablished(ctx, tx)
	if err != nil {
		return EnrollmentTLSMaterial{}, err
	}
	caCertPEM, caKeyPEM, err := s.enrollmentCA(ctx, tx, established)
	if err != nil {
		return EnrollmentTLSMaterial{}, err
	}
	certificatePEM, keySecretID, err := loadEnrollmentLeaf(ctx, tx)
	if err != nil {
		return EnrollmentTLSMaterial{}, err
	}
	if !established && (certificatePEM != "" || keySecretID != "") {
		return EnrollmentTLSMaterial{}, errors.New("enrollment TLS material exists without its establishment marker")
	}
	if established && (certificatePEM == "" || keySecretID == "") {
		return EnrollmentTLSMaterial{}, errors.New("established enrollment TLS material is incomplete")
	}
	if !established {
		issued, err := acme.IssueSignedByCAWithKey(configured.Hostnames, enrollmentLeafValidityDays, caCertPEM, caKeyPEM, "")
		if err != nil {
			return EnrollmentTLSMaterial{}, err
		}
		certificate, err := validateEnrollmentLeaf(issued.CertificatePEM, issued.PrivateKeyPEM, caCertPEM, caKeyPEM)
		if err != nil || !sameEnrollmentHostnames(certificate, configured.Hostnames) {
			if err == nil {
				err = errors.New("enrollment leaf certificate SANs do not cover configured hostnames")
			}
			return EnrollmentTLSMaterial{}, err
		}
		secretID, err := s.secrets.Put(ctx, EnrollmentLeafKeyPurpose, issued.PrivateKeyPEM)
		if err != nil || strings.TrimSpace(secretID) == "" {
			if err == nil {
				err = errors.New("secret store returned an empty id")
			}
			return EnrollmentTLSMaterial{}, fmt.Errorf("store enrollment leaf private key: %w", err)
		}
		if err := persistEnrollmentLeaf(ctx, tx, issued.CertificatePEM, secretID); err != nil {
			return EnrollmentTLSMaterial{}, err
		}
		if _, err := tx.Exec(ctx, `insert into internal_ca_enrollment_state (id, established_at) values ($1, now())`, enrollmentStateID); err != nil {
			return EnrollmentTLSMaterial{}, fmt.Errorf("mark enrollment TLS identity established: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return EnrollmentTLSMaterial{}, err
		}
		return EnrollmentTLSMaterial{CertificatePEM: issued.CertificatePEM, PrivateKeyPEM: issued.PrivateKeyPEM, ExpiresAt: certificate.NotAfter}, nil
	}
	keyPEM, err := s.secrets.Get(ctx, keySecretID)
	if err != nil {
		return EnrollmentTLSMaterial{}, err
	}
	if strings.TrimSpace(keyPEM) == "" {
		return EnrollmentTLSMaterial{}, errors.New("enrollment leaf private key secret is missing")
	}
	certificate, err := validateEnrollmentLeaf(certificatePEM, keyPEM, caCertPEM, caKeyPEM)
	if err != nil {
		return EnrollmentTLSMaterial{}, fmt.Errorf("validate established enrollment TLS material: %w", err)
	}
	if sameEnrollmentHostnames(certificate, configured.Hostnames) && certificate.NotAfter.After(time.Now().UTC().Add(enrollmentLeafRenewalWindow)) {
		if err := tx.Commit(ctx); err != nil {
			return EnrollmentTLSMaterial{}, err
		}
		return EnrollmentTLSMaterial{CertificatePEM: certificatePEM, PrivateKeyPEM: keyPEM, ExpiresAt: certificate.NotAfter}, nil
	}
	issued, err := acme.IssueSignedByCAWithKey(configured.Hostnames, enrollmentLeafValidityDays, caCertPEM, caKeyPEM, keyPEM)
	if err != nil {
		return EnrollmentTLSMaterial{}, fmt.Errorf("renew enrollment TLS material: %w", err)
	}
	certificate, err = validateEnrollmentLeaf(issued.CertificatePEM, issued.PrivateKeyPEM, caCertPEM, caKeyPEM)
	if err != nil || !sameEnrollmentHostnames(certificate, configured.Hostnames) {
		if err == nil {
			err = errors.New("renewed enrollment leaf SANs do not cover configured hostnames")
		}
		return EnrollmentTLSMaterial{}, fmt.Errorf("validate renewed enrollment TLS material: %w", err)
	}
	if err := persistEnrollmentLeaf(ctx, tx, issued.CertificatePEM, keySecretID); err != nil {
		return EnrollmentTLSMaterial{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return EnrollmentTLSMaterial{}, err
	}
	return EnrollmentTLSMaterial{CertificatePEM: issued.CertificatePEM, PrivateKeyPEM: keyPEM, ExpiresAt: certificate.NotAfter}, nil
}

func validateEnrollmentLeaf(certificatePEM, keyPEM, caCertPEM, caKeyPEM string) (*x509.Certificate, error) {
	pair, err := tls.X509KeyPair([]byte(certificatePEM), []byte(keyPEM))
	if err != nil || len(pair.Certificate) == 0 {
		return nil, errors.New("enrollment leaf certificate and private key are invalid or do not match")
	}
	certificate, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil || certificate.IsCA {
		return nil, errors.New("enrollment leaf certificate is invalid")
	}
	serverAuth := false
	for _, usage := range certificate.ExtKeyUsage {
		serverAuth = serverAuth || usage == x509.ExtKeyUsageServerAuth
	}
	for _, name := range certificate.DNSNames {
		if net.ParseIP(name) != nil {
			return nil, errors.New("enrollment leaf certificate encodes an IP as a DNS SAN")
		}
	}
	if !serverAuth || len(certificate.DNSNames)+len(certificate.IPAddresses) == 0 {
		return nil, errors.New("enrollment leaf certificate lacks ServerAuth or SANs")
	}
	caPair, err := tls.X509KeyPair([]byte(caCertPEM), []byte(caKeyPEM))
	if err != nil || len(caPair.Certificate) == 0 {
		return nil, errors.New("internal CA certificate and key are invalid")
	}
	caCert, err := x509.ParseCertificate(caPair.Certificate[0])
	if err != nil {
		return nil, errors.New("internal CA certificate is invalid")
	}
	roots := x509.NewCertPool()
	roots.AddCert(caCert)
	if _, err := certificate.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		return nil, errors.New("enrollment leaf certificate does not chain to the internal CA")
	}
	return certificate, nil
}

func sameEnrollmentHostnames(certificate *x509.Certificate, configured []string) bool {
	names := append([]string{}, certificate.DNSNames...)
	for _, name := range certificate.DNSNames {
		if net.ParseIP(name) != nil {
			return false
		}
	}
	for _, ip := range certificate.IPAddresses {
		names = append(names, ip.String())
	}
	normalized, err := NormalizeEnrollmentHostnames(names)
	return err == nil && acme.CertificateCoversHostnames(names, configured) && strings.Join(normalized, "\x00") == strings.Join(configured, "\x00")
}

func enrollmentEstablished(ctx context.Context, tx pgx.Tx) (bool, error) {
	var establishedAt time.Time
	err := tx.QueryRow(ctx, `select established_at from internal_ca_enrollment_state where id = $1`, enrollmentStateID).Scan(&establishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (s *Store) enrollmentCA(ctx context.Context, tx pgx.Tx, established bool) (certPEM, keyPEM string, err error) {
	var secretID string
	err = tx.QueryRow(ctx, `select certificate_pem, key_secret_id::text from internal_ca where id = $1 for update`, internalCAID).Scan(&certPEM, &secretID)
	if errors.Is(err, pgx.ErrNoRows) {
		if established {
			return "", "", errors.New("established internal CA row is missing")
		}
		material, createErr := acme.CreateInternalCA(0)
		if createErr != nil {
			return "", "", createErr
		}
		secretID, createErr = s.secrets.Put(ctx, "internal-ca-private-key", material.PrivateKeyPEM)
		if createErr != nil {
			return "", "", createErr
		}
		if _, createErr = tx.Exec(ctx, `insert into internal_ca (id, certificate_pem, key_secret_id) values ($1, $2, $3)`, internalCAID, material.CertificatePEM, secretID); createErr != nil {
			return "", "", fmt.Errorf("persist internal CA: %w", createErr)
		}
		return material.CertificatePEM, material.PrivateKeyPEM, nil
	}
	if err != nil {
		return "", "", err
	}
	if strings.TrimSpace(certPEM) == "" || strings.TrimSpace(secretID) == "" {
		return "", "", errors.New("established internal CA material is incomplete")
	}
	keyPEM, err = s.secrets.Get(ctx, secretID)
	if err != nil {
		return "", "", err
	}
	if strings.TrimSpace(keyPEM) == "" {
		return "", "", errors.New("internal CA private key secret is missing")
	}
	if err := acme.ValidateInternalCAMaterial(certPEM, keyPEM); err != nil {
		return "", "", fmt.Errorf("validate established internal CA: %w", err)
	}
	return certPEM, keyPEM, nil
}

func loadEnrollmentLeaf(ctx context.Context, tx pgx.Tx) (certificatePEM, keySecretID string, err error) {
	var certificate, secretID []byte
	err = tx.QueryRow(ctx, `select enrollment_certificate_pem, enrollment_key_secret_id::text from internal_ca where id = $1`, internalCAID).Scan(&certificate, &secretID)
	if err != nil {
		return "", "", err
	}
	return string(certificate), string(secretID), nil
}

func persistEnrollmentLeaf(ctx context.Context, tx pgx.Tx, certificatePEM, keySecretID string) error {
	result, err := tx.Exec(ctx, `update internal_ca set enrollment_certificate_pem = $1, enrollment_key_secret_id = $2, updated_at = now() where id = $3`, certificatePEM, keySecretID, internalCAID)
	if err != nil {
		return fmt.Errorf("persist enrollment TLS material: %w", err)
	}
	if result.RowsAffected() != 1 {
		return errors.New("persist enrollment TLS material: internal CA row is missing")
	}
	return nil
}

func (s *Store) issueInternalLeaf(ctx context.Context, hostnames []string, validityDays int) (acme.Material, error) {
	caCertPEM, caKeyPEM, err := s.ensureInternalCA(ctx)
	if err != nil {
		return acme.Material{}, err
	}
	return acme.IssueSignedByCA(hostnames, validityDays, caCertPEM, caKeyPEM)
}
