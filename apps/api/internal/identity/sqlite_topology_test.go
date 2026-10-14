package identity

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

const (
	topoKeyID   = "cccccccc-2222-4222-8222-222222222222"
	topoCredA   = "dddddddd-1111-4111-8111-111111111111"
	topoCredB   = "dddddddd-2222-4222-8222-222222222222"
	topoNodeA   = "eeeeeeee-1111-4111-8111-111111111111"
	topoNodeB   = "eeeeeeee-2222-4222-8222-222222222222"
	topoInstA   = "ffffffff-1111-4111-8111-111111111111"
	topoInstB   = "ffffffff-2222-4222-8222-222222222222"
	topoPrimary = "99999999-1111-4111-8111-111111111111"
	topoAttempt = "88888888-1111-4111-8111-111111111111"
)

// seedTopology bootstraps an identity with the given role and generation and
// inserts a cluster key, two enrolled nodes with credentials, and a populated
// node_state relationship.
func (f *sqliteIdentityFixture) seedTopology(t *testing.T, role domain.TopologyRole, generation domain.LeadershipGeneration) {
	t.Helper()
	ctx := context.Background()
	if _, _, err := f.store.Ensure(ctx, sqliteInstA, sqliteNodeA); err != nil {
		t.Fatal(err)
	}
	if err := f.store.UpdateRole(ctx, role); err != nil {
		t.Fatal(err)
	}
	if err := f.store.UpdateLeadershipGeneration(ctx, generation); err != nil {
		t.Fatal(err)
	}
	key := uuid.MustParse(topoKeyID)
	if err := f.store.writer.Write(ctx, func(v sqlitedb.WriteValue) error {
		exec := func(query string, args ...any) error {
			_, err := v.Conn().ExecContext(v.Context(), query, args...)
			return err
		}
		stmts := []struct {
			q string
			a []any
		}{
			{`insert into cluster_keys (id, purpose, wrapped_kek, wrapping_key_version, created_at) values (?, 'cluster-kek', 'wrapped', 1, ?)`, []any{topoKeyID, v.Timestamp()}},
			{`insert into node_credentials (id, node_id, credential_hash, hash_version, created_at) values (?, ?, 'h1', 'v1', ?)`, []any{topoCredA, topoNodeA, v.Timestamp()}},
			{`insert into node_credentials (id, node_id, credential_hash, hash_version, created_at) values (?, ?, 'h2', 'v1', ?)`, []any{topoCredB, topoNodeB, v.Timestamp()}},
			{`insert into enrolled_nodes (node_id, installation_id, primary_id, credential_id, enrolled_at, created_by_attempt_id) values (?, ?, ?, ?, ?, ?)`,
				[]any{topoNodeA, topoInstA, topoPrimary, topoCredA, v.Timestamp(), topoAttempt}},
			{`insert into node_state (id, enrollment_primary_id, primary_url, primary_installation_id, credential_id, sync_enabled, updated_at)
			  values ('default', ?, 'https://old-primary.example', ?, ?, 1, ?)`, []any{topoPrimary, topoInstB, topoCredB, v.Timestamp()}},
		}
		for _, s := range stmts {
			if err := exec(s.q, s.a...); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.store.UpdateClusterKeyID(ctx, &key); err != nil {
		t.Fatal(err)
	}
}

func (f *sqliteIdentityFixture) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := f.db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

func TestSQLitePromoteTopologyIsTransactional(t *testing.T) {
	f := newSQLiteIdentityFixture(t)
	f.seedTopology(t, domain.TopologyRoleNode, 4)
	ctx := context.Background()

	change, err := f.store.PromoteTopology(ctx, PromoteInput{
		FromRole: domain.TopologyRoleNode, FromGeneration: 4, NextGeneration: 5,
	})
	if err != nil {
		t.Fatalf("PromoteTopology: %v", err)
	}
	if change.Generation != 5 || change.RevokedCredentials != 2 || change.ClusterKeyID != nil {
		t.Fatalf("change=%+v", change)
	}
	got, err := f.store.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Role != domain.TopologyRolePrimary || got.LeadershipGeneration != 5 ||
		got.LatestKnownGeneration != 5 || got.ClusterKeyID != nil {
		t.Fatalf("identity=%+v", got)
	}
	if n := f.count(t, `select count(*) from node_credentials where revoked_at is null`); n != 0 {
		t.Fatalf("%d credentials still active", n)
	}
	if n := f.count(t, `select count(*) from enrolled_nodes where revoked_at is null`); n != 0 {
		t.Fatalf("%d enrolled nodes still active", n)
	}
	if n := f.count(t, `select count(*) from cluster_keys where retired_at is null`); n != 0 {
		t.Fatalf("%d cluster keys still active", n)
	}
	if n := f.count(t, `select count(*) from node_state where enrollment_primary_id is null and primary_url is null
		and primary_installation_id is null and credential_id is null and sync_enabled = 0`); n != 1 {
		t.Fatalf("node_state relationship was not reset")
	}
	if n := f.count(t, `select count(*) from audit_events where action = 'topology.promote' and result = 'success'
		and json_extract(after_value, '$.generation') = 5 and json_extract(after_value, '$.revokedCredentials') = 2`); n != 1 {
		t.Fatalf("expected one promote audit event, got %d", n)
	}
}

func TestSQLitePromoteTopologyRejectsStaleGenerationAtomically(t *testing.T) {
	f := newSQLiteIdentityFixture(t)
	f.seedTopology(t, domain.TopologyRoleNode, 4)
	ctx := context.Background()

	_, err := f.store.PromoteTopology(ctx, PromoteInput{
		FromRole: domain.TopologyRoleNode, FromGeneration: 3, NextGeneration: 4,
	})
	if !errors.Is(err, ErrGenerationMismatch) {
		t.Fatalf("err=%v, want ErrGenerationMismatch", err)
	}
	got, _ := f.store.Get(ctx)
	if got.Role != domain.TopologyRoleNode || got.LeadershipGeneration != 4 || got.ClusterKeyID == nil {
		t.Fatalf("identity mutated: %+v", got)
	}
	if n := f.count(t, `select count(*) from node_credentials where revoked_at is null`); n != 2 {
		t.Fatalf("credentials revoked by a refused promotion: %d active", n)
	}
	if n := f.count(t, `select count(*) from audit_events where action = 'topology.promote'`); n != 0 {
		t.Fatalf("audit written for refused promotion")
	}
}

func TestSQLiteDemoteTopologyRevokesNodesAndSetsNode(t *testing.T) {
	f := newSQLiteIdentityFixture(t)
	f.seedTopology(t, domain.TopologyRolePrimaryWithNodes, 7)
	ctx := context.Background()

	change, err := f.store.DemoteTopology(ctx, DemoteInput{FromRole: domain.TopologyRolePrimaryWithNodes, FromGeneration: 7})
	if err != nil {
		t.Fatalf("DemoteTopology: %v", err)
	}
	if change.Role != domain.TopologyRoleNode || change.Generation != 7 || change.RevokedCredentials != 2 {
		t.Fatalf("change=%+v", change)
	}
	got, _ := f.store.Get(ctx)
	if got.Role != domain.TopologyRoleNode || got.LeadershipGeneration != 7 || got.ClusterKeyID != nil {
		t.Fatalf("identity=%+v", got)
	}
	if n := f.count(t, `select count(*) from node_credentials where revoked_at is null`); n != 0 {
		t.Fatalf("%d credentials still active", n)
	}
	if n := f.count(t, `select count(*) from audit_events where action = 'topology.demote'`); n != 1 {
		t.Fatalf("expected one demote audit event, got %d", n)
	}
}

func TestSQLiteDemoteTopologyRejectsRoleDrift(t *testing.T) {
	f := newSQLiteIdentityFixture(t)
	f.seedTopology(t, domain.TopologyRoleNode, 7)
	_, err := f.store.DemoteTopology(context.Background(), DemoteInput{FromRole: domain.TopologyRolePrimary, FromGeneration: 7})
	if !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("err=%v, want ErrInvalidTransition", err)
	}
	if n := f.count(t, `select count(*) from node_credentials where revoked_at is null`); n != 2 {
		t.Fatalf("credentials revoked despite refusal")
	}
}

func TestSQLiteServicePromoteThenStalePrimaryWritesStayBlocked(t *testing.T) {
	f := newSQLiteIdentityFixture(t)
	f.seedTopology(t, domain.TopologyRoleNode, 4)
	ctx := context.Background()
	svc := NewService(f.store)
	if _, err := svc.Load(ctx); err != nil {
		t.Fatal(err)
	}
	result, err := svc.Promote(ctx, PromoteOptions{})
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if !svc.IsWritable() || result.Identity.LeadershipGeneration != 5 || result.RevokedCredentials != 2 {
		t.Fatalf("result=%+v writable=%v", result, svc.IsWritable())
	}
	// The former primary later observes the new generation and must be blocked.
	other := NewService(f.store)
	if _, err := other.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := other.RecordImportedSnapshot(ctx, uuid.New(), 6); err != nil {
		t.Fatal(err)
	}
	if other.IsWritable() || !other.IsStalePrimary() {
		t.Fatalf("stale primary must not be writable: %+v", other.Current())
	}
}
