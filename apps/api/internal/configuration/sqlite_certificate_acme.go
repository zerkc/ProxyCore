package configuration

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/acme"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

const sqliteCloudflareTokenPurpose = "cloudflare-api-token"

// SQLite counterpart of the Let's Encrypt half of Store.issue and of
// Store.IssueCertificate's failed-row path. The network (ACME, apply-job polling)
// and key generation never run inside a sqlitedb.Writer.Write: only the final
// persist (secret + row) and the credential persist (secret + connection) do,
// each as ONE Write.

func (s *SQLiteCertificateIssueStore) httpChallengeTimeout() time.Duration {
	if s.jobTimeout > 0 {
		return s.jobTimeout
	}
	return 60 * time.Second
}

func (s *SQLiteCertificateIssueStore) httpChallengePoll() time.Duration {
	if s.jobPoll > 0 {
		return s.jobPoll
	}
	return 250 * time.Millisecond
}

func (s *SQLiteCertificateIssueStore) acmeIssue(ctx context.Context, options acme.LetsEncryptOptions) (acme.Material, error) {
	if s.issueACME != nil {
		return s.issueACME(ctx, options)
	}
	return acme.IssueLetsEncrypt(ctx, options)
}

// issueLetsEncrypt mirrors the default branch of Store.issue. Every error is
// returned to IssueCertificate, which decides whether to record a failed row.
func (s *SQLiteCertificateIssueStore) issueLetsEncrypt(ctx context.Context, input CertificateIssueInput, base domain.CertificateStatus, actorUserID string) (domain.CertificateStatus, error) {
	if input.Challenge == "none" {
		return domain.CertificateStatus{}, errors.New("Let's Encrypt requires HTTP-01 or DNS-01")
	}
	if s.secrets == nil {
		return domain.CertificateStatus{}, errors.New("Let's Encrypt issuance requires a master key")
	}
	if input.Challenge == "http-01" && actorUserID != "" {
		apply, err := sqliteCreateApplyJob(ctx, s.writer, actorUserID)
		if err != nil {
			return domain.CertificateStatus{}, err
		}
		if err := sqliteWaitForJob(ctx, s.db, apply.Job.ID, s.httpChallengeTimeout(), s.httpChallengePoll()); err != nil {
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
	material, err := s.acmeIssue(ctx, options)
	if err != nil {
		return domain.CertificateStatus{}, err
	}
	expires := material.ExpiresAt
	renew := renewalDate(material.ExpiresAt)
	base.ExpiresAt, base.RenewAfter = &expires, &renew
	return s.insertIssued(ctx, base, material)
}

// insertFailedLetsEncrypt mirrors PG's failed-row insert: status failed, the
// failure reason, no key, PEM or dates. As in PG the failed row is returned with
// a nil error, and an insert failure is returned instead of the original error.
func (s *SQLiteCertificateIssueStore) insertFailedLetsEncrypt(ctx context.Context, base domain.CertificateStatus, cause error) (domain.CertificateStatus, error) {
	var stored domain.CertificateStatus
	err := s.writer.Write(ctx, func(v sqlitedb.WriteValue) error {
		id, err := v.NewUUID()
		if err != nil {
			return err
		}
		failed := base
		failed.ID, failed.Status, failed.FailureReason = id, "failed", cause.Error()
		failed.ExpiresAt, failed.RenewAfter = nil, nil
		stored, err = sqliteInsertCertificate(v, failed)
		return err
	})
	if err != nil {
		return domain.CertificateStatus{}, err
	}
	return stored, nil
}

const sqliteCloudflareConnectionLookup = `select id from provider_connections
	where provider = 'cloudflare' and name = ? and scope = 'dns-01' and enabled = 1
	order by created_at desc, rowid desc limit 1`

// resolveCloudflareCredentials mirrors Store.resolveCloudflareCredentials,
// including the zone fallbacks for connections whose scope is not dns-01.
// Intentional differences from PG: the token's secret and connection are created
// in ONE Write that re-checks for a connection first, so concurrent calls
// converge on one connection and a failed insert leaves no orphan secret; ties
// on created_at break by insertion order.
func (s *SQLiteCertificateIssueStore) resolveCloudflareCredentials(ctx context.Context, input *CloudflareInput) (apiToken, zoneID, zoneName string, err error) {
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
		name := "cloudflare-" + hex.EncodeToString(sum[:])
		var existing string
		err := s.db.QueryRowContext(ctx, sqliteCloudflareConnectionLookup, name).Scan(&existing)
		if errors.Is(err, sql.ErrNoRows) {
			if s.afterCredentialMiss != nil {
				s.afterCredentialMiss()
			}
			err = s.createCloudflareConnection(ctx, name, apiTokenInput)
		}
		if err != nil {
			return "", "", "", err
		}
		return apiTokenInput, zoneIDInput, zoneNameInput, nil
	}

	var secretID, scope, name string
	err = s.db.QueryRowContext(ctx, `select secret_id, scope, name from provider_connections
		where provider = 'cloudflare' and enabled = 1
		order by created_at desc, rowid desc limit 1`).Scan(&secretID, &scope, &name)
	if errors.Is(err, sql.ErrNoRows) {
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

func (s *SQLiteCertificateIssueStore) createCloudflareConnection(ctx context.Context, name, token string) error {
	return s.writer.Write(ctx, func(v sqlitedb.WriteValue) error {
		var existing string
		err := v.Conn().QueryRowContext(v.Context(), sqliteCloudflareConnectionLookup, name).Scan(&existing)
		if err == nil {
			return nil // a concurrent caller created it first
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		secretID, err := s.secrets.PutInWrite(v, sqliteCloudflareTokenPurpose, token)
		if err != nil {
			return err
		}
		id, err := v.NewUUID()
		if err != nil {
			return err
		}
		_, err = v.Conn().ExecContext(v.Context(), `insert into provider_connections
			(id, provider, name, secret_id, scope, enabled, created_at, updated_at)
			values (?, 'cloudflare', ?, ?, 'dns-01', 1, ?, ?)`, id, name, secretID, v.Timestamp(), v.Timestamp())
		return err
	})
}
