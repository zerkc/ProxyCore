package cluster

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/zerkc/ProxyCore/apps/api/internal/secrets"
)

const (
	ClusterKeyPurpose         = "cluster-kek"
	ClusterKeyWrappingVersion = 1
	clusterKeyAdvisoryLock    = int64(1_872_642)
)

var (
	ErrClusterKeyDenied = errors.New("cluster key denied")
	clusterKeyRedacted  = "[cluster key material redacted]"
)

// SQLTx is the minimal transaction surface needed to load or create a
// cluster-KEK row. The caller owns the transaction and its commit/rollback.
type SQLTx interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// KeyStore is the secure cluster-KEK persistence boundary. Implementations
// return plaintext only to the bounded in-memory caller; the database receives
// only a master-key-encrypted envelope.
type KeyStore interface {
	LoadOrCreate(ctx context.Context, tx SQLTx, activeID *uuid.UUID) (KeyMaterial, error)
}

// KeyMaterial owns one plaintext cluster KEK and must be destroyed by its
// caller as soon as the bootstrap envelope has been sealed.
type KeyMaterial struct {
	ID  uuid.UUID
	key []byte
}

// NewKeyMaterial validates and copies a key for tests and secure adapters.
func NewKeyMaterial(id uuid.UUID, key []byte) (KeyMaterial, error) {
	if id == uuid.Nil || len(key) != KEKLength {
		return KeyMaterial{}, ErrClusterKeyDenied
	}
	return KeyMaterial{ID: id, key: append([]byte(nil), key...)}, nil
}

// CopyBytes returns one bounded copy of the plaintext key.
func (m KeyMaterial) CopyBytes() []byte {
	if len(m.key) != KEKLength {
		return nil
	}
	return append([]byte(nil), m.key...)
}

// Destroy zeroes the owned key bytes. It is safe to call repeatedly.
func (m *KeyMaterial) Destroy() {
	if m == nil {
		return
	}
	for i := range m.key {
		m.key[i] = 0
	}
	m.key = nil
}

func (m KeyMaterial) String() string { return clusterKeyRedacted }

func (m KeyMaterial) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, clusterKeyRedacted)
}

// Store persists cluster KEKs encrypted with the installation master key.
type Store struct {
	masterKeyBase64 string
}

func NewStore(masterKeyBase64 string) *Store {
	return &Store{masterKeyBase64: masterKeyBase64}
}

func (s *Store) String() string { return "[cluster key store redacted]" }

func (s *Store) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, s.String())
}

func (s *Store) LoadOrCreate(ctx context.Context, tx SQLTx, activeID *uuid.UUID) (KeyMaterial, error) {
	if s == nil || tx == nil || ctx == nil {
		return KeyMaterial{}, ErrClusterKeyDenied
	}
	// The installation identity row normally serializes callers. This
	// transaction-scoped lock also makes the boundary safe when used directly
	// by recovery or a fresh process before that row is available.
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock($1)`, clusterKeyAdvisoryLock); err != nil {
		return KeyMaterial{}, ErrClusterKeyDenied
	}
	if activeID != nil {
		return s.load(ctx, tx, *activeID)
	}

	var id uuid.UUID
	var purpose, wrapped string
	var wrappingVersion int
	err := tx.QueryRow(ctx, `
		select id, purpose, wrapped_kek, wrapping_key_version
		from cluster_keys
		where retired_at is null
		order by created_at asc, id asc
		limit 1
		for update
	`).Scan(&id, &purpose, &wrapped, &wrappingVersion)
	if err == nil {
		return s.decrypt(id, purpose, wrapped, wrappingVersion)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return KeyMaterial{}, ErrClusterKeyDenied
	}

	key, err := GenerateKEK()
	if err != nil {
		return KeyMaterial{}, ErrClusterKeyDenied
	}
	id, err = uuid.NewRandom()
	if err != nil {
		zeroBytes(key)
		return KeyMaterial{}, ErrClusterKeyDenied
	}
	material, err := NewKeyMaterial(id, key)
	zeroBytes(key)
	if err != nil {
		return KeyMaterial{}, ErrClusterKeyDenied
	}
	// EncryptSecret also accepts only a string; this is the unavoidable bounded
	// conversion required by the existing secret-store API. The material is
	// destroyed on every error and never leaves this transaction boundary.
	wrapped, err = secrets.EncryptSecret(string(material.key), s.masterKeyBase64)
	if err != nil {
		material.Destroy()
		return KeyMaterial{}, ErrClusterKeyDenied
	}
	if _, err := tx.Exec(ctx, `
		insert into cluster_keys (id, purpose, wrapped_kek, wrapping_key_version)
		values ($1, $2, $3, $4)
	`, material.ID, ClusterKeyPurpose, wrapped, ClusterKeyWrappingVersion); err != nil {
		material.Destroy()
		return KeyMaterial{}, ErrClusterKeyDenied
	}
	return material, nil
}

func (s *Store) load(ctx context.Context, tx SQLTx, id uuid.UUID) (KeyMaterial, error) {
	var purpose, wrapped string
	var wrappingVersion int
	if err := tx.QueryRow(ctx, `
		select purpose, wrapped_kek, wrapping_key_version
		from cluster_keys
		where id = $1 and retired_at is null
		for update
	`, id).Scan(&purpose, &wrapped, &wrappingVersion); err != nil {
		return KeyMaterial{}, ErrClusterKeyDenied
	}
	return s.decrypt(id, purpose, wrapped, wrappingVersion)
}

func (s *Store) decrypt(id uuid.UUID, purpose, wrapped string, wrappingVersion int) (KeyMaterial, error) {
	if s == nil || purpose != ClusterKeyPurpose || wrappingVersion != ClusterKeyWrappingVersion || id == uuid.Nil {
		return KeyMaterial{}, ErrClusterKeyDenied
	}
	// The existing secret-store API returns plaintext as a string. Copy it once
	// into a byte buffer, then zero that buffer; Go cannot zero the immutable
	// string returned by that API, so the residual string lifetime is bounded
	// to this function and is an explicit boundary limitation.
	plaintext, err := secrets.DecryptSecret(wrapped, s.masterKeyBase64)
	if err != nil {
		return KeyMaterial{}, ErrClusterKeyDenied
	}
	key := []byte(plaintext)
	material, err := NewKeyMaterial(id, key)
	zeroBytes(key)
	if err != nil {
		return KeyMaterial{}, ErrClusterKeyDenied
	}
	return material, nil
}

func zeroBytes(value []byte) {
	for i := range value {
		value[i] = 0
	}
}

var _ KeyStore = (*Store)(nil)
