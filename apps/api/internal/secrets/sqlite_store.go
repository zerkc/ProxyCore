package secrets

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

var _ Store = (*SQLiteStore)(nil)

// SQLiteStore is a SQLite-backed secret store. The database must be opened by
// sqlitedb.Open and migrated; it uses the same crypto as PgStore.
type SQLiteStore struct {
	db              *sql.DB
	writer          *sqlitedb.Writer
	masterKeyBase64 string
}

// NewSQLiteStore builds a SQLite secret store.
func NewSQLiteStore(db *sql.DB, masterKeyBase64 string) *SQLiteStore {
	s := &SQLiteStore{db: db, masterKeyBase64: masterKeyBase64}
	if db != nil {
		s.writer = sqlitedb.NewWriter(db, nil)
	}
	return s
}

func (s *SQLiteStore) ready() error {
	if s == nil || s.db == nil || s.writer == nil {
		return errors.New("sqlite secret store is not configured")
	}
	return nil
}

// Put encrypts and stores a secret, returning its lowercase UUID id.
func (s *SQLiteStore) Put(ctx context.Context, purpose, plaintext string) (string, error) {
	if err := s.ready(); err != nil {
		return "", err
	}
	ciphertext, err := EncryptSecret(plaintext, s.masterKeyBase64)
	if err != nil {
		return "", err
	}
	var id string
	err = s.writer.Write(ctx, func(v sqlitedb.WriteValue) (err error) {
		id, err = insertSecret(v, purpose, ciphertext)
		return err
	})
	if err != nil {
		return "", err
	}
	return id, nil
}

// PutInWrite is Put inside a caller-owned writer transaction: it opens none, so
// the secret commits or rolls back with the surrounding Write. It uses the
// writer value's UUID and clock, and never retains the connection.
func (s *SQLiteStore) PutInWrite(v sqlitedb.WriteValue, purpose, plaintext string) (string, error) {
	if err := s.ready(); err != nil {
		return "", err
	}
	ciphertext, err := EncryptSecret(plaintext, s.masterKeyBase64)
	if err != nil {
		return "", err
	}
	return insertSecret(v, purpose, ciphertext)
}

func insertSecret(v sqlitedb.WriteValue, purpose, ciphertext string) (string, error) {
	id, err := v.NewUUID()
	if err != nil {
		return "", err
	}
	if _, err := v.Conn().ExecContext(v.Context(),
		`insert into secrets (id, purpose, ciphertext, created_at, updated_at) values (?,?,?,?,?)`,
		id, purpose, ciphertext, v.Timestamp(), v.Timestamp()); err != nil {
		return "", err
	}
	return id, nil
}

// Get returns the decrypted secret, or "" if it does not exist. Like PG's uuid
// column, it rejects malformed ids with an error and accepts uppercase hex.
func (s *SQLiteStore) Get(ctx context.Context, id string) (string, error) {
	if err := s.ready(); err != nil {
		return "", err
	}
	return s.get(ctx, s.db, id)
}

// GetInWrite is Get on the writer's connection, so it also sees secrets that
// the surrounding Write has not committed yet.
func (s *SQLiteStore) GetInWrite(v sqlitedb.WriteValue, id string) (string, error) {
	if err := s.ready(); err != nil {
		return "", err
	}
	return s.get(v.Context(), v.Conn(), id)
}

type secretQueryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func (s *SQLiteStore) get(ctx context.Context, q secretQueryer, id string) (string, error) {
	key, err := canonicalUUID(id)
	if err != nil {
		return "", err
	}
	var ciphertext string
	err = q.QueryRowContext(ctx, `select ciphertext from secrets where id = ?`, key).Scan(&ciphertext)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return DecryptSecret(ciphertext, s.masterKeyBase64)
}

func canonicalUUID(id string) (string, error) {
	invalid := fmt.Errorf("invalid input syntax for type uuid: %q", id)
	if len(id) != 36 {
		return "", invalid
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case i == 8 || i == 13 || i == 18 || i == 23:
			if c != '-' {
				return "", invalid
			}
		case !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'):
			return "", invalid
		}
	}
	return strings.ToLower(id), nil
}
