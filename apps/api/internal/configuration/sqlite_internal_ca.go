package configuration

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/zerkc/ProxyCore/apps/api/internal/acme"
	"github.com/zerkc/ProxyCore/apps/api/internal/secrets"
	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

// sqliteInternalCASecrets is what the CA needs from secrets.SQLiteStore: the
// in-transaction pair of sqliteSecretWriter plus Get for reads outside a Write.
// Never call Put/Get-with-own-Write from inside Writer.Write (nested write).
type sqliteInternalCASecrets interface {
	sqliteSecretWriter
	Get(ctx context.Context, id string) (string, error)
}

var _ sqliteInternalCASecrets = (*secrets.SQLiteStore)(nil)

// SQLiteInternalCAStore is the unwired SQLite counterpart of the internal-CA
// pieces used by self-signed issuance and enrollment TLS. Enrollment TLS uses
// the same CA and encrypted secret store; its single-writer transaction replaces
// PostgreSQL advisory locks. Cluster identity remains a separate store.
// It is a sibling of SQLiteCertificateStore (same read *sql.DB / write
// *sqlitedb.Writer split) so both can share one db and writer.
type SQLiteInternalCAStore struct {
	db      *sql.DB
	writer  *sqlitedb.Writer
	secrets sqliteInternalCASecrets
}

// NewSQLiteInternalCAStore is cheap and nil-safe. A nil secret store (including
// a typed-nil *secrets.SQLiteStore) behaves like PG's nil secrets.Store.
func NewSQLiteInternalCAStore(db *sql.DB, writer *sqlitedb.Writer, secretStore sqliteInternalCASecrets) *SQLiteInternalCAStore {
	if typed, ok := secretStore.(*secrets.SQLiteStore); ok && typed == nil {
		secretStore = nil
	}
	return &SQLiteInternalCAStore{db: db, writer: writer, secrets: secretStore}
}

// EnsureInternalCACertificatePEM returns the internal CA certificate, creating
// the CA on first use.
func (s *SQLiteInternalCAStore) EnsureInternalCACertificatePEM(ctx context.Context) (string, error) {
	certPEM, _, err := s.ensureInternalCA(ctx)
	return certPEM, err
}

// ensureInternalCA mirrors Store.ensureInternalCA. Intentional difference from
// PG: key generation stays outside the writer transaction, but one Write then
// re-checks for a CA (first writer wins), stores the key secret and the
// internal_ca row atomically, and returns the winner. Concurrent first-time
// calls therefore converge on one CA with no orphan secrets (PG leaves one
// orphan key secret per losing caller, observed on postgres:17).
func (s *SQLiteInternalCAStore) ensureInternalCA(ctx context.Context) (certPEM, keyPEM string, err error) {
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
	err = s.writer.Write(ctx, func(v sqlitedb.WriteValue) error {
		existingCert, existingKey, err := sqliteLoadInternalCA(v.Context(), v.Conn(), func(id string) (string, error) {
			return s.secrets.GetInWrite(v, id)
		})
		if err != nil {
			return err
		}
		if existingCert != "" && existingKey != "" {
			certPEM, keyPEM = existingCert, existingKey
			return nil
		}
		secretID, err := s.secrets.PutInWrite(v, "internal-ca-private-key", material.PrivateKeyPEM)
		if err != nil {
			return err
		}
		if _, err := v.Conn().ExecContext(v.Context(),
			`insert into internal_ca (id, certificate_pem, key_secret_id, created_at, updated_at) values (?, ?, ?, ?, ?)`,
			internalCAID, material.CertificatePEM, secretID, v.Timestamp(), v.Timestamp()); err != nil {
			return fmt.Errorf("persist internal CA: %w", err)
		}
		certPEM, keyPEM = material.CertificatePEM, material.PrivateKeyPEM
		return nil
	})
	if err != nil {
		return "", "", err
	}
	return certPEM, keyPEM, nil
}

// loadInternalCA reads the CA on the shared read pool; it never opens a Write.
func (s *SQLiteInternalCAStore) loadInternalCA(ctx context.Context) (certPEM, keyPEM string, err error) {
	return sqliteLoadInternalCA(ctx, s.db, func(id string) (string, error) { return s.secrets.Get(ctx, id) })
}

// sqliteLoadInternalCA mirrors Store.loadInternalCA: no row is ("", "", nil);
// incomplete or invalid material is an error with PG's text.
func sqliteLoadInternalCA(ctx context.Context, q sqliteCertificateQuerier, getKey func(id string) (string, error)) (certPEM, keyPEM string, err error) {
	var secretID string
	err = q.QueryRowContext(ctx, `select certificate_pem, key_secret_id from internal_ca where id = ?`, internalCAID).Scan(&certPEM, &secretID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	if err != nil {
		return "", "", err
	}
	if strings.TrimSpace(certPEM) == "" || strings.TrimSpace(secretID) == "" {
		return "", "", errors.New("established internal CA material is incomplete")
	}
	keyPEM, err = getKey(secretID)
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

// issueInternalLeaf mirrors Store.issueInternalLeaf: ensure the CA, then sign.
// It stores nothing; callers persist the leaf key and certificate row.
func (s *SQLiteInternalCAStore) issueInternalLeaf(ctx context.Context, hostnames []string, validityDays int) (acme.Material, error) {
	caCertPEM, caKeyPEM, err := s.ensureInternalCA(ctx)
	if err != nil {
		return acme.Material{}, err
	}
	return acme.IssueSignedByCA(hostnames, validityDays, caCertPEM, caKeyPEM)
}
