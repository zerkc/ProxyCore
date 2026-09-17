package enrollment

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zerkc/ProxyCore/apps/api/internal/auth"
	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
)

type stubStore struct{}

func (*stubStore) CreateEnrollmentToken(context.Context, string, string, string, string, string, time.Time, time.Time) (domain.TopologyRole, error) {
	return domain.TopologyRolePrimary, nil
}
func (*stubStore) CheckEnrollmentToken(context.Context, string, string, time.Time, bool) error {
	return nil
}
func (*stubStore) RevokeEnrollmentToken(context.Context, string, string, time.Time) error { return nil }

func owner() auth.User { return auth.User{ID: uuid.NewString(), Role: auth.RoleOwner, Active: true} }

func TestCreateTokenRequiresActiveOwner(t *testing.T) {
	for _, user := range []auth.User{
		{ID: "operator", Role: auth.RoleOperator, Active: true},
		{ID: "inactive", Role: auth.RoleOwner},
	} {
		_, err := NewService(nil, Options{}).CreateToken(context.Background(), user)
		if !errors.Is(err, ErrOwnerRequired) {
			t.Fatalf("CreateToken(%+v) error = %v", user, err)
		}
	}
}

func TestCreateTokenFormatAndEntropy(t *testing.T) {
	created, err := NewService(&stubStore{}, Options{TTL: 10 * time.Minute}).CreateToken(context.Background(), owner())
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	parts := strings.SplitN(created.Token, "_", 3)
	if len(parts) != 3 {
		t.Fatalf("token format has %d sections, want 3", len(parts))
	}
	if parts[0] != tokenPrefix || len(parts[1]) != tokenSelectorLen*2 || len(parts[2]) != 43 {
		t.Fatalf("token format lengths invalid: prefix=%q selectorLen=%d secretLen=%d", parts[0], len(parts[1]), len(parts[2]))
	}
}

func TestPostgresEnrollmentTokenTransactionAndSingleUse(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	actor := owner()
	if _, err := pool.Exec(ctx, `insert into users (id, username, password_hash, role) values ($1, $2, 'test-hash', 'owner')`, actor.ID, "owner_"+actor.ID[:8]); err != nil {
		t.Fatalf("insert owner: %v", err)
	}
	if _, err := pool.Exec(ctx, `insert into installation_identity (id, installation_id, node_id) values ('default', gen_random_uuid(), gen_random_uuid())`); err != nil {
		t.Fatalf("insert identity: %v", err)
	}
	store := configuration.NewPhase2Store(pool)
	now := time.Date(2026, 10, 2, 3, 4, 5, 0, time.UTC)
	if _, err := store.CreateEnrollmentToken(ctx, uuid.NewString(), "rollback-selector", strings.Repeat("a", 64), TokenHashVersion, uuid.NewString(), now, now.Add(time.Hour)); err == nil {
		t.Fatal("expected invalid owner insert to fail")
	}
	var role string
	if err := pool.QueryRow(ctx, `select role::text from installation_identity where id = 'default'`).Scan(&role); err != nil {
		t.Fatalf("read role after failed insert: %v", err)
	}
	if role != "standalone-primary" {
		t.Fatalf("role after failed insert = %q, want standalone-primary", role)
	}
	var count int
	if err := pool.QueryRow(ctx, `select count(*) from enrollment_tokens`).Scan(&count); err != nil {
		t.Fatalf("count tokens after rollback: %v", err)
	}
	if count != 0 {
		t.Fatalf("token rows after rollback = %d, want 0", count)
	}

	service := NewService(store, Options{TTL: 10 * time.Minute, Now: func() time.Time { return now }})
	issued, err := service.CreateToken(ctx, actor)
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	if err := pool.QueryRow(ctx, `select role::text from installation_identity where id = 'default'`).Scan(&role); err != nil {
		t.Fatalf("read role after first token: %v", err)
	}
	if role != "primary" {
		t.Fatalf("role after first token = %q, want primary", role)
	}
	var selector, hash, version string
	if err := pool.QueryRow(ctx, `select token_selector, token_hash, hash_version from enrollment_tokens where id = $1`, issued.ID).Scan(&selector, &hash, &version); err != nil {
		t.Fatalf("read stored token metadata: %v", err)
	}
	if selector == "" {
		t.Fatal("stored token selector is empty")
	}
	if len(hash) != 64 {
		t.Fatalf("stored token hash length = %d, want 64", len(hash))
	}
	if version != TokenHashVersion {
		t.Fatalf("stored hash version = %q, want %q", version, TokenHashVersion)
	}
	if strings.Contains(hash, issued.Token) {
		t.Fatal("stored token hash contains plaintext token")
	}
	if _, err := pool.Exec(ctx, `update enrollment_tokens set hash_version = 'unknown-v2' where id = $1`, issued.ID); err != nil {
		t.Fatalf("set unknown hash version: %v", err)
	}
	if err := service.VerifyToken(ctx, issued.Token); !errors.Is(err, ErrEnrollmentDenied) {
		t.Fatalf("unknown hash version was accepted: %v", err)
	}
	if _, err := pool.Exec(ctx, `update enrollment_tokens set hash_version = $1 where id = $2`, TokenHashVersion, issued.ID); err != nil {
		t.Fatalf("restore supported hash version: %v", err)
	}
	if err := service.VerifyToken(ctx, issued.Token); err != nil {
		t.Fatalf("VerifyToken before consume: %v", err)
	}
	results := make(chan error, 12)
	var wg sync.WaitGroup
	for i := 0; i < cap(results); i++ {
		wg.Add(1)
		go func() { defer wg.Done(); results <- service.ConsumeToken(ctx, issued.Token) }()
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		} else if !errors.Is(err, ErrEnrollmentDenied) {
			t.Fatalf("concurrent consume error = %v", err)
		}
	}
	if successes != 1 {
		t.Fatalf("successful consumes = %d, want 1", successes)
	}
	revoked, err := service.CreateToken(ctx, actor)
	if err != nil {
		t.Fatalf("create revoke test token: %v", err)
	}
	if err := service.RevokeToken(ctx, actor, revoked.ID); err != nil {
		t.Fatalf("revoke token: %v", err)
	}
	if err := service.ConsumeToken(ctx, revoked.Token); !errors.Is(err, ErrEnrollmentDenied) {
		t.Fatalf("revoked token was accepted: %v", err)
	}
	expired, err := service.CreateToken(ctx, actor)
	if err != nil {
		t.Fatalf("create expiry test token: %v", err)
	}
	late := NewService(store, Options{Now: func() time.Time { return now.Add(11 * time.Minute) }})
	if err := late.ConsumeToken(ctx, expired.Token); !errors.Is(err, ErrEnrollmentDenied) {
		t.Fatalf("expired token was accepted: %v", err)
	}

	if _, err := service.CreateToken(ctx, actor); err != nil {
		t.Fatalf("primary token: %v", err)
	}
	if _, err := pool.Exec(ctx, `update installation_identity set role = 'primary-with-nodes' where id = 'default'`); err != nil {
		t.Fatal(err)
	}
	if _, err := service.CreateToken(ctx, actor); err != nil {
		t.Fatalf("primary-with-nodes token: %v", err)
	}
	for _, blocked := range []string{"node", "stale-primary"} {
		if _, err := pool.Exec(ctx, `update installation_identity set role = $1 where id = 'default'`, blocked); err != nil {
			t.Fatal(err)
		}
		if _, err := service.CreateToken(ctx, actor); err == nil {
			t.Fatalf("role %q issued a token", blocked)
		}
	}
}

func postgresPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := strings.TrimSpace(strings.SplitN(os.Getenv("DATABASE_URL"), "\x00", 2)[0])
	if dsn == "" {
		t.Skip("DATABASE_URL is not set")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Skip("cannot connect to database: " + err.Error())
	}
	schema := "pne1_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	ident := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "create schema "+ident); err != nil {
		admin.Close()
		t.Skip("cannot create schema: " + err.Error())
	}
	var pool *pgxpool.Pool
	t.Cleanup(func() {
		if pool != nil {
			pool.Close()
		}
		_, _ = admin.Exec(context.Background(), "drop schema if exists "+ident+" cascade")
		admin.Close()
	})
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse database config: %v", err)
	}
	cfg.ConnConfig.RuntimeParams = map[string]string{"search_path": schema}
	pool, err = pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect isolated schema: %v", err)
	}
	if err := auth.NewPostgresStore(pool).EnsureSchema(ctx); err != nil {
		t.Fatalf("ensure auth schema: %v", err)
	}
	if err := configuration.EnsureSchema(ctx, pool); err != nil {
		t.Fatalf("ensure configuration schema: %v", err)
	}
	return pool
}
