package hostread

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPostgresAppliedRevisionAndCertFiles(t *testing.T) {
	url, ok := hostreadTestDatabaseURL()
	if !ok {
		t.Skip("no test database URL is set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := openHostreadTestPool(t, ctx, url)

	if _, err := pool.Exec(ctx, `create table config_revisions (
		id text primary key,
		applied_at timestamptz
	)`); err != nil {
		t.Fatalf("create config_revisions fixture: %v", err)
	}

	root := filepath.Join(t.TempDir(), "candidates")
	reader := New(pool, Config{CandidateRoot: root})
	if _, err := reader.AppliedRevision(ctx); !errors.Is(err, ErrNoAppliedRevision) {
		t.Fatalf("empty AppliedRevision error = %v, want ErrNoAppliedRevision", err)
	}

	olderRevision := "00000000-0000-4000-8000-000000000001"
	newerRevision := "00000000-0000-4000-8000-000000000002"
	olderAt := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	newerAt := olderAt.Add(time.Minute)
	if _, err := pool.Exec(ctx, `insert into config_revisions (id, applied_at)
		values ($1, $2), ($3, $4)`, olderRevision, olderAt, newerRevision, newerAt); err != nil {
		t.Fatalf("insert config revisions: %v", err)
	}

	appliedRevision, err := reader.AppliedRevision(ctx)
	if err != nil {
		t.Fatalf("AppliedRevision: %v", err)
	}
	if appliedRevision != newerRevision {
		t.Fatalf("AppliedRevision = %q, want %q", appliedRevision, newerRevision)
	}

	certDir := filepath.Join(root, newerRevision, "nginx", "certs")
	if err := os.MkdirAll(certDir, 0o755); err != nil {
		t.Fatalf("create cert fixture: %v", err)
	}
	fixtures := map[string]string{
		"a.crt": "certificate-a",
		"b.crt": "certificate-b",
		"a.key": "key-a",
		"b.key": "key-b",
	}
	for name, contents := range fixtures {
		if err := os.WriteFile(filepath.Join(certDir, name), []byte(contents), 0o600); err != nil {
			t.Fatalf("write certificate fixture %q: %v", name, err)
		}
	}

	files, err := reader.AppliedCertFiles(ctx, appliedRevision)
	if err != nil {
		t.Fatalf("AppliedCertFiles: %v", err)
	}
	if len(files) != len(fixtures) {
		t.Fatalf("AppliedCertFiles count = %d, want %d", len(files), len(fixtures))
	}
	for _, file := range files {
		name := strings.TrimPrefix(file.Path, "certs/")
		contents, ok := fixtures[name]
		if !ok {
			t.Errorf("unexpected certificate path %q", file.Path)
			continue
		}
		if file.Size != int64(len(contents)) {
			t.Errorf("certificate %q size = %d, want %d", file.Path, file.Size, len(contents))
		}
	}
}

func hostreadTestDatabaseURL() (string, bool) {
	if url := os.Getenv("PGX_TEST_DATABASE_URL"); url != "" {
		return url, true
	}
	if url := os.Getenv("DATABASE_URL"); url != "" {
		return url, true
	}
	return "", false
}

func openHostreadTestPool(t *testing.T, ctx context.Context, url string) *pgxpool.Pool {
	t.Helper()
	bootstrapConfig, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse test database URL: %v", err)
	}
	bootstrapConfig.MaxConns = 1
	bootstrap, err := pgxpool.NewWithConfig(ctx, bootstrapConfig)
	if err != nil {
		t.Fatalf("create bootstrap test pool: %v", err)
	}
	t.Cleanup(bootstrap.Close)
	if err := bootstrap.Ping(ctx); err != nil {
		t.Fatalf("ping bootstrap test database: %v", err)
	}

	schema := fmt.Sprintf("hostread_test_%d", time.Now().UnixNano())
	if _, err := bootstrap.Exec(ctx, "create schema "+quoteHostreadIdentifier(schema)); err != nil {
		t.Fatalf("create test schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = bootstrap.Exec(context.Background(), "drop schema "+quoteHostreadIdentifier(schema)+" cascade")
	})

	fixtureConfig, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse fixture database URL: %v", err)
	}
	fixtureConfig.MaxConns = 2
	if fixtureConfig.ConnConfig.RuntimeParams == nil {
		fixtureConfig.ConnConfig.RuntimeParams = make(map[string]string)
	}
	fixtureConfig.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, fixtureConfig)
	if err != nil {
		t.Fatalf("create fixture test pool: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping fixture test database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func quoteHostreadIdentifier(identifier string) string {
	return `"` + strings.ReplaceAll(identifier, `"`, `""`) + `"`
}
