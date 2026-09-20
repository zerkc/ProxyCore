package dbexport

import (
	"context"
	"net/url"
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
