package secrets

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store persists encrypted secrets.
type Store interface {
	Put(ctx context.Context, purpose, plaintext string) (string, error)
	Get(ctx context.Context, id string) (string, error)
}

// PgStore is a Postgres-backed secret store.
type PgStore struct {
	pool            *pgxpool.Pool
	masterKeyBase64 string
}

// NewPgStore builds a Postgres secret store.
func NewPgStore(pool *pgxpool.Pool, masterKeyBase64 string) *PgStore {
	return &PgStore{pool: pool, masterKeyBase64: masterKeyBase64}
}

// Put encrypts and stores a secret, returning its id.
func (s *PgStore) Put(ctx context.Context, purpose, plaintext string) (string, error) {
	ciphertext, err := EncryptSecret(plaintext, s.masterKeyBase64)
	if err != nil {
		return "", err
	}
	var id string
	err = s.pool.QueryRow(ctx,
		`insert into secrets (purpose, ciphertext) values ($1, $2) returning id::text`,
		purpose, ciphertext,
	).Scan(&id)
	if err != nil {
		return "", err
	}
	return id, nil
}

// Get returns the decrypted secret, or "" if it does not exist.
func (s *PgStore) Get(ctx context.Context, id string) (string, error) {
	var ciphertext string
	err := s.pool.QueryRow(ctx, `select ciphertext from secrets where id = $1`, id).Scan(&ciphertext)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return DecryptSecret(ciphertext, s.masterKeyBase64)
}
