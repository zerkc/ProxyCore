package dbexport

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

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

// AcquireImportTestGate serializes only the dbimport and httpexport test processes
// sharing one database. Its session lock is separate from the product import lock.
// Keep the returned release function until after m.Run; closing the connection
// releases the lock. Callers release it before os.Exit (LIFO process cleanup).
func AcquireImportTestGate() (func() error, error) {
	rawURL, ok := testDatabaseURL()
	if !ok {
		return func() error { return nil }, nil
	}
	config, err := pgx.ParseConfig(disableSSLInURL(rawURL))
	if err != nil {
		return nil, fmt.Errorf("parse test database URL: %w", err)
	}
	config.TLSConfig = nil
	config.Fallbacks = nil
	ctx, cancel := context.WithTimeout(context.Background(), 11*time.Minute)
	defer cancel()
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("connect import test gate: %w", err)
	}
	const gateNamespace int32 = 0x50584354
	const gateKey int32 = 0x494d504f
	for {
		var acquired bool
		if err := conn.QueryRow(ctx, "select pg_try_advisory_lock($1, $2)", gateNamespace, gateKey).Scan(&acquired); err != nil {
			_ = conn.Close(context.Background())
			return nil, fmt.Errorf("acquire import test gate: %w", err)
		}
		if acquired {
			return func() error {
				closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				return conn.Close(closeCtx)
			}, nil
		}
		select {
		case <-ctx.Done():
			_ = conn.Close(context.Background())
			return nil, fmt.Errorf("acquire import test gate: %w", ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
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
	basePool := NewTestPoolFromEnv(t)
	pool, _, cleanup := PerTestSchema(t, basePool)
	basePool.Close()
	t.Cleanup(pool.Close)
	t.Cleanup(cleanup)
	return pool
}

// PerTestSchema creates a schema derived from t.Name and returns a new pool
// configured to select it. The caller closes the base pool after this returns,
// then registers the new pool's Close before cleanup so cleanup runs first.
func PerTestSchema(t *testing.T, pool *pgxpool.Pool) (isolated *pgxpool.Pool, schema string, cleanup func()) {
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
	runtimeParams := make(map[string]string, len(config.ConnConfig.RuntimeParams)+1)
	for key, value := range config.ConnConfig.RuntimeParams {
		runtimeParams[key] = value
	}
	runtimeParams["search_path"] = schema
	config.ConnConfig.RuntimeParams = runtimeParams
	isolated, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("create isolated pool for schema %q: %v", schema, err)
	}
	cleanup = func() {
		if _, err := isolated.Exec(context.Background(), "drop schema if exists "+ident+" cascade"); err != nil {
			t.Logf("drop per-test schema %q: %v", schema, err)
		}
	}
	return isolated, schema, cleanup
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
