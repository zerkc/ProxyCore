package cluster

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/secrets"
	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

// SQLiteKeyStore persists cluster KEKs in SQLite, wrapped with the installation
// master key. It is additive and inactive until a caller selects it; the caller
// owns Open, Migrate and Close of the database/writer pair.
//
// Deliberate differences from Store (PostgreSQL): there is no advisory lock or
// row lock, so first-creation concurrency is resolved by re-checking inside the
// single writer (first writer wins, others return the winner's key), and
// created_at comes from the writer clock. Active-key reads by id need no write
// because the row is immutable. All failures map to the bare
// ErrClusterKeyDenied exactly like Store's id path.
type SQLiteKeyStore struct {
	db     *sql.DB
	writer *sqlitedb.Writer
	master string
	unwrap *Store
}

func NewSQLiteKeyStore(db *sql.DB, writer *sqlitedb.Writer, masterKeyBase64 string) *SQLiteKeyStore {
	return &SQLiteKeyStore{db: db, writer: writer, master: masterKeyBase64, unwrap: NewStore(masterKeyBase64)}
}

func (s *SQLiteKeyStore) String() string { return "[cluster key store redacted]" }

func (s *SQLiteKeyStore) Format(state fmt.State, _ rune) { _, _ = io.WriteString(state, s.String()) }

// KeyCandidate is a freshly generated and wrapped KEK prepared outside a Write
// so the single-writer critical section stays short. Callers must Destroy it.
type KeyCandidate struct {
	material KeyMaterial
	wrapped  string
}

func (c *KeyCandidate) String() string { return clusterKeyRedacted }

func (c *KeyCandidate) Format(state fmt.State, _ rune) { _, _ = io.WriteString(state, c.String()) }

// ID returns the candidate key id, or uuid.Nil for a nil or destroyed candidate.
func (c *KeyCandidate) ID() uuid.UUID {
	if c == nil {
		return uuid.Nil
	}
	return c.material.ID
}

// Destroy zeroes the plaintext key. It is safe on nil and to call repeatedly.
func (c *KeyCandidate) Destroy() {
	if c == nil {
		return
	}
	c.material.Destroy()
	c.material.ID = uuid.Nil
	c.wrapped = ""
}

// NewKeyCandidate generates a KEK and its master-key envelope without touching
// the database.
func (s *SQLiteKeyStore) NewKeyCandidate() (*KeyCandidate, error) {
	if s == nil {
		return nil, ErrClusterKeyDenied
	}
	key, err := GenerateKEK()
	if err != nil {
		return nil, ErrClusterKeyDenied
	}
	defer zeroBytes(key)
	id, err := uuid.NewRandom()
	if err != nil {
		return nil, ErrClusterKeyDenied
	}
	material, err := NewKeyMaterial(id, key)
	if err != nil {
		return nil, ErrClusterKeyDenied
	}
	wrapped, err := secrets.EncryptSecret(string(material.key), s.master)
	if err != nil {
		material.Destroy()
		return nil, ErrClusterKeyDenied
	}
	return &KeyCandidate{material: material, wrapped: wrapped}, nil
}

func (s *SQLiteKeyStore) readable(ctx context.Context) bool {
	return s != nil && s.db != nil && s.unwrap != nil && ctx != nil && ctx.Err() == nil
}

type sqliteKeyQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// LoadActive returns the unretired KEK with the given id. The row is immutable
// so a plain read is enough; no Write is taken.
func (s *SQLiteKeyStore) LoadActive(ctx context.Context, id uuid.UUID) (KeyMaterial, error) {
	if !s.readable(ctx) {
		return KeyMaterial{}, ErrClusterKeyDenied
	}
	return s.loadByID(ctx, s.db, id)
}

func (s *SQLiteKeyStore) loadByID(ctx context.Context, q sqliteKeyQuerier, id uuid.UUID) (KeyMaterial, error) {
	var purpose, wrapped string
	var version int
	if err := q.QueryRowContext(ctx, `select purpose, wrapped_kek, wrapping_key_version
		from cluster_keys where id = ? and retired_at is null`, id.String()).Scan(&purpose, &wrapped, &version); err != nil {
		return KeyMaterial{}, ErrClusterKeyDenied
	}
	return s.unwrap.decrypt(id, purpose, wrapped, version)
}

// loadOldest reads the oldest unretired KEK. found=false means no active row.
func (s *SQLiteKeyStore) loadOldest(ctx context.Context, q sqliteKeyQuerier) (material KeyMaterial, found bool, err error) {
	var idText, purpose, wrapped string
	var version int
	err = q.QueryRowContext(ctx, `select id, purpose, wrapped_kek, wrapping_key_version
		from cluster_keys where retired_at is null
		order by created_at asc, id asc limit 1`).Scan(&idText, &purpose, &wrapped, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return KeyMaterial{}, false, nil
	}
	if err != nil {
		return KeyMaterial{}, false, ErrClusterKeyDenied
	}
	id, err := uuid.Parse(idText)
	if err != nil {
		return KeyMaterial{}, true, ErrClusterKeyDenied
	}
	material, err = s.unwrap.decrypt(id, purpose, wrapped, version)
	return material, true, err
}

// LoadOrCreate returns the KEK for activeID, or with nil the oldest unretired
// KEK, creating one when none exists. Creation generates and wraps the key
// before the single Write, re-checks inside it, and returns the winner's key,
// so concurrent creators converge on exactly one active KEK.
func (s *SQLiteKeyStore) LoadOrCreate(ctx context.Context, activeID *uuid.UUID) (KeyMaterial, error) {
	if !s.readable(ctx) || s.writer == nil {
		return KeyMaterial{}, ErrClusterKeyDenied
	}
	if activeID != nil {
		return s.loadByID(ctx, s.db, *activeID)
	}
	material, found, err := s.loadOldest(ctx, s.db)
	if found || err != nil {
		return material, err
	}
	candidate, err := s.NewKeyCandidate()
	if err != nil {
		return KeyMaterial{}, err
	}
	defer candidate.Destroy()
	var result KeyMaterial
	if err := s.writer.Write(ctx, func(v sqlitedb.WriteValue) error {
		var err error
		result, err = s.LoadOrCreateInWrite(v, nil, candidate)
		return err
	}); err != nil {
		result.Destroy()
		return KeyMaterial{}, ErrClusterKeyDenied
	}
	return result, nil
}

// LoadOrCreateInWrite is LoadOrCreate for callers already inside a Write: the
// KEK row commits or rolls back with the caller's other writes. A nil candidate
// is generated inline; an unused candidate is left for the caller to Destroy.
func (s *SQLiteKeyStore) LoadOrCreateInWrite(v sqlitedb.WriteValue, activeID *uuid.UUID, candidate *KeyCandidate) (KeyMaterial, error) {
	if v.Conn() == nil || !s.readable(v.Context()) {
		return KeyMaterial{}, ErrClusterKeyDenied
	}
	if activeID != nil {
		return s.loadByID(v.Context(), v.Conn(), *activeID)
	}
	material, found, err := s.loadOldest(v.Context(), v.Conn())
	if found || err != nil {
		return material, err
	}
	if candidate == nil {
		if candidate, err = s.NewKeyCandidate(); err != nil {
			return KeyMaterial{}, err
		}
		defer candidate.Destroy()
	}
	if candidate.ID() == uuid.Nil || candidate.wrapped == "" {
		return KeyMaterial{}, ErrClusterKeyDenied
	}
	if _, err := v.Conn().ExecContext(v.Context(), `insert into cluster_keys
		(id, purpose, wrapped_kek, wrapping_key_version, created_at) values (?, ?, ?, ?, ?)`,
		candidate.material.ID.String(), ClusterKeyPurpose, candidate.wrapped, ClusterKeyWrappingVersion, v.Timestamp()); err != nil {
		return KeyMaterial{}, ErrClusterKeyDenied
	}
	key := candidate.material.CopyBytes()
	defer zeroBytes(key)
	material, err = NewKeyMaterial(candidate.material.ID, key)
	if err != nil {
		return KeyMaterial{}, ErrClusterKeyDenied
	}
	return material, nil
}
