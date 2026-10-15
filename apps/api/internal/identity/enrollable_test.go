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

func TestUnlinkedNodeMayReEnrollButLinkedNodeStaysRefused(t *testing.T) {
	key := uuid.New()
	for _, test := range []struct {
		name     string
		role     domain.TopologyRole
		unlinked bool
		want     bool
	}{
		{"linked node with key", domain.TopologyRoleNode, false, false},
		{"unlinked node with key", domain.TopologyRoleNode, true, true},
		{"overlay is meaningless on primary-with-nodes", domain.TopologyRolePrimaryWithNodes, true, false},
		{"overlay is meaningless on unknown role", domain.TopologyRole("bogus"), true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := (Identity{Role: test.role, ClusterKeyID: &key, NodeUnlinked: test.unlinked}).CanEnrollAsNode(); got != test.want {
				t.Fatalf("CanEnrollAsNode()=%t want %t", got, test.want)
			}
		})
	}
}

func TestCommitNodeEnrollmentAllowsUnlinkedNodeOnlyWhileOverlayIsSet(t *testing.T) {
	store := newFakeStore()
	key := uuid.New()
	store.seed(Identity{InstallationID: domain.NewInstallationID(), NodeID: domain.NewNodeID(), Role: domain.TopologyRoleNode,
		LeadershipGeneration: 2, LatestKnownGeneration: 2, ClusterKeyID: &key})
	svc := NewService(store)
	if _, err := svc.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	next := uuid.New()
	if _, err := svc.CommitNodeEnrollment(t.Context(), &next, 3); err == nil {
		t.Fatal("linked node must be refused")
	}
	svc.SetNodeUnlinked(true)
	got, err := svc.CommitNodeEnrollment(t.Context(), &next, 3)
	if err != nil || got.ClusterKeyID == nil || *got.ClusterKeyID != next {
		t.Fatalf("unlinked re-enroll: %+v err=%v", got, err)
	}
	// Service.Load replaces the cache: the overlay never survives it on its
	// own, so a failed store read cannot leave a node marked unlinked.
	svc.SetNodeUnlinked(true)
	if _, err := svc.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	if svc.Current().NodeUnlinked {
		t.Fatal("Load must not carry the overlay")
	}
}
