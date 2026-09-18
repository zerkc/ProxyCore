package configuration

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
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
	CertificatePEM   string    `json:"certificatePem"`
	PrivateKeyPEM    string    `json:"-"`
	CACertificatePEM string    `json:"caCertificatePem"`
	CADERHashSHA256  string    `json:"caDerSha256"`
	ExpiresAt        time.Time `json:"expiresAt"`
}

// EnrollmentTLSPublicMaterial is the redacted trust projection safe for Owner-facing serialization.
type EnrollmentTLSPublicMaterial struct {
	CertificatePEM   string    `json:"certificatePem"`
	CACertificatePEM string    `json:"caCertificatePem"`
	CADERHashSHA256  string    `json:"caDerSha256"`
	ExpiresAt        time.Time `json:"expiresAt"`
}

// EnrollmentTLSTrust describes the read-only, established enrollment trust state.
type EnrollmentTLSTrust struct {
	Configured bool
	Ready      bool
	Material   EnrollmentTLSPublicMaterial
}

// Public returns enrollment certificate and CA trust material without private or secret data.
func (m EnrollmentTLSMaterial) Public() EnrollmentTLSPublicMaterial {
	return EnrollmentTLSPublicMaterial{
		CertificatePEM:   m.CertificatePEM,
		CACertificatePEM: m.CACertificatePEM,
		CADERHashSHA256:  m.CADERHashSHA256,
		ExpiresAt:        m.ExpiresAt,
	}
}

type validatedEnrollmentLeaf struct {
	certificate *x509.Certificate
	ca          *x509.Certificate
}

func newEnrollmentTLSMaterial(_ string, privateKeyPEM, caPEM string, validated validatedEnrollmentLeaf) EnrollmentTLSMaterial {
	return EnrollmentTLSMaterial{
		CertificatePEM:   canonicalCertificatePEM(validated.certificate),
		PrivateKeyPEM:    privateKeyPEM,
		CACertificatePEM: caPEM,
		CADERHashSHA256:  acme.DERHashSHA256(validated.ca.Raw),
		ExpiresAt:        validated.certificate.NotAfter,
	}
}

var enrollmentTLSMu sync.Mutex

// EnsureInternalCACertificatePEM returns the ProxyCore internal CA certificate,
// creating the CA on first use.
func (s *Store) EnsureInternalCACertificatePEM(ctx context.Context) (string, error) {
	certPEM, _, err := s.ensureInternalCA(ctx)
	return certPEM, err
}

// LoadEnrollmentTLSTrust loads established enrollment trust without creating,
// renewing, or otherwise mutating any configuration or certificate material.
func (s *Store) LoadEnrollmentTLSTrust(ctx context.Context) (EnrollmentTLSTrust, error) {
	configured, hostnames, err := s.loadEnrollmentHostnamesReadOnly(ctx)
	if err != nil {
		return EnrollmentTLSTrust{}, err
	}
	trust := EnrollmentTLSTrust{Configured: configured}
	if !configured {
		return trust, nil
	}
	public, established, err := s.loadEstablishedEnrollmentTLSPublicMaterial(ctx, hostnames)
	if err != nil {
		return EnrollmentTLSTrust{}, err
	}
	if !established {
		return trust, nil
	}
	trust.Ready = true
	trust.Material = public
	return trust, nil
}

func (s *Store) loadEnrollmentHostnamesReadOnly(ctx context.Context) (bool, []string, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx,
		`select enrollment_hostnames from installation_settings where id = $1`, installationID,
	).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil, nil
	}
	if err != nil {
		return false, nil, err
	}
	if len(raw) == 0 || string(raw) == "null" {
		return false, nil, nil
	}
	var hostnames []string
	if err := json.Unmarshal(raw, &hostnames); err != nil {
		return false, nil, fmt.Errorf("read enrollment hostnames: %w", err)
	}
	canonical, err := NormalizeEnrollmentHostnames(hostnames)
	if err != nil {
		return false, nil, fmt.Errorf("read enrollment hostnames: %w", err)
	}
	return len(canonical) != 0, canonical, nil
}

func (s *Store) loadEstablishedEnrollmentTLSPublicMaterial(ctx context.Context, hostnames []string) (EnrollmentTLSPublicMaterial, bool, error) {
	var establishedAt time.Time
	err := s.pool.QueryRow(ctx,
		`select established_at from internal_ca_enrollment_state where id = $1`, enrollmentStateID,
	).Scan(&establishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		var leafPEM, leafSecretID []byte
		rowErr := s.pool.QueryRow(ctx, `
			select enrollment_certificate_pem, enrollment_key_secret_id::text
			  from internal_ca
			 where id = $1`, internalCAID).Scan(&leafPEM, &leafSecretID)
		if errors.Is(rowErr, pgx.ErrNoRows) {
			return EnrollmentTLSPublicMaterial{}, false, nil
		}
		if rowErr != nil {
			return EnrollmentTLSPublicMaterial{}, false, rowErr
		}
		if strings.TrimSpace(string(leafPEM)) != "" || strings.TrimSpace(string(leafSecretID)) != "" {
			return EnrollmentTLSPublicMaterial{}, false, errors.New("enrollment TLS material exists without its establishment marker")
		}
		return EnrollmentTLSPublicMaterial{}, false, nil
	}
	if err != nil {
		return EnrollmentTLSPublicMaterial{}, false, err
	}

	var caPEM, caSecretID string
	var leafPEM, leafSecretID []byte
	if err := s.pool.QueryRow(ctx, `
		select certificate_pem, key_secret_id::text, enrollment_certificate_pem, enrollment_key_secret_id::text
		  from internal_ca
		 where id = $1`, internalCAID).Scan(&caPEM, &caSecretID, &leafPEM, &leafSecretID); err != nil {
		return EnrollmentTLSPublicMaterial{}, false, fmt.Errorf("load established enrollment CA: %w", err)
	}
	if strings.TrimSpace(caPEM) == "" || strings.TrimSpace(caSecretID) == "" || strings.TrimSpace(string(leafPEM)) == "" || strings.TrimSpace(string(leafSecretID)) == "" {
		return EnrollmentTLSPublicMaterial{}, false, errors.New("established enrollment TLS material is incomplete")
	}
	if s.secrets == nil {
		return EnrollmentTLSPublicMaterial{}, false, errors.New("established enrollment TLS material is unavailable")
	}
	caKeyPEM, err := s.secrets.Get(ctx, caSecretID)
	if err != nil {
		return EnrollmentTLSPublicMaterial{}, false, err
	}
	leafKeyPEM, err := s.secrets.Get(ctx, string(leafSecretID))
	if err != nil {
		return EnrollmentTLSPublicMaterial{}, false, err
	}
	if err := acme.ValidateInternalCAMaterial(caPEM, caKeyPEM); err != nil {
		return EnrollmentTLSPublicMaterial{}, false, fmt.Errorf("validate established internal CA: %w", err)
	}
	validated, err := validateEnrollmentLeaf(string(leafPEM), leafKeyPEM, caPEM, caKeyPEM)
	if err != nil {
		return EnrollmentTLSPublicMaterial{}, false, fmt.Errorf("validate established enrollment TLS material: %w", err)
	}
	if !sameEnrollmentHostnames(validated.certificate, hostnames) {
		return EnrollmentTLSPublicMaterial{}, false, errors.New("established enrollment leaf certificate SANs do not cover configured hostnames")
	}
	material := newEnrollmentTLSMaterial(string(leafPEM), leafKeyPEM, caPEM, validated)
	return material.Public(), true, nil
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
		validated, err := validateEnrollmentLeaf(issued.CertificatePEM, issued.PrivateKeyPEM, caCertPEM, caKeyPEM)
		if err != nil || !sameEnrollmentHostnames(validated.certificate, configured.Hostnames) {
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
		return newEnrollmentTLSMaterial(issued.CertificatePEM, issued.PrivateKeyPEM, caCertPEM, validated), nil
	}
	keyPEM, err := s.secrets.Get(ctx, keySecretID)
	if err != nil {
		return EnrollmentTLSMaterial{}, err
	}
	if strings.TrimSpace(keyPEM) == "" {
		return EnrollmentTLSMaterial{}, errors.New("enrollment leaf private key secret is missing")
	}
	validated, err := validateEnrollmentLeaf(certificatePEM, keyPEM, caCertPEM, caKeyPEM)
	if err != nil {
		return EnrollmentTLSMaterial{}, fmt.Errorf("validate established enrollment TLS material: %w", err)
	}
	if sameEnrollmentHostnames(validated.certificate, configured.Hostnames) && validated.certificate.NotAfter.After(time.Now().UTC().Add(enrollmentLeafRenewalWindow)) {
		if err := tx.Commit(ctx); err != nil {
			return EnrollmentTLSMaterial{}, err
		}
		return newEnrollmentTLSMaterial(certificatePEM, keyPEM, caCertPEM, validated), nil
	}
	issued, err := acme.IssueSignedByCAWithKey(configured.Hostnames, enrollmentLeafValidityDays, caCertPEM, caKeyPEM, keyPEM)
	if err != nil {
		return EnrollmentTLSMaterial{}, fmt.Errorf("renew enrollment TLS material: %w", err)
	}
	validated, err = validateEnrollmentLeaf(issued.CertificatePEM, issued.PrivateKeyPEM, caCertPEM, caKeyPEM)
	if err != nil || !sameEnrollmentHostnames(validated.certificate, configured.Hostnames) {
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
	return newEnrollmentTLSMaterial(issued.CertificatePEM, keyPEM, caCertPEM, validated), nil
}

func validateEnrollmentLeaf(certificatePEM, keyPEM, caCertPEM, caKeyPEM string) (validatedEnrollmentLeaf, error) {
	certificate, err := parseSingleCertificatePEM(certificatePEM)
	if err != nil {
		return validatedEnrollmentLeaf{}, errors.New("enrollment leaf certificate PEM is invalid")
	}
	pair, err := tls.X509KeyPair([]byte(certificatePEM), []byte(keyPEM))
	if err != nil || len(pair.Certificate) != 1 || !bytes.Equal(pair.Certificate[0], certificate.Raw) {
		return validatedEnrollmentLeaf{}, errors.New("enrollment leaf certificate and private key are invalid or do not match")
	}
	if certificate.IsCA {
		return validatedEnrollmentLeaf{}, errors.New("enrollment leaf certificate is invalid")
	}
	serverAuth := false
	for _, usage := range certificate.ExtKeyUsage {
		serverAuth = serverAuth || usage == x509.ExtKeyUsageServerAuth
	}
	for _, name := range certificate.DNSNames {
		if net.ParseIP(name) != nil {
			return validatedEnrollmentLeaf{}, errors.New("enrollment leaf certificate encodes an IP as a DNS SAN")
		}
	}
	if !serverAuth || len(certificate.DNSNames)+len(certificate.IPAddresses) == 0 {
		return validatedEnrollmentLeaf{}, errors.New("enrollment leaf certificate lacks ServerAuth or SANs")
	}
	caPair, err := tls.X509KeyPair([]byte(caCertPEM), []byte(caKeyPEM))
	if err != nil || len(caPair.Certificate) == 0 {
		return validatedEnrollmentLeaf{}, errors.New("internal CA certificate and key are invalid")
	}
	caCert, err := x509.ParseCertificate(caPair.Certificate[0])
	if err != nil {
		return validatedEnrollmentLeaf{}, errors.New("internal CA certificate is invalid")
	}
	roots := x509.NewCertPool()
	roots.AddCert(caCert)
	if _, err := certificate.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		return validatedEnrollmentLeaf{}, errors.New("enrollment leaf certificate does not chain to the internal CA")
	}
	return validatedEnrollmentLeaf{certificate: certificate, ca: caCert}, nil
}

func parseSingleCertificatePEM(certificatePEM string) (*x509.Certificate, error) {
	data := []byte(certificatePEM)
	if !bytes.HasPrefix(data, []byte("-----BEGIN CERTIFICATE-----")) {
		return nil, errors.New("certificate PEM must start with a CERTIFICATE block")
	}
	block, rest := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("certificate PEM must contain exactly one CERTIFICATE block")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, err
	}
	return certificate, nil
}

func canonicalCertificatePEM(certificate *x509.Certificate) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw}))
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
