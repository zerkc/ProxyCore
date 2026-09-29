package auth

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPostgresStoreEnsureSchemaConcurrent(t *testing.T) {
	url := os.Getenv("PGX_TEST_DATABASE_URL")
	if url == "" {
		url = os.Getenv("DATABASE_URL")
	}
	if url == "" {
		t.Skip("PGX_TEST_DATABASE_URL and DATABASE_URL are unset")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	for attempt := 0; attempt < 3; attempt++ {
		schema := "auth_schema_parallel_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		ident := pgx.Identifier{schema}.Sanitize()
		if _, err := admin.Exec(ctx, "create schema "+ident); err != nil {
			t.Fatal(err)
		}
		config, err := pgxpool.ParseConfig(url)
		if err != nil {
			t.Fatal(err)
		}
		if config.ConnConfig.RuntimeParams == nil {
			config.ConnConfig.RuntimeParams = map[string]string{}
		}
		config.ConnConfig.RuntimeParams["search_path"] = schema
		config.MaxConns = 12
		pool, err := pgxpool.NewWithConfig(ctx, config)
		if err != nil {
			t.Fatal(err)
		}
		store := NewPostgresStore(pool)
		const workers = 12
		start := make(chan struct{})
		results := make(chan error, workers)
		var wg sync.WaitGroup
		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func() { defer wg.Done(); <-start; results <- store.EnsureSchema(ctx) }()
		}
		close(start)
		wg.Wait()
		close(results)
		for err := range results {
			if err != nil {
				t.Errorf("attempt %d: concurrent EnsureSchema: %v", attempt, err)
			}
		}
		if err := store.EnsureSchema(ctx); err != nil {
			t.Errorf("attempt %d: repeated EnsureSchema: %v", attempt, err)
		}
		pool.Close()
		if _, err := admin.Exec(ctx, "drop schema "+ident+" cascade"); err != nil {
			t.Fatal(err)
		}
	}
}
