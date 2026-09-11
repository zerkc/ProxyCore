package identity

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
)

// fakeStore is an in-memory Store for unit tests. It mimics the singleton
// behavior of the Postgres-backed PgStore and the same concurrency guard
// semantics.
type fakeStore struct {
	mu      sync.Mutex
	current Identity
	present bool
}

func newFakeStore() *fakeStore {
	return &fakeStore{}
}

// seed primes the fake store with the given identity so tests can simulate
// an installation that has already booted or been observed by another node.
func (f *fakeStore) seed(id Identity) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.current = id
	f.present = true
}

func (f *fakeStore) Get(_ context.Context) (Identity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.present {
		return Identity{}, ErrNotFound
	}
	return f.current, nil
}

func (f *fakeStore) Ensure(_ context.Context, installationID domain.InstallationID, nodeID domain.NodeID) (Identity, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.present {
		return f.current, false, nil
	}
	id := Identity{
		InstallationID:        installationID,
		NodeID:                nodeID,
		Role:                  domain.TopologyRoleStandalone,
		LeadershipGeneration:  1,
		LatestKnownGeneration: 1,
	}
	f.current = id
	f.present = true
	return id, true, nil
}

func (f *fakeStore) UpdateRole(_ context.Context, role domain.TopologyRole) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.present {
		return ErrNotFound
	}
	if !role.IsValid() {
		return errors.New("invalid role")
	}
	f.current.Role = role
	return nil
}

func (f *fakeStore) UpdateLeadershipGeneration(_ context.Context, gen domain.LeadershipGeneration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.present {
		return ErrNotFound
	}
	f.current.LeadershipGeneration = gen
	if gen > f.current.LatestKnownGeneration {
		f.current.LatestKnownGeneration = gen
	}
	return nil
}

func (f *fakeStore) UpdateLatestKnownGeneration(_ context.Context, gen domain.LeadershipGeneration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.present {
		return ErrNotFound
	}
	if gen > f.current.LatestKnownGeneration {
		f.current.LatestKnownGeneration = gen
	}
	return nil
}

func (f *fakeStore) UpdateClusterKeyID(_ context.Context, keyID *uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.present {
		return ErrNotFound
	}
	f.current.ClusterKeyID = keyID
	return nil
}

func TestServiceEnsureBootstrappedFresh(t *testing.T) {
	store := newFakeStore()
	svc := NewService(store)
	id, created, err := svc.EnsureBootstrapped(context.Background())
	if err != nil {
		t.Fatalf("EnsureBootstrapped: %v", err)
	}
	if !created {
		t.Errorf("expected created=true on first boot")
	}
	if !id.InstallationID.IsValid() {
		t.Errorf("expected valid installation id, got %q", id.InstallationID)
	}
	if !id.NodeID.IsValid() {
		t.Errorf("expected valid node id, got %q", id.NodeID)
	}
	if id.Role != domain.TopologyRoleStandalone {
		t.Errorf("expected standalone-primary, got %s", id.Role)
	}
	if id.LeadershipGeneration != 1 || id.LatestKnownGeneration != 1 {
		t.Errorf("expected generation 1/1, got %d/%d", id.LeadershipGeneration, id.LatestKnownGeneration)
	}
}

func TestServiceEnsureBootstrappedRestart(t *testing.T) {
	store := newFakeStore()
	svc := NewService(store)
	first, _, err := svc.EnsureBootstrapped(context.Background())
	if err != nil {
		t.Fatalf("first EnsureBootstrapped: %v", err)
	}
	second, created, err := svc.EnsureBootstrapped(context.Background())
	if err != nil {
		t.Fatalf("second EnsureBootstrapped: %v", err)
	}
	if created {
		t.Errorf("expected created=false on restart, got true")
	}
	if first.InstallationID != second.InstallationID {
		t.Errorf("installation id changed across restart: %s -> %s", first.InstallationID, second.InstallationID)
	}
}

func TestServiceEnsureBootstrappedConcurrent(t *testing.T) {
	store := newFakeStore()
	svcA := NewService(store)
	svcB := NewService(store)
	type result struct {
		id      Identity
		created bool
		err     error
	}
	results := make(chan result, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		id, created, err := svcA.EnsureBootstrapped(context.Background())
		results <- result{id, created, err}
	}()
	go func() {
		defer wg.Done()
		id, created, err := svcB.EnsureBootstrapped(context.Background())
		results <- result{id, created, err}
	}()
	wg.Wait()
	close(results)

	var seen []Identity
	for r := range results {
		if r.err != nil {
			t.Fatalf("concurrent EnsureBootstrapped: %v", r.err)
		}
		seen = append(seen, r.id)
	}
	if seen[0].InstallationID != seen[1].InstallationID {
		t.Errorf("expected the same installation id across concurrent bootstraps, got %s and %s", seen[0].InstallationID, seen[1].InstallationID)
	}
}

func TestServiceLoadMissingReturnsErrNotFound(t *testing.T) {
	store := newFakeStore()
	svc := NewService(store)
	_, err := svc.Load(context.Background())
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestServiceIsStalePrimaryDetectsOlderGeneration(t *testing.T) {
	store := newFakeStore()
	store.seed(Identity{
		InstallationID:        domain.NewInstallationID(),
		NodeID:                domain.NewNodeID(),
		Role:                  domain.TopologyRoleStalePrimary,
		LeadershipGeneration:  3,
		LatestKnownGeneration: 7,
	})
	svc := NewService(store)
	if _, err := svc.Load(context.Background()); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !svc.IsStalePrimary() {
		t.Errorf("expected IsStalePrimary=true when role=stale-primary")
	}
	if svc.IsWritable() {
		t.Errorf("expected IsWritable=false when stale-primary")
	}
}

func TestServiceIsWritableForStandaloneAndPrimary(t *testing.T) {
	cases := []struct {
		role     domain.TopologyRole
		writable bool
	}{
		{domain.TopologyRoleStandalone, true},
		{domain.TopologyRolePrimary, true},
		{domain.TopologyRolePrimaryWithNodes, true},
		{domain.TopologyRoleNode, false},
		{domain.TopologyRoleStalePrimary, false},
	}
	for _, tc := range cases {
		store := newFakeStore()
		store.seed(Identity{
			InstallationID: domain.NewInstallationID(),
			NodeID:         domain.NewNodeID(),
			Role:           tc.role,
		})
		svc := NewService(store)
		if _, err := svc.Load(context.Background()); err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got := svc.IsWritable(); got != tc.writable {
			t.Errorf("role=%s IsWritable=%v want %v", tc.role, got, tc.writable)
		}
	}
}

func TestServiceRecordImportedSnapshotMovesToStalePrimary(t *testing.T) {
	store := newFakeStore()
	svc := NewService(store)
	if _, _, err := svc.EnsureBootstrapped(context.Background()); err != nil {
		t.Fatalf("EnsureBootstrapped: %v", err)
	}
	id, err := svc.RecordImportedSnapshot(context.Background(), uuid.New(), 10)
	if err != nil {
		t.Fatalf("RecordImportedSnapshot: %v", err)
	}
	if id.Role != domain.TopologyRoleStalePrimary {
		t.Errorf("expected stale-primary after observing newer generation, got %s", id.Role)
	}
	if id.LatestKnownGeneration != 10 {
		t.Errorf("expected latest_known_generation=10, got %d", id.LatestKnownGeneration)
	}
	if !svc.IsStalePrimary() {
		t.Errorf("expected IsStalePrimary=true after recording newer snapshot")
	}
}

func TestServiceRecordImportedSnapshotDoesNotDowngrade(t *testing.T) {
	store := newFakeStore()
	svc := NewService(store)
	if _, _, err := svc.EnsureBootstrapped(context.Background()); err != nil {
		t.Fatalf("EnsureBootstrapped: %v", err)
	}
	if _, err := svc.RecordImportedSnapshot(context.Background(), uuid.New(), 5); err != nil {
		t.Fatalf("first RecordImportedSnapshot: %v", err)
	}
	// Force the role back to standalone so we can prove RecordImportedSnapshot
	// does not move back to stale-primary when observed generation is older.
	if _, err := svc.TransitionTo(context.Background(), domain.TopologyRolePrimary); err != nil {
		t.Fatalf("TransitionTo: %v", err)
	}
	if _, err := svc.RecordImportedSnapshot(context.Background(), uuid.New(), 3); err != nil {
		t.Fatalf("second RecordImportedSnapshot: %v", err)
	}
	id := svc.Current()
	if id.Role != domain.TopologyRolePrimary {
		t.Errorf("expected role to remain primary when observed generation is older, got %s", id.Role)
	}
	if id.LatestKnownGeneration != 5 {
		t.Errorf("expected latest_known_generation to remain 5, got %d", id.LatestKnownGeneration)
	}
}

func TestServicePromoteToPrimaryIncrementsGeneration(t *testing.T) {
	store := newFakeStore()
	svc := NewService(store)
	if _, _, err := svc.EnsureBootstrapped(context.Background()); err != nil {
		t.Fatalf("EnsureBootstrapped: %v", err)
	}
	// Move the local installation to node first; promotion from node is the
	// Phase 4 contract.
	if _, err := svc.TransitionTo(context.Background(), domain.TopologyRoleNode); err != nil {
		t.Fatalf("TransitionTo node: %v", err)
	}
	id, err := svc.PromoteToPrimary(context.Background())
	if err != nil {
		t.Fatalf("PromoteToPrimary: %v", err)
	}
	if id.Role != domain.TopologyRolePrimary {
		t.Errorf("expected role=primary, got %s", id.Role)
	}
	if id.LeadershipGeneration != 2 {
		t.Errorf("expected leadership_generation=2 after promote, got %d", id.LeadershipGeneration)
	}
	if id.LatestKnownGeneration != 2 {
		t.Errorf("expected latest_known_generation=2, got %d", id.LatestKnownGeneration)
	}
}

func TestServiceTransitionRejectsInvalidRole(t *testing.T) {
	store := newFakeStore()
	svc := NewService(store)
	if _, _, err := svc.EnsureBootstrapped(context.Background()); err != nil {
		t.Fatalf("EnsureBootstrapped: %v", err)
	}
	// standalone -> primary is not allowed in the Phase 0 matrix.
	if _, err := svc.TransitionTo(context.Background(), domain.TopologyRolePrimary); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("expected ErrInvalidTransition, got %v", err)
	}
}

func TestServiceCurrentPanicsBeforeBootstrap(t *testing.T) {
	store := newFakeStore()
	svc := NewService(store)
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("expected panic when Current called before bootstrap")
		}
	}()
	_ = svc.Current()
}
