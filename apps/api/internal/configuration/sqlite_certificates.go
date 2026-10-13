package configuration

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

// SQLiteCertificateStore is an unwired SQLite counterpart of the PostgreSQL
// certificate reads and delete. Reads use the shared *sql.DB; DeleteCertificate
// goes through the single writer. Certificate issuance stays outside this type:
// the sqlite* helpers below run inside a caller-owned writer transaction.
type SQLiteCertificateStore struct {
	db     *sql.DB
	writer *sqlitedb.Writer
}

func NewSQLiteCertificateStore(db *sql.DB, writer *sqlitedb.Writer) *SQLiteCertificateStore {
	return &SQLiteCertificateStore{db: db, writer: writer}
}

const sqliteCertificateColumns = `id, hostnames, issuer, challenge, environment, status,
	expires_at, renew_after, key_secret_id, certificate_pem, failure_reason`

// sqliteCertificateQuerier is satisfied by *sql.DB and by the writer's *sql.Conn.
type sqliteCertificateQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// ListCertificates returns certificates newest first. created_at is a
// fixed-width BINARY tagged clock, so string order is time order across the
// PostgreSQL 2000 epoch and Unix zero. Ties are unspecified, as in PG.
func (s *SQLiteCertificateStore) ListCertificates(ctx context.Context) ([]domain.CertificateStatus, error) {
	return sqliteQueryCertificates(ctx, s.db, `select `+sqliteCertificateColumns+` from certificates order by created_at desc`)
}

// GetCertificate canonicalizes the id once, accepting every spelling PG's uuid
// cast accepts; malformed input fails with PG's text (postgresInvalidUUIDError).
func (s *SQLiteCertificateStore) GetCertificate(ctx context.Context, id string) (domain.CertificateStatus, error) {
	canonical, err := sqliteCertificateLookupID(id)
	if err != nil {
		return domain.CertificateStatus{}, err
	}
	return sqliteGetCertificate(ctx, s.db, canonical)
}

// DeleteCertificate mirrors Store.DeleteCertificate with two deliberate
// differences. First, get, reference count and delete run in ONE writer
// transaction, so a record cannot start referencing the certificate between
// the check and the delete (PG's autocommit statements have that window).
// Second, the id is canonicalized at entry: PG compares the raw spelling with
// the stored proxy_settings text, so an alias such as an uppercase id bypassed
// the in-use check (observed on postgres:17). The stored certificateId is still
// matched as exact text, like PG. Every dns_records row counts, enabled or not.
// The key secret is NOT deleted, as in PG.
func (s *SQLiteCertificateStore) DeleteCertificate(ctx context.Context, id string) error {
	canonical, err := sqliteCertificateLookupID(id)
	if err != nil {
		return err
	}
	return s.writer.Write(ctx, func(v sqlitedb.WriteValue) error {
		if _, err := sqliteGetCertificate(v.Context(), v.Conn(), canonical); err != nil {
			return err
		}
		var refs int
		if err := v.Conn().QueryRowContext(v.Context(), `select count(*) from dns_records
			where json_extract(proxy_settings, '$.certificateId') = ?`, canonical).Scan(&refs); err != nil {
			return err
		}
		if refs > 0 {
			return ErrCertificateInUse
		}
		result, err := v.Conn().ExecContext(v.Context(), `delete from certificates where id = ?`, canonical)
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if affected == 0 {
			return ErrCertificateNotFound
		}
		return nil
	})
}

// ListCertificatesDueForRenewal mirrors renewal.go: active rows whose renew_after
// is at or before now, restricted to letsencrypt with http-01/dns-01 or
// self-signed with challenge none, oldest renew_after first. The tagged clock
// compares as text; now keeps nanoseconds (PG truncates to microseconds).
func (s *SQLiteCertificateStore) ListCertificatesDueForRenewal(ctx context.Context, now time.Time) ([]domain.CertificateStatus, error) {
	key, err := sqliteCertificateTimeKey(&now)
	if err != nil {
		return nil, err
	}
	return sqliteQueryCertificates(ctx, s.db, `select `+sqliteCertificateColumns+` from certificates
		where status = 'active'
			and renew_after is not null
			and renew_after <= ?
			and (
				(issuer = 'letsencrypt' and challenge in ('http-01', 'dns-01'))
				or (issuer = 'self-signed' and challenge = 'none')
			)
		order by renew_after asc`, *key)
}

func sqliteCertificateLookupID(id string) (string, error) {
	canonical, err := sqlitedb.ParseUUIDLookupText(id)
	if err != nil {
		return "", postgresInvalidUUIDError(id)
	}
	return canonical, nil
}

func sqliteGetCertificate(ctx context.Context, q sqliteCertificateQuerier, canonicalID string) (domain.CertificateStatus, error) {
	cert, err := scanCertificate(sqliteCertificateScanner{q.QueryRowContext(ctx,
		`select `+sqliteCertificateColumns+` from certificates where id = ?`, canonicalID)})
	if errors.Is(err, sql.ErrNoRows) {
		return domain.CertificateStatus{}, ErrCertificateNotFound
	}
	return cert, err
}

func sqliteQueryCertificates(ctx context.Context, q sqliteCertificateQuerier, query string, args ...any) ([]domain.CertificateStatus, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	certificates := []domain.CertificateStatus{}
	for rows.Next() {
		cert, err := scanCertificate(sqliteCertificateScanner{rows})
		if err != nil {
			return nil, err
		}
		certificates = append(certificates, cert)
	}
	return certificates, rows.Err()
}

// sqliteCertificateTimeKey encodes an optional instant as a tagged clock key;
// nil is SQL NULL. time.Time.Unix floors, so Nanosecond is always nonnegative.
func sqliteCertificateTimeKey(value *time.Time) (*string, error) {
	if value == nil {
		return sqlitedb.EncodeTimestamp(sqlitedb.NullTimestamp{})
	}
	return sqlitedb.EncodeTimestamp(sqlitedb.FiniteTimestamp{Seconds: value.Unix(), Nanos: int32(value.Nanosecond())})
}

// sqliteInsertCertificate mirrors insertCertificate inside a writer transaction.
// Ids (row and key secret) are canonicalized like PG's uuid cast; an empty
// status defaults to pending; empty secret, PEM and failure reason are NULL.
func sqliteInsertCertificate(v sqlitedb.WriteValue, cert domain.CertificateStatus) (domain.CertificateStatus, error) {
	id, err := sqliteCertificateLookupID(cert.ID)
	if err != nil {
		return domain.CertificateStatus{}, err
	}
	var secretID *string
	if cert.SecretID != "" {
		canonical, err := sqliteCertificateLookupID(cert.SecretID)
		if err != nil {
			return domain.CertificateStatus{}, err
		}
		secretID = &canonical
	}
	hostnames, err := json.Marshal(cert.Hostnames)
	if err != nil {
		return domain.CertificateStatus{}, err
	}
	expires, err := sqliteCertificateTimeKey(cert.ExpiresAt)
	if err != nil {
		return domain.CertificateStatus{}, err
	}
	renew, err := sqliteCertificateTimeKey(cert.RenewAfter)
	if err != nil {
		return domain.CertificateStatus{}, err
	}
	status := cert.Status
	if status == "" {
		status = "pending"
	}
	return scanCertificate(sqliteCertificateScanner{v.Conn().QueryRowContext(v.Context(), `
		insert into certificates (id, hostnames, issuer, challenge, environment, status, expires_at, renew_after,
			key_secret_id, certificate_pem, failure_reason, created_at, updated_at)
		values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		returning `+sqliteCertificateColumns,
		id, string(hostnames), cert.Issuer, cert.Challenge, cert.Environment, status, expires, renew,
		secretID, sqliteNullableString(cert.CertificatePEM), sqliteNullableString(cert.FailureReason),
		v.Timestamp(), v.Timestamp())})
}

// sqliteUpdateActiveCertificateMaterial mirrors updateActiveCertificateMaterial:
// it only touches an active certificate of the given issuer, clears the failure
// reason and moves updated_at. A miss reports PG's "no longer active" error.
func sqliteUpdateActiveCertificateMaterial(
	v sqlitedb.WriteValue,
	id, issuer, secretID, certificatePEM string,
	expiresAt, renewAfter time.Time,
) (domain.CertificateStatus, error) {
	canonical, err := sqliteCertificateLookupID(id)
	if err != nil {
		return domain.CertificateStatus{}, err
	}
	secret, err := sqliteCertificateLookupID(secretID)
	if err != nil {
		return domain.CertificateStatus{}, err
	}
	expires, err := sqliteCertificateTimeKey(&expiresAt)
	if err != nil {
		return domain.CertificateStatus{}, err
	}
	renew, err := sqliteCertificateTimeKey(&renewAfter)
	if err != nil {
		return domain.CertificateStatus{}, err
	}
	cert, err := scanCertificate(sqliteCertificateScanner{v.Conn().QueryRowContext(v.Context(), `
		update certificates
		set status = 'active', expires_at = ?, renew_after = ?, key_secret_id = ?, certificate_pem = ?,
			failure_reason = null, updated_at = ?
		where id = ? and issuer = ? and status = 'active'
		returning `+sqliteCertificateColumns,
		expires, renew, secret, certificatePEM, v.Timestamp(), canonical, issuer)})
	if errors.Is(err, sql.ErrNoRows) {
		return domain.CertificateStatus{}, fmt.Errorf("certificate %s is no longer active", id)
	}
	return cert, err
}

func sqliteNullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}
