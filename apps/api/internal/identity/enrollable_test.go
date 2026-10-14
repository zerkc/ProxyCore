package identity

import (
	"testing"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
)

func TestIdentityCanEnrollAsNode(t *testing.T) {
	key := uuid.New()
	for _, test := range []struct {
		name string
		role domain.TopologyRole
		key  *uuid.UUID
		want bool
	}{
		{"standalone", domain.TopologyRoleStandalone, nil, true},
		{"standalone with own key", domain.TopologyRoleStandalone, &key, true},
		{"primary", domain.TopologyRolePrimary, &key, true},
		{"stale primary", domain.TopologyRoleStalePrimary, &key, true},
		{"demoted unbound node", domain.TopologyRoleNode, nil, true},
		{"bound node", domain.TopologyRoleNode, &key, false},
		{"primary with nodes", domain.TopologyRolePrimaryWithNodes, &key, false},
		{"unknown role", domain.TopologyRole("bogus"), nil, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := Identity{Role: test.role, ClusterKeyID: test.key}.CanEnrollAsNode()
			if got != test.want {
				t.Fatalf("CanEnrollAsNode()=%t, want %t", got, test.want)
			}
			if RoleCanEnrollAsNode(test.role, test.key != nil) != test.want {
				t.Fatalf("RoleCanEnrollAsNode mismatch for %s", test.name)
			}
		})
	}
}

func TestCommitNodeEnrollmentSetsRoleAndClusterKey(t *testing.T) {
	store := newFakeStore()
	store.seed(Identity{InstallationID: domain.NewInstallationID(), NodeID: domain.NewNodeID(), Role: domain.TopologyRolePrimary, LeadershipGeneration: 2, LatestKnownGeneration: 2})
	svc := NewService(store)
	if _, err := svc.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	key := uuid.New()
	got, err := svc.CommitNodeEnrollment(t.Context(), &key, 5)
	if err != nil {
		t.Fatal(err)
	}
	if got.LeadershipGeneration != 5 || got.LatestKnownGeneration != 5 || got.IsStalePrimary() {
		t.Fatalf("generation not adopted: %+v", got)
	}
	if got.Role != domain.TopologyRoleNode || got.ClusterKeyID == nil || *got.ClusterKeyID != key {
		t.Fatalf("identity=%+v", got)
	}
	persisted, _ := store.Get(t.Context())
	if persisted.Role != domain.TopologyRoleNode || persisted.ClusterKeyID == nil || *persisted.ClusterKeyID != key {
		t.Fatalf("persisted=%+v", persisted)
	}
	// A bound node must be refused and left untouched.
	other := uuid.New()
	if _, err := svc.CommitNodeEnrollment(t.Context(), &other, 5); err == nil {
		t.Fatal("bound node must be refused")
	}
	if cur := svc.Current(); *cur.ClusterKeyID != key {
		t.Fatalf("key changed: %v", cur.ClusterKeyID)
	}
}
