package configuration

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zerkc/ProxyCore/apps/api/internal/acme"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
)

// issueLetsEncryptFn is overridable in tests.
var issueLetsEncryptFn = acme.IssueLetsEncrypt

// RenewalOptions configures automatic certificate renewal.
type RenewalOptions struct {
	StagingDirectoryURL    string
	ProductionDirectoryURL string
	Email                  string
	Now                    func() time.Time
	Log                    *log.Logger
}

// RenewResult is the outcome of one certificate renewal attempt.
type RenewResult struct {
	Certificate domain.CertificateStatus
	Applied     bool
	JobID       string
}

// ListCertificatesDueForRenewal returns active LE/self-signed certs past renew_after.
func (s *Store) ListCertificatesDueForRenewal(ctx context.Context, now time.Time) ([]domain.CertificateStatus, error) {
	rows, err := s.pool.Query(ctx, `
		select id::text, hostnames, issuer::text, challenge::text, environment, status::text,
			expires_at, renew_after, key_secret_id::text, certificate_pem, failure_reason
		from certificates
		where status = 'active'
			and renew_after is not null
			and renew_after <= $1
			and (
				(issuer = 'letsencrypt' and challenge in ('http-01', 'dns-01'))
				or (issuer = 'self-signed' and challenge = 'none')
			)
		order by renew_after asc
	`, now.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.CertificateStatus{}
	for rows.Next() {
		cert, err := scanCertificate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, cert)
	}
	return out, rows.Err()
}

// ListLetsEncryptDueForRenewal returns active LE certs past renew_after.
func (s *Store) ListLetsEncryptDueForRenewal(ctx context.Context, now time.Time) ([]domain.CertificateStatus, error) {
	all, err := s.ListCertificatesDueForRenewal(ctx, now)
	if err != nil {
		return nil, err
	}
	out := []domain.CertificateStatus{}
	for _, cert := range all {
		if cert.Issuer == "letsencrypt" {
			out = append(out, cert)
		}
	}
	return out, nil
}

// RenewLetsEncryptCertificate re-issues one active LE certificate in place.
// On failure the active certificate row is left unchanged except failure_reason.
func (s *Store) RenewLetsEncryptCertificate(ctx context.Context, cert domain.CertificateStatus, opts RenewalOptions) (RenewResult, error) {
	if cert.Issuer != "letsencrypt" {
		return RenewResult{}, errors.New("automatic renewal is only supported for Let's Encrypt certificates")
	}
	if cert.Status != "active" {
		return RenewResult{}, fmt.Errorf("certificate %s is not active", cert.ID)
	}
	if cert.Challenge != "http-01" && cert.Challenge != "dns-01" {
		return RenewResult{}, fmt.Errorf("certificate %s has unsupported challenge %q", cert.ID, cert.Challenge)
	}
	if s.secrets == nil {
		return RenewResult{}, errors.New("Let's Encrypt renewal requires a master key")
	}

	directoryURL := directoryURLForEnvironment(cert.Environment, opts.StagingDirectoryURL, opts.ProductionDirectoryURL)
	if directoryURL == "" {
		return RenewResult{}, errors.New("ACME directory URL is required for renewal")
	}

	options := acme.LetsEncryptOptions{
		Hostnames:    cert.Hostnames,
		DirectoryURL: directoryURL,
		Email:        opts.Email,
		Challenge:    cert.Challenge,
		KeyType:      "rsa",
	}
	if cert.Challenge == "http-01" {
		options.Http01 = acme.GlobalHttp01Store()
	}
	if cert.Challenge == "dns-01" {
		options.PropagationSeconds = 60
		apiToken, zoneID, zoneName, err := s.resolveCloudflareCredentials(ctx, nil)
		if err != nil {
			_ = s.markRenewalFailure(ctx, cert.ID, err.Error())
			return RenewResult{}, err
		}
		options.Dns01 = acme.NewCloudflareDns01Adapter(acme.CloudflareOptions{
			APIToken: apiToken,
			ZoneID:   zoneID,
			ZoneName: zoneName,
		})
	}

	material, err := issueLetsEncryptFn(ctx, options)
	if err != nil {
		_ = s.markRenewalFailure(ctx, cert.ID, err.Error())
		return RenewResult{}, err
	}
	return s.persistRenewedCertificate(ctx, cert, material)
}

// RenewSelfSignedCertificateByID loads and regenerates an active self-signed certificate.
func (s *Store) RenewSelfSignedCertificateByID(ctx context.Context, id string) (RenewResult, error) {
	cert, err := s.GetCertificate(ctx, id)
	if err != nil {
		return RenewResult{}, err
	}
	return s.RenewSelfSignedCertificate(ctx, cert)
}

// RenewSelfSignedCertificate re-issues one active internal (CA-signed) certificate in place.
func (s *Store) RenewSelfSignedCertificate(ctx context.Context, cert domain.CertificateStatus) (RenewResult, error) {
	if cert.Issuer != "self-signed" {
		return RenewResult{}, errors.New("self-signed renewal requires issuer self-signed")
	}
	if cert.Status != "active" {
		return RenewResult{}, fmt.Errorf("certificate %s is not active", cert.ID)
	}
	if cert.Challenge != "none" {
		return RenewResult{}, fmt.Errorf("certificate %s has unsupported challenge %q", cert.ID, cert.Challenge)
	}
	if s.secrets == nil {
		return RenewResult{}, errors.New("self-signed renewal requires a master key")
	}

	material, err := s.issueInternalLeaf(ctx, cert.Hostnames, 365)
	if err != nil {
		_ = s.markRenewalFailure(ctx, cert.ID, err.Error())
		return RenewResult{}, err
	}
	return s.persistRenewedCertificate(ctx, cert, material)
}

func (s *Store) persistRenewedCertificate(ctx context.Context, cert domain.CertificateStatus, material acme.Material) (RenewResult, error) {
	secretID, err := s.secrets.Put(ctx, "certificate-private-key", material.PrivateKeyPEM)
	if err != nil {
		_ = s.markRenewalFailure(ctx, cert.ID, err.Error())
		return RenewResult{}, err
	}

	expires := material.ExpiresAt
	renew := renewalDate(material.ExpiresAt)
	updated, err := s.updateActiveCertificateMaterial(ctx, cert.ID, cert.Issuer, secretID, material.CertificatePEM, expires, renew)
	if err != nil {
		return RenewResult{}, err
	}

	result := RenewResult{Certificate: updated}
	ownerID, err := s.firstOwnerID(ctx)
	if err != nil {
		return result, fmt.Errorf("certificate renewed but could not enqueue apply: %w", err)
	}
	if ownerID == "" {
		return result, nil
	}
	apply, err := s.CreateApplyJob(ctx, ownerID)
	if err != nil {
		return result, fmt.Errorf("certificate renewed but apply enqueue failed: %w", err)
	}
	result.Applied = true
	result.JobID = apply.Job.ID
	return result, nil
}

// RenewDueCertificates renews every LE/self-signed certificate that is due.
func (s *Store) RenewDueCertificates(ctx context.Context, opts RenewalOptions) (renewed int, failed int, err error) {
	now := time.Now().UTC()
	if opts.Now != nil {
		now = opts.Now().UTC()
	}
	logger := opts.Log
	if logger == nil {
		logger = log.Default()
	}

	due, err := s.ListCertificatesDueForRenewal(ctx, now)
	if err != nil {
		return 0, 0, err
	}
	for _, cert := range due {
		var result RenewResult
		var renewErr error
		switch cert.Issuer {
		case "letsencrypt":
			result, renewErr = s.RenewLetsEncryptCertificate(ctx, cert, opts)
		case "self-signed":
			result, renewErr = s.RenewSelfSignedCertificate(ctx, cert)
		default:
			continue
		}
		if renewErr != nil {
			failed++
			logger.Printf("certificate renew %s (%s %v): %v", cert.ID, cert.Issuer, cert.Hostnames, renewErr)
			continue
		}
		renewed++
		if result.Applied {
			logger.Printf("certificate renew %s (%s %v): renewed and apply queued (%s)", cert.ID, cert.Issuer, cert.Hostnames, result.JobID)
		} else {
			logger.Printf("certificate renew %s (%s %v): renewed (apply not queued)", cert.ID, cert.Issuer, cert.Hostnames)
		}
	}
	return renewed, failed, nil
}

// RenewDueLetsEncryptCertificates renews due Let's Encrypt certificates (and self-signed).
// Kept for compatibility with earlier wiring; prefer RenewDueCertificates.
func (s *Store) RenewDueLetsEncryptCertificates(ctx context.Context, opts RenewalOptions) (renewed int, failed int, err error) {
	return s.RenewDueCertificates(ctx, opts)
}

func directoryURLForEnvironment(environment, stagingURL, productionURL string) string {
	switch strings.ToLower(strings.TrimSpace(environment)) {
	case "production":
		return productionURL
	default:
		return stagingURL
	}
}

func (s *Store) updateActiveCertificateMaterial(
	ctx context.Context,
	id, issuer, secretID, certificatePEM string,
	expiresAt, renewAfter time.Time,
) (domain.CertificateStatus, error) {
	row := s.pool.QueryRow(ctx, `
		update certificates
		set status = 'active',
			expires_at = $2,
			renew_after = $3,
			key_secret_id = $4,
			certificate_pem = $5,
			failure_reason = null,
			updated_at = now()
		where id = $1
			and issuer = $6::proxycore_certificate_issuer
			and status = 'active'
		returning id::text, hostnames, issuer::text, challenge::text, environment, status::text,
			expires_at, renew_after, key_secret_id::text, certificate_pem, failure_reason
	`, id, expiresAt, renewAfter, secretID, certificatePEM, issuer)
	cert, err := scanCertificate(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.CertificateStatus{}, fmt.Errorf("certificate %s is no longer active", id)
	}
	return cert, err
}

func (s *Store) markRenewalFailure(ctx context.Context, id, reason string) error {
	_, err := s.pool.Exec(ctx, `
		update certificates
		set failure_reason = $2,
			updated_at = now()
		where id = $1
			and status = 'active'
	`, id, reason)
	return err
}

func (s *Store) firstOwnerID(ctx context.Context) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx, `
		select id::text from users
		where role = 'owner' and active = true
		order by created_at asc
		limit 1
	`).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return id, err
}
