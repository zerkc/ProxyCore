package configuration

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zerkc/ProxyCore/apps/api/internal/auth"
)

func TestEnsureSchemaIsRaceSafeUnderParallelCalls(t *testing.T) {
	databaseURL := configurationTestDatabaseURL()
	if databaseURL == "" {
		t.Skip("no configuration test database URL is set")
	}

	ctx := context.Background()
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	t.Cleanup(admin.Close)

	schema := "ensure_schema_parallel_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	ident := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "create schema "+ident); err != nil {
		t.Fatalf("create isolated schema: %v", err)
	}

	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse database config: %v", err)
	}
	if config.ConnConfig.RuntimeParams == nil {
		config.ConnConfig.RuntimeParams = map[string]string{}
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	config.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("connect isolated schema: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(), "drop schema if exists "+ident+" cascade")
	})

	if err := auth.NewPostgresStore(pool).EnsureSchema(ctx); err != nil {
		t.Fatalf("ensure auth schema: %v", err)
	}

	results := make(chan error, 2)
	var waitGroup sync.WaitGroup
	waitGroup.Add(2)
	for range 2 {
		go func() {
			defer waitGroup.Done()
			results <- EnsureSchema(ctx, pool)
		}()
	}
	waitGroup.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatalf("parallel EnsureSchema: %v", err)
		}
	}

	for call := 1; call <= 3; call++ {
		if err := EnsureSchema(ctx, pool); err != nil {
			t.Fatalf("sequential EnsureSchema call %d: %v", call, err)
		}
	}
}

func configurationTestDatabaseURL() string {
	for _, name := range []string{"PHASE2_DATABASE_URL", "PGX_TEST_DATABASE_URL"} {
		if value := os.Getenv(name); value != "" {
			return value
		}
	}
	return ""
}
