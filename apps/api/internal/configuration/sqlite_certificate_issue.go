package configuration

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zerkc/ProxyCore/apps/api/internal/acme"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

// ErrSQLiteLetsEncryptNotImplemented is returned for every issuer other than
// self-signed and uploaded. Let's Encrypt (and its failed-row behavior) is T56d,
// which replaces this rejection; until then nothing is written for it.
var ErrSQLiteLetsEncryptNotImplemented = errors.New("Let's Encrypt issuance is not implemented for SQLite yet")

const sqliteCertificateKeyPurpose = "certificate-private-key"

// SQLiteCertificateIssueStore is the unwired SQLite counterpart of the network
// free certificate issuance and renewal paths. It composes (and promotes the
// methods of) SQLiteCertificateStore and SQLiteInternalCAStore, so one value
// carries reads, delete, due-list, EnsureInternalCACertificatePEM, issuance and
// manual renewal. Share one db and one writer with the sibling stores.
type SQLiteCertificateIssueStore struct {
	*SQLiteCertificateStore
	*SQLiteInternalCAStore
	db      *sql.DB
	writer  *sqlitedb.Writer
	secrets sqliteInternalCASecrets
}

// NewSQLiteCertificateIssueStore is cheap and nil-safe: a nil (or typed-nil
// *secrets.SQLiteStore) secret store behaves like PG's nil secrets.Store.
func NewSQLiteCertificateIssueStore(db *sql.DB, writer *sqlitedb.Writer, secretStore sqliteInternalCASecrets) *SQLiteCertificateIssueStore {
	ca := NewSQLiteInternalCAStore(db, writer, secretStore)
	return &SQLiteCertificateIssueStore{
		SQLiteCertificateStore: NewSQLiteCertificateStore(db, writer),
		SQLiteInternalCAStore:  ca,
		db:                     db,
		writer:                 writer,
		secrets:                ca.secrets, // already normalized for typed nil
	}
}

// IssueCertificate mirrors Store.IssueCertificate for issuer self-signed and
// uploaded. Both return the error and write nothing on failure (PG only records
// a failed row for letsencrypt). actorUserID is unused until 56d (http-01).
//
// Intentional difference from PG: key generation, PEM validation and the CA
// ensure happen first, outside the writer, then ONE Write inserts the key secret
// and the certificate row, so they commit or roll back together. PG commits the
// secret first and leaves an orphan when the row insert fails.
func (s *SQLiteCertificateIssueStore) IssueCertificate(ctx context.Context, input CertificateIssueInput, actorUserID string) (domain.CertificateStatus, error) {
	environment := input.Environment
	if environment == "" {
		if input.Issuer == "letsencrypt" {
			environment = "staging"
		} else {
			environment = "local"
		}
	}
	base := domain.CertificateStatus{
		Hostnames:   input.Hostnames,
		Issuer:      input.Issuer,
		Challenge:   input.Challenge,
		Environment: environment,
		Status:      "active",
	}

	switch input.Issuer {
	case "self-signed":
		if input.Challenge != "none" || s.secrets == nil {
			return domain.CertificateStatus{}, errors.New("Self-signed issuance requires challenge none and a master key")
		}
		material, err := s.issueInternalLeaf(ctx, input.Hostnames, 365)
		if err != nil {
			return domain.CertificateStatus{}, err
		}
		expires := material.ExpiresAt
		renew := renewalDate(material.ExpiresAt)
		base.ExpiresAt, base.RenewAfter = &expires, &renew
		return s.insertIssued(ctx, base, material)

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
		expires := material.ExpiresAt
		base.ExpiresAt = &expires
		return s.insertIssued(ctx, base, material)

	default:
		return domain.CertificateStatus{}, ErrSQLiteLetsEncryptNotImplemented
	}
}

// insertIssued stores the key secret and the certificate row in one Write.
func (s *SQLiteCertificateIssueStore) insertIssued(ctx context.Context, base domain.CertificateStatus, material acme.Material) (domain.CertificateStatus, error) {
	var stored domain.CertificateStatus
	err := s.writer.Write(ctx, func(v sqlitedb.WriteValue) error {
		secretID, err := s.secrets.PutInWrite(v, sqliteCertificateKeyPurpose, material.PrivateKeyPEM)
		if err != nil {
			return err
		}
		id, err := v.NewUUID()
		if err != nil {
			return err
		}
		cert := base
		cert.ID, cert.SecretID, cert.CertificatePEM = id, secretID, material.CertificatePEM
		stored, err = sqliteInsertCertificate(v, cert)
		return err
	})
	if err != nil {
		return domain.CertificateStatus{}, err
	}
	return stored, nil
}

// RenewSelfSignedCertificateByID mirrors Store.RenewSelfSignedCertificateByID:
// the id is canonicalized once (GetCertificate), unknown ids are
// ErrCertificateNotFound, malformed ids fail with PG's uuid text.
func (s *SQLiteCertificateIssueStore) RenewSelfSignedCertificateByID(ctx context.Context, id string) (RenewResult, error) {
	cert, err := s.GetCertificate(ctx, id)
	if err != nil {
		return RenewResult{}, err
	}
	return s.RenewSelfSignedCertificate(ctx, cert)
}

// RenewSelfSignedCertificate mirrors Store.RenewSelfSignedCertificate: the
// building block T56d's RenewDueCertificates calls for due self-signed rows.
func (s *SQLiteCertificateIssueStore) RenewSelfSignedCertificate(ctx context.Context, cert domain.CertificateStatus) (RenewResult, error) {
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

// persistRenewedCertificate mirrors Store.persistRenewedCertificate and is what
// T56d reuses for Let's Encrypt renewals. The new key secret and the material
// update commit in ONE Write (PG leaves an orphan secret when the update misses);
// the previous key secret is left in place, as in PG. The owner lookup and the
// apply-job enqueue are separate and best effort: a failure there returns the
// already committed renewal together with the error, never rolling it back.
func (s *SQLiteCertificateIssueStore) persistRenewedCertificate(ctx context.Context, cert domain.CertificateStatus, material acme.Material) (RenewResult, error) {
	expires := material.ExpiresAt
	renew := renewalDate(material.ExpiresAt)
	var updated domain.CertificateStatus
	secretFailed := false
	err := s.writer.Write(ctx, func(v sqlitedb.WriteValue) error {
		secretID, err := s.secrets.PutInWrite(v, sqliteCertificateKeyPurpose, material.PrivateKeyPEM)
		if err != nil {
			secretFailed = true
			return err
		}
		updated, err = sqliteUpdateActiveCertificateMaterial(v, cert.ID, cert.Issuer, secretID, material.CertificatePEM, expires, renew)
		return err
	})
	if err != nil {
		if secretFailed {
			_ = s.markRenewalFailure(ctx, cert.ID, err.Error())
		}
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
	apply, err := sqliteCreateApplyJob(ctx, s.writer, ownerID)
	if err != nil {
		return result, fmt.Errorf("certificate renewed but apply enqueue failed: %w", err)
	}
	result.Applied = true
	result.JobID = apply.Job.ID
	return result, nil
}

// markRenewalFailure mirrors Store.markRenewalFailure: only an active row is marked.
func (s *SQLiteCertificateIssueStore) markRenewalFailure(ctx context.Context, id, reason string) error {
	canonical, err := sqliteCertificateLookupID(id)
	if err != nil {
		return err
	}
	return s.writer.Write(ctx, func(v sqlitedb.WriteValue) error {
		_, err := v.Conn().ExecContext(v.Context(),
			`update certificates set failure_reason = ?, updated_at = ? where id = ? and status = 'active'`,
			reason, v.Timestamp(), canonical)
		return err
	})
}

// firstOwnerID mirrors Store.firstOwnerID: the oldest active owner, "" if none.
// Equal created_at is unspecified, as in PG.
func (s *SQLiteCertificateIssueStore) firstOwnerID(ctx context.Context) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `select id from users where role = 'owner' and active = 1 order by created_at asc limit 1`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// sqliteCreateApplyJob is the standalone counterpart of Store.CreateApplyJob:
// one Write around sqliteCreateApplyJobInTransaction. T56d reuses it for the
// http-01 challenge route. Never call it from inside Writer.Write.
func sqliteCreateApplyJob(ctx context.Context, writer *sqlitedb.Writer, actorUserID string) (ApplyResult, error) {
	var result ApplyResult
	err := writer.Write(ctx, func(v sqlitedb.WriteValue) (err error) {
		result, err = sqliteCreateApplyJobInTransaction(v, actorUserID)
		return err
	})
	if err != nil {
		return ApplyResult{}, err
	}
	return result, nil
}
