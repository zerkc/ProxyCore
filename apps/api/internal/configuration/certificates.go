package configuration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zerkc/ProxyCore/apps/api/internal/acme"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
)

// ListCertificates returns certificates newest first.
func (s *Store) ListCertificates(ctx context.Context) ([]domain.CertificateStatus, error) {
	return listCertificatesOrdered(ctx, s.pool, "created_at desc")
}

func listCertificatesOrdered(ctx context.Context, q querier, orderBy string) ([]domain.CertificateStatus, error) {
	query := `
		select id::text, hostnames, issuer::text, challenge::text, environment, status::text,
			expires_at, renew_after, key_secret_id::text, certificate_pem, failure_reason
		from certificates order by ` + orderBy
	rows, err := q.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	certificates := []domain.CertificateStatus{}
	for rows.Next() {
		cert, err := scanCertificate(rows)
		if err != nil {
			return nil, err
		}
		certificates = append(certificates, cert)
	}
	return certificates, rows.Err()
}

func scanCertificate(row scanner) (domain.CertificateStatus, error) {
	var (
		cert          domain.CertificateStatus
		hostnamesRaw  []byte
		secretID      *string
		certificate   *string
		failureReason *string
	)
	if err := row.Scan(
		&cert.ID, &hostnamesRaw, &cert.Issuer, &cert.Challenge, &cert.Environment, &cert.Status,
		&cert.ExpiresAt, &cert.RenewAfter, &secretID, &certificate, &failureReason,
	); err != nil {
		return domain.CertificateStatus{}, err
	}
	if len(hostnamesRaw) > 0 {
		if err := json.Unmarshal(hostnamesRaw, &cert.Hostnames); err != nil {
			return domain.CertificateStatus{}, err
		}
	}
	cert.SecretID = deref(secretID)
	cert.CertificatePEM = deref(certificate)
	cert.FailureReason = deref(failureReason)
	return cert, nil
}

func renewalDate(expiresAt time.Time) time.Time {
	return expiresAt.Add(-30 * 24 * time.Hour)
}

// IssueCertificate issues (or records the failure of) a certificate.
func (s *Store) IssueCertificate(ctx context.Context, input CertificateIssueInput, actorUserID string) (domain.CertificateStatus, error) {
	environment := input.Environment
	if environment == "" {
		if input.Issuer == "letsencrypt" {
			environment = "staging"
		} else {
			environment = "local"
		}
	}
	base := domain.CertificateStatus{
		ID:          newUUID(),
		Hostnames:   input.Hostnames,
		Issuer:      input.Issuer,
		Challenge:   input.Challenge,
		Environment: environment,
	}

	cert, err := s.issue(ctx, input, base, actorUserID)
	if err != nil {
		if input.Issuer != "letsencrypt" {
			return domain.CertificateStatus{}, err
		}
		failed := base
		failed.Status = "failed"
		failed.FailureReason = err.Error()
		return insertCertificate(ctx, s.pool, failed)
	}
	return cert, nil
}

func (s *Store) issue(ctx context.Context, input CertificateIssueInput, base domain.CertificateStatus, actorUserID string) (domain.CertificateStatus, error) {
	switch input.Issuer {
	case "self-signed":
		if input.Challenge != "none" || s.secrets == nil {
			return domain.CertificateStatus{}, errors.New("Self-signed issuance requires challenge none and a master key")
		}
		// Leaf certificates are signed by the installation's long-lived internal CA
		// so clients that trust the CA once keep trusting renewed leaves.
		material, err := s.issueInternalLeaf(ctx, input.Hostnames, 365)
		if err != nil {
			return domain.CertificateStatus{}, err
		}
		secretID, err := s.secrets.Put(ctx, "certificate-private-key", material.PrivateKeyPEM)
		if err != nil {
			return domain.CertificateStatus{}, err
		}
		cert := base
		cert.Status = "active"
		expires := material.ExpiresAt
		renew := renewalDate(material.ExpiresAt)
		cert.ExpiresAt = &expires
		cert.RenewAfter = &renew
		cert.SecretID = secretID
		cert.CertificatePEM = material.CertificatePEM
		return insertCertificate(ctx, s.pool, cert)

	case "uploaded":
		if input.Challenge != "none" || s.secrets == nil {
			return domain.CertificateStatus{}, errors.New("Uploaded certificates require challenge none and a master key")
		}
		if input.CertificatePEM == "" || input.PrivateKeyPEM == "" {
			return domain.CertificateStatus{}, errors.New("Uploaded certificate and private key are required")
		}
		material, err := acme.ValidateUploadedCertificate(input.Hostnames, input.CertificatePEM, input.PrivateKeyPEM)
		if err != nil {
			return domain.CertificateStatus{}, err
		}
		secretID, err := s.secrets.Put(ctx, "certificate-private-key", material.PrivateKeyPEM)
		if err != nil {
			return domain.CertificateStatus{}, err
		}
		cert := base
		cert.Status = "active"
		expires := material.ExpiresAt
		cert.ExpiresAt = &expires
		cert.SecretID = secretID
		cert.CertificatePEM = material.CertificatePEM
		return insertCertificate(ctx, s.pool, cert)

	default: // letsencrypt
		if input.Challenge == "none" {
			return domain.CertificateStatus{}, errors.New("Let's Encrypt requires HTTP-01 or DNS-01")
		}
		if s.secrets == nil {
			return domain.CertificateStatus{}, errors.New("Let's Encrypt issuance requires a master key")
		}
		if input.Challenge == "http-01" && actorUserID != "" {
			apply, err := s.CreateApplyJob(ctx, actorUserID)
			if err != nil {
				return domain.CertificateStatus{}, err
			}
			if err := s.waitForJob(ctx, apply.Job.ID, 60*time.Second); err != nil {
				return domain.CertificateStatus{}, err
			}
		}
		options := acme.LetsEncryptOptions{
			Hostnames:    input.Hostnames,
			DirectoryURL: input.DirectoryURL,
			Email:        input.Email,
			Challenge:    input.Challenge,
			KeyType:      input.KeyType,
		}
		if input.PropagationSeconds != nil {
			options.PropagationSeconds = *input.PropagationSeconds
		}
		if input.Challenge == "http-01" {
			options.Http01 = acme.GlobalHttp01Store()
		}
		if input.Challenge == "dns-01" {
			apiToken, zoneID, zoneName, err := s.resolveCloudflareCredentials(ctx, input.Cloudflare)
			if err != nil {
				return domain.CertificateStatus{}, err
			}
			options.Dns01 = acme.NewCloudflareDns01Adapter(acme.CloudflareOptions{APIToken: apiToken, ZoneID: zoneID, ZoneName: zoneName})
		}
		material, err := acme.IssueLetsEncrypt(ctx, options)
		if err != nil {
			return domain.CertificateStatus{}, err
		}
		secretID, err := s.secrets.Put(ctx, "certificate-private-key", material.PrivateKeyPEM)
		if err != nil {
			return domain.CertificateStatus{}, err
		}
		cert := base
		cert.Status = "active"
		expires := material.ExpiresAt
		renew := renewalDate(material.ExpiresAt)
		cert.ExpiresAt = &expires
		cert.RenewAfter = &renew
		cert.SecretID = secretID
		cert.CertificatePEM = material.CertificatePEM
		return insertCertificate(ctx, s.pool, cert)
	}
}

func insertCertificate(ctx context.Context, q querier, cert domain.CertificateStatus) (domain.CertificateStatus, error) {
	hostnamesJSON, err := json.Marshal(cert.Hostnames)
	if err != nil {
		return domain.CertificateStatus{}, err
	}
	status := cert.Status
	if status == "" {
		status = "pending"
	}
	row := q.QueryRow(ctx, `
		insert into certificates
			(id, hostnames, issuer, challenge, environment, status, expires_at, renew_after, key_secret_id, certificate_pem, failure_reason)
		values ($1, $2, $3::proxycore_certificate_issuer, $4::proxycore_certificate_challenge, $5,
			$6::proxycore_certificate_status, $7, $8, $9, $10, $11)
		returning id::text, hostnames, issuer::text, challenge::text, environment, status::text,
			expires_at, renew_after, key_secret_id::text, certificate_pem, failure_reason
	`, cert.ID, hostnamesJSON, cert.Issuer, cert.Challenge, cert.Environment, status,
		nullableTime(cert.ExpiresAt), nullableTime(cert.RenewAfter), nullableString(cert.SecretID),
		nullableString(cert.CertificatePEM), nullableString(cert.FailureReason))
	return scanCertificate(row)
}

func nullableTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return *value
}

func (s *Store) waitForJob(ctx context.Context, jobID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		job, err := s.getJob(ctx, jobID)
		if err != nil {
			return err
		}
		if job == nil {
			return fmt.Errorf("Certificate apply job disappeared: %s", jobID)
		}
		switch job.Status {
		case "applied":
			return nil
		case "failed", "rolled-back":
			message := job.Status
			if job.ErrorMessage != nil {
				message = *job.ErrorMessage
			}
			return fmt.Errorf("HTTP-01 challenge route apply failed: %s", message)
		}
		time.Sleep(250 * time.Millisecond)
	}
	return errors.New("Timed out waiting for the HTTP-01 challenge route")
}

func (s *Store) getJob(ctx context.Context, id string) (*JobRecord, error) {
	row := s.pool.QueryRow(ctx, `
		select id::text, revision_id::text, actor_user_id::text, target::text, status::text, correlation_id,
			created_at, claimed_at, started_at, finished_at, validation_output, apply_output, health_output, error_message
		from apply_jobs where id = $1
	`, id)
	job, err := scanJob(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &job, nil
}

func (s *Store) resolveCloudflareCredentials(ctx context.Context, input *CloudflareInput) (apiToken, zoneID, zoneName string, err error) {
	if s.secrets == nil {
		return "", "", "", errors.New("Cloudflare DNS-01 requires a master key")
	}
	var apiTokenInput, zoneIDInput, zoneNameInput string
	if input != nil {
		apiTokenInput = input.APIToken
		zoneIDInput = strings.TrimSpace(input.ZoneID)
		zoneNameInput = strings.ToLower(strings.TrimSpace(input.ZoneName))
	}
	if apiTokenInput != "" {
		sum := sha256.Sum256([]byte(apiTokenInput))
		tokenName := "cloudflare-" + hex.EncodeToString(sum[:])
		var existing string
		err := s.pool.QueryRow(ctx, `
			select id::text from provider_connections
			where provider = 'cloudflare' and name = $1 and scope = 'dns-01' and enabled = true
			order by created_at desc limit 1
		`, tokenName).Scan(&existing)
		if errors.Is(err, pgx.ErrNoRows) {
			secretID, putErr := s.secrets.Put(ctx, "cloudflare-api-token", apiTokenInput)
			if putErr != nil {
				return "", "", "", putErr
			}
			if _, insErr := s.pool.Exec(ctx, `
				insert into provider_connections (provider, name, secret_id, scope)
				values ('cloudflare', $1, $2, 'dns-01')
			`, tokenName, secretID); insErr != nil {
				return "", "", "", insErr
			}
		} else if err != nil {
			return "", "", "", err
		}
		return apiTokenInput, zoneIDInput, zoneNameInput, nil
	}

	var secretID, scope, name string
	err = s.pool.QueryRow(ctx, `
		select secret_id::text, scope, name from provider_connections
		where provider = 'cloudflare' and enabled = true
		order by created_at desc limit 1
	`).Scan(&secretID, &scope, &name)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", "", errors.New("Cloudflare credentials not configured for this DNS zone")
	}
	if err != nil {
		return "", "", "", err
	}
	token, err := s.secrets.Get(ctx, secretID)
	if err != nil {
		return "", "", "", err
	}
	if token == "" {
		return "", "", "", errors.New("Cloudflare credential secret is unavailable")
	}
	if scope == "dns-01" {
		return token, "", "", nil
	}
	return token, name, scope, nil
}
