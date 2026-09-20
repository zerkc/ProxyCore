package dbexport

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func testDatabaseURL() (string, bool) {
	if url := os.Getenv("PGX_TEST_DATABASE_URL"); url != "" {
		return url, true
	}
	if url := os.Getenv("DATABASE_URL"); url != "" {
		return url, true
	}
	return "", false
}

// NewTestPoolFromEnv returns a *pgxpool.Pool based on PGX_TEST_DATABASE_URL
// (preferred) or DATABASE_URL. It forces sslmode=disable and clears any
// inherited TLS configuration so local disposable Postgres containers work
// out of the box. The pool uses at most 8 connections. The caller MUST
// call pool.Close when done.
func NewTestPoolFromEnv(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url, ok := testDatabaseURL()
	if !ok {
		t.Skip("no test database URL is set")
	}

	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse test database URL: %v", err)
	}
	config.ConnConfig.TLSConfig = nil
	config.ConnConfig.Fallbacks = nil
	config.MaxConns = 8

	pool, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatalf("create test pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}
