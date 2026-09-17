package enrollment

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zerkc/ProxyCore/apps/api/internal/auth"
	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/identity"
)

func TestPostgresEnrollmentActivatesLiveIdentity(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	actor := seedLiveIdentity(t, pool)
	live := identity.NewService(identity.NewPgStore(pool))
	if _, err := live.Load(ctx); err != nil {
		t.Fatalf("load live identity: %v", err)
	}
	service := NewServiceWithIdentity(configuration.NewPhase2Store(pool), live, Options{TTL: 10 * time.Minute})
	if _, err := service.CreateToken(ctx, actor); err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	got := live.Current()
	if got.Role != domain.TopologyRolePrimary {
		t.Fatalf("live identity role = %s, want primary", got.Role)
	}
	if got.UpdatedAt.IsZero() {
		t.Fatal("live identity UpdatedAt was not refreshed")
	}
}

func TestPostgresEnrollmentActivationRollbackKeepsLiveStandalone(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	seedLiveIdentity(t, pool)
	live := identity.NewService(identity.NewPgStore(pool))
	if _, err := live.Load(ctx); err != nil {
		t.Fatalf("load live identity: %v", err)
	}
	service := NewServiceWithIdentity(configuration.NewPhase2Store(pool), live, Options{TTL: 10 * time.Minute})
	invalidOwner := auth.User{ID: uuid.NewString(), Role: auth.RoleOwner, Active: true}
	if _, err := service.CreateToken(ctx, invalidOwner); err == nil {
		t.Fatal("CreateToken unexpectedly succeeded for missing owner row")
	}
	if got := live.Current().Role; got != domain.TopologyRoleStandalone {
		t.Fatalf("live identity role after rollback = %s, want standalone-primary", got)
	}
	var role string
	if err := pool.QueryRow(ctx, `select role::text from installation_identity where id = 'default'`).Scan(&role); err != nil {
		t.Fatalf("read durable role after rollback: %v", err)
	}
	if role != string(domain.TopologyRoleStandalone) {
		t.Fatalf("durable role after rollback = %q, want standalone-primary", role)
	}
}

func seedLiveIdentity(t *testing.T, pool *pgxpool.Pool) auth.User {
	t.Helper()
	ctx := context.Background()
	actor := owner()
	if _, err := pool.Exec(ctx, `insert into users (id, username, password_hash, role) values ($1, $2, 'test-hash', 'owner')`, actor.ID, "owner_"+strings.ReplaceAll(actor.ID[:8], "-", "")); err != nil {
		t.Fatalf("insert owner: %v", err)
	}
	if _, err := pool.Exec(ctx, `insert into installation_identity (id, installation_id, node_id) values ('default', gen_random_uuid(), gen_random_uuid())`); err != nil {
		t.Fatalf("insert identity: %v", err)
	}
	return actor
}
