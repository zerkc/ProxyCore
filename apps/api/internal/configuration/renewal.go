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

// RenewalOptions configures Let's Encrypt automatic renewal.
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

// ListLetsEncryptDueForRenewal returns active LE certs past renew_after.
func (s *Store) ListLetsEncryptDueForRenewal(ctx context.Context, now time.Time) ([]domain.CertificateStatus, error) {
	rows, err := s.pool.Query(ctx, `
		select id::text, hostnames, issuer::text, challenge::text, environment, status::text,
			expires_at, renew_after, key_secret_id::text, certificate_pem, failure_reason
		from certificates
		where issuer = 'letsencrypt'
			and status = 'active'
			and challenge in ('http-01', 'dns-01')
			and renew_after is not null
			and renew_after <= $1
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
		options.PropagationSeconds = 30
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

	secretID, err := s.secrets.Put(ctx, "certificate-private-key", material.PrivateKeyPEM)
	if err != nil {
		_ = s.markRenewalFailure(ctx, cert.ID, err.Error())
		return RenewResult{}, err
	}

	expires := material.ExpiresAt
	renew := renewalDate(material.ExpiresAt)
	updated, err := s.updateActiveLetsEncryptCertificate(ctx, cert.ID, secretID, material.CertificatePEM, expires, renew)
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

// RenewDueLetsEncryptCertificates renews every LE certificate that is due.
func (s *Store) RenewDueLetsEncryptCertificates(ctx context.Context, opts RenewalOptions) (renewed int, failed int, err error) {
	now := time.Now().UTC()
	if opts.Now != nil {
		now = opts.Now().UTC()
	}
	logger := opts.Log
	if logger == nil {
		logger = log.Default()
	}

	due, err := s.ListLetsEncryptDueForRenewal(ctx, now)
	if err != nil {
		return 0, 0, err
	}
	for _, cert := range due {
		result, renewErr := s.RenewLetsEncryptCertificate(ctx, cert, opts)
		if renewErr != nil {
			failed++
			logger.Printf("letsencrypt renew %s (%v): %v", cert.ID, cert.Hostnames, renewErr)
			continue
		}
		renewed++
		if result.Applied {
			logger.Printf("letsencrypt renew %s (%v): renewed and apply queued (%s)", cert.ID, cert.Hostnames, result.JobID)
		} else {
			logger.Printf("letsencrypt renew %s (%v): renewed (apply not queued)", cert.ID, cert.Hostnames)
		}
	}
	return renewed, failed, nil
}

func directoryURLForEnvironment(environment, stagingURL, productionURL string) string {
	switch strings.ToLower(strings.TrimSpace(environment)) {
	case "production":
		return productionURL
	default:
		return stagingURL
	}
}

func (s *Store) updateActiveLetsEncryptCertificate(
	ctx context.Context,
	id, secretID, certificatePEM string,
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
			and issuer = 'letsencrypt'
			and status = 'active'
		returning id::text, hostnames, issuer::text, challenge::text, environment, status::text,
			expires_at, renew_after, key_secret_id::text, certificate_pem, failure_reason
	`, id, expiresAt, renewAfter, secretID, certificatePEM)
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
			and issuer = 'letsencrypt'
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
