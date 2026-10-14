package identity

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
)

// fakeTopologyStore records workflow calls on top of the in-memory store.
type fakeTopologyStore struct {
	*fakeStore
	promotes []PromoteInput
	demotes  []DemoteInput
	revoked  int
	keyID    *uuid.UUID
	err      error
}

func (f *fakeTopologyStore) PromoteTopology(_ context.Context, in PromoteInput) (TopologyChange, error) {
	if f.err != nil {
		return TopologyChange{}, f.err
	}
	f.promotes = append(f.promotes, in)
	return TopologyChange{
		Role: domain.TopologyRolePrimary, Generation: in.NextGeneration,
		ClusterKeyID: f.keyID, RevokedCredentials: f.revoked,
	}, nil
}

func (f *fakeTopologyStore) DemoteTopology(_ context.Context, in DemoteInput) (TopologyChange, error) {
	if f.err != nil {
		return TopologyChange{}, f.err
	}
	f.demotes = append(f.demotes, in)
	return TopologyChange{Role: domain.TopologyRoleNode, Generation: in.FromGeneration, RevokedCredentials: f.revoked}, nil
}

func newTopologyService(t *testing.T, role domain.TopologyRole, generation domain.LeadershipGeneration) (*Service, *fakeTopologyStore) {
	t.Helper()
	store := &fakeTopologyStore{fakeStore: newFakeStore(), revoked: 3}
	store.seed(Identity{
		InstallationID:        domain.NewInstallationID(),
		NodeID:                domain.NewNodeID(),
		Role:                  role,
		LeadershipGeneration:  generation,
		LatestKnownGeneration: generation,
	})
	svc := NewService(store)
	if _, err := svc.Load(context.Background()); err != nil {
		t.Fatalf("Load: %v", err)
	}
	return svc, store
}

func TestTransitionToAllowsPrimaryRolesToNodeAndKeepsInvalidOnesForbidden(t *testing.T) {
	for _, from := range []domain.TopologyRole{domain.TopologyRolePrimary, domain.TopologyRolePrimaryWithNodes} {
		svc, _ := newTopologyService(t, from, 4)
		got, err := svc.TransitionTo(context.Background(), domain.TopologyRoleNode)
		if err != nil || got.Role != domain.TopologyRoleNode {
			t.Fatalf("%s -> node: role=%s err=%v", from, got.Role, err)
		}
	}
	for _, tc := range []struct{ from, to domain.TopologyRole }{
		{domain.TopologyRolePrimary, domain.TopologyRoleStandalone},
		{domain.TopologyRolePrimaryWithNodes, domain.TopologyRoleStandalone},
		{domain.TopologyRoleNode, domain.TopologyRoleStandalone},
		{domain.TopologyRoleNode, domain.TopologyRolePrimaryWithNodes},
		{domain.TopologyRoleStandalone, domain.TopologyRolePrimary},
	} {
		svc, _ := newTopologyService(t, tc.from, 4)
		if _, err := svc.TransitionTo(context.Background(), tc.to); !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("%s -> %s: err=%v, want ErrInvalidTransition", tc.from, tc.to, err)
		}
	}
}

func TestTransitionToSameRoleIsIdempotent(t *testing.T) {
	svc, store := newTopologyService(t, domain.TopologyRoleNode, 4)
	got, err := svc.TransitionTo(context.Background(), domain.TopologyRoleNode)
	if err != nil || got.Role != domain.TopologyRoleNode || got.LeadershipGeneration != 4 {
		t.Fatalf("node -> node: %+v err=%v", got, err)
	}
	if len(store.promotes)+len(store.demotes) != 0 {
		t.Fatalf("same-role transition must not run a workflow")
	}
}

func TestPromoteRunsTransactionalWorkflowAndBumpsGeneration(t *testing.T) {
	svc, store := newTopologyService(t, domain.TopologyRoleNode, 4)
	expected := domain.LeadershipGeneration(4)
	result, err := svc.Promote(context.Background(), PromoteOptions{ActorUserID: "owner-1", ExpectedGeneration: &expected})
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if result.Identity.Role != domain.TopologyRolePrimary || result.Identity.LeadershipGeneration != 5 ||
		result.Identity.LatestKnownGeneration != 5 {
		t.Fatalf("identity=%+v", result.Identity)
	}
	if result.PreviousRole != domain.TopologyRoleNode || result.PreviousGeneration != 4 || result.RevokedCredentials != 3 {
		t.Fatalf("result=%+v", result)
	}
	if len(store.promotes) != 1 || store.promotes[0].ActorUserID != "owner-1" ||
		store.promotes[0].FromGeneration != 4 || store.promotes[0].NextGeneration != 5 {
		t.Fatalf("store input=%+v", store.promotes)
	}
	if svc.Current().LeadershipGeneration != 5 || svc.IsStalePrimary() {
		t.Fatalf("cache not updated: %+v", svc.Current())
	}
}

func TestPromoteRejectsExpectedGenerationMismatchWithoutWriting(t *testing.T) {
	svc, store := newTopologyService(t, domain.TopologyRoleNode, 4)
	stale := domain.LeadershipGeneration(3)
	_, err := svc.Promote(context.Background(), PromoteOptions{ExpectedGeneration: &stale})
	if !errors.Is(err, ErrGenerationMismatch) {
		t.Fatalf("err=%v, want ErrGenerationMismatch", err)
	}
	if len(store.promotes) != 0 || svc.Current().Role != domain.TopologyRoleNode || svc.Current().LeadershipGeneration != 4 {
		t.Fatalf("state changed after refused promotion: %+v", svc.Current())
	}
}

func TestPromoteRefusesWritablePrimaryRoles(t *testing.T) {
	for _, role := range []domain.TopologyRole{domain.TopologyRolePrimary, domain.TopologyRolePrimaryWithNodes} {
		svc, store := newTopologyService(t, role, 4)
		if _, err := svc.Promote(context.Background(), PromoteOptions{}); !errors.Is(err, ErrInvalidTransition) {
			t.Fatalf("%s: err=%v, want ErrInvalidTransition", role, err)
		}
		if len(store.promotes) != 0 {
			t.Fatalf("%s: workflow ran", role)
		}
	}
}

func TestPromoteStoreFailureLeavesCacheUntouched(t *testing.T) {
	svc, store := newTopologyService(t, domain.TopologyRoleNode, 4)
	store.err = errors.New("disk full")
	if _, err := svc.Promote(context.Background(), PromoteOptions{}); err == nil {
		t.Fatal("expected store failure")
	}
	if got := svc.Current(); got.Role != domain.TopologyRoleNode || got.LeadershipGeneration != 4 {
		t.Fatalf("cache changed: %+v", got)
	}
}

func TestPromoteStaysAboveLatestKnownGeneration(t *testing.T) {
	svc, store := newTopologyService(t, domain.TopologyRoleStalePrimary, 4)
	store.current.LatestKnownGeneration = 9
	if _, err := svc.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	result, err := svc.Promote(context.Background(), PromoteOptions{})
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if result.Identity.LeadershipGeneration != 10 {
		t.Fatalf("generation=%d, want 10", result.Identity.LeadershipGeneration)
	}
}

func TestDemoteRevokesNodesAndMovesToNode(t *testing.T) {
	svc, store := newTopologyService(t, domain.TopologyRolePrimaryWithNodes, 6)
	result, err := svc.Demote(context.Background(), DemoteOptions{ActorUserID: "owner-1"})
	if err != nil {
		t.Fatalf("Demote: %v", err)
	}
	if result.Identity.Role != domain.TopologyRoleNode || result.Identity.LeadershipGeneration != 6 ||
		result.PreviousRole != domain.TopologyRolePrimaryWithNodes || result.RevokedCredentials != 3 {
		t.Fatalf("result=%+v", result)
	}
	if len(store.demotes) != 1 || store.demotes[0].ActorUserID != "owner-1" {
		t.Fatalf("store input=%+v", store.demotes)
	}
	if svc.Current().Role != domain.TopologyRoleNode || svc.IsWritable() {
		t.Fatalf("cache=%+v", svc.Current())
	}
}

func TestDemoteRefusesNonPrimaryAndPlainStores(t *testing.T) {
	svc, store := newTopologyService(t, domain.TopologyRoleNode, 6)
	if _, err := svc.Demote(context.Background(), DemoteOptions{}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("node demote err=%v", err)
	}
	if len(store.demotes) != 0 {
		t.Fatal("workflow ran for a node")
	}

	plain := newFakeStore()
	plain.seed(Identity{InstallationID: domain.NewInstallationID(), NodeID: domain.NewNodeID(),
		Role: domain.TopologyRolePrimary, LeadershipGeneration: 2, LatestKnownGeneration: 2})
	plainSvc := NewService(plain)
	if _, err := plainSvc.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := plainSvc.Demote(context.Background(), DemoteOptions{}); !errors.Is(err, ErrTopologyWorkflowUnavailable) {
		t.Fatalf("plain store err=%v, want ErrTopologyWorkflowUnavailable", err)
	}
	if plainSvc.Current().Role != domain.TopologyRolePrimary {
		t.Fatal("role changed without a transactional store")
	}
}

func TestDemoteRejectsExpectedGenerationMismatch(t *testing.T) {
	svc, store := newTopologyService(t, domain.TopologyRolePrimary, 6)
	stale := domain.LeadershipGeneration(5)
	if _, err := svc.Demote(context.Background(), DemoteOptions{ExpectedGeneration: &stale}); !errors.Is(err, ErrGenerationMismatch) {
		t.Fatalf("err=%v", err)
	}
	if len(store.demotes) != 0 {
		t.Fatal("workflow ran")
	}
}

func TestStalePrimaryStillBlocksWritesAfterObservingNewerGeneration(t *testing.T) {
	svc, _ := newTopologyService(t, domain.TopologyRolePrimary, 3)
	if _, err := svc.RecordImportedSnapshot(context.Background(), uuid.New(), 4); err != nil {
		t.Fatal(err)
	}
	if !svc.IsStalePrimary() || svc.IsWritable() {
		t.Fatalf("expected stale-primary write block, identity=%+v", svc.Current())
	}
}

func TestPromoteMintsClusterKeyAfterCommit(t *testing.T) {
	svc, store := newTopologyService(t, domain.TopologyRoleNode, 4)
	minted := uuid.New()
	calls := 0
	svc.SetClusterKeyProvisioner(func(context.Context) (uuid.UUID, error) {
		calls++
		return minted, nil
	})
	result, err := svc.Promote(context.Background(), PromoteOptions{})
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if calls != 1 || result.ClusterKeyErr != nil {
		t.Fatalf("provisioner calls=%d err=%v", calls, result.ClusterKeyErr)
	}
	if result.Identity.ClusterKeyID == nil || *result.Identity.ClusterKeyID != minted ||
		svc.Current().ClusterKeyID == nil || *svc.Current().ClusterKeyID != minted {
		t.Fatalf("cluster key not minted: result=%+v cache=%+v", result.Identity, svc.Current())
	}
	if store.current.ClusterKeyID == nil || *store.current.ClusterKeyID != minted {
		t.Fatalf("cluster key not persisted: %+v", store.current)
	}
}

func TestPromoteSurvivesClusterKeyProvisionFailure(t *testing.T) {
	svc, _ := newTopologyService(t, domain.TopologyRoleNode, 4)
	boom := errors.New("keystore unavailable")
	svc.SetClusterKeyProvisioner(func(context.Context) (uuid.UUID, error) { return uuid.Nil, boom })
	result, err := svc.Promote(context.Background(), PromoteOptions{})
	if err != nil {
		t.Fatalf("a key failure must not fail the promote: %v", err)
	}
	if !errors.Is(result.ClusterKeyErr, boom) {
		t.Fatalf("ClusterKeyErr=%v, want %v", result.ClusterKeyErr, boom)
	}
	if got := svc.Current(); got.Role != domain.TopologyRolePrimary || got.LeadershipGeneration != 5 || got.ClusterKeyID != nil {
		t.Fatalf("promote rolled back or key set: %+v", got)
	}
}
