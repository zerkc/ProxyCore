package dbexport

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
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
// (preferred) or DATABASE_URL. It rewrites the URL to strip any inherited
// libpq sslmode-related options and force sslmode=disable, then clears
// TLSConfig and Fallbacks on the parsed config as belt-and-braces. This
// keeps local disposable Postgres containers reachable regardless of
// what the caller's env variable ships with. We avoid setting
// sslmode via ConnConfig.RuntimeParams because PostgreSQL rejects it
// as an unrecognized configuration parameter at startup. The pool
// uses at most 8 connections. The caller MUST call pool.Close when done.
func NewTestPoolFromEnv(t *testing.T) *pgxpool.Pool {
	t.Helper()
	rawURL, ok := testDatabaseURL()
	if !ok {
		t.Skip("no test database URL is set")
	}

	cleanURL := disableSSLInURL(rawURL)
	config, err := pgxpool.ParseConfig(cleanURL)
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

// NewTestPoolFromEnvWithSchema returns a test pool isolated to a schema named
// from the current test. The schema is dropped automatically during cleanup.
func NewTestPoolFromEnvWithSchema(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := NewTestPoolFromEnv(t)
	_, cleanup := PerTestSchema(t, pool)
	t.Cleanup(cleanup)
	return pool
}

// PerTestSchema creates and selects a schema derived from t.Name. It resets
// the pool after changing its connection configuration so existing idle
// connections also receive the new search_path. Call cleanup when the test is
// finished; the helper intentionally does not register it automatically.
func PerTestSchema(t *testing.T, pool *pgxpool.Pool) (schema string, cleanup func()) {
	t.Helper()
	if pool == nil {
		t.Fatal("PerTestSchema called with nil pool")
	}
	digest := sha256.Sum256([]byte(t.Name()))
	schema = "proxycore_test_" + hex.EncodeToString(digest[:16])
	ident := pgx.Identifier{schema}.Sanitize()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, "create schema if not exists "+ident); err != nil {
		t.Fatalf("create per-test schema %q: %v", schema, err)
	}
	config := pool.Config()
	if config.ConnConfig.RuntimeParams == nil {
		config.ConnConfig.RuntimeParams = map[string]string{}
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool.Reset()
	cleanup = func() {
		if _, err := pool.Exec(context.Background(), "drop schema if exists "+ident+" cascade"); err != nil {
			t.Logf("drop per-test schema %q: %v", schema, err)
		}
	}
	return schema, cleanup
}

// disableSSLInURL drops any libpq sslmode-related query parameters from
// the connection string and forces sslmode=disable so local disposable
// Postgres containers are always reached over plain TCP. If the URL
// cannot be parsed, it is returned unchanged and the caller surfaces
// the parse error (which is what we want for malformed URLs).
func disableSSLInURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	q := u.Query()
	for _, k := range []string{"sslmode", "sslrootcert", "sslkey", "sslcert"} {
		q.Del(k)
	}
	q.Set("sslmode", "disable")
	u.RawQuery = q.Encode()
	return u.String()
}
