package enrollment

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/identity"
	replicationsnapshot "github.com/zerkc/ProxyCore/apps/api/internal/snapshot"
	syncapply "github.com/zerkc/ProxyCore/apps/api/internal/sync"
)

type converterArchiveFake struct {
	mu       sync.Mutex
	archives []replicationsnapshot.StandaloneArchive
}

func (f *converterArchiveFake) SaveStandaloneArchive(_ context.Context, archive replicationsnapshot.StandaloneArchive) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.archives = append(f.archives, archive)
	return nil
}

func (f *converterArchiveFake) PurgeExpiredArchives(_ context.Context, _ time.Time) (int, error) {
	return 0, nil
}

type converterEnqueuerFake struct {
	mu   sync.Mutex
	jobs [][]byte
}

func (f *converterEnqueuerFake) CreateApplyJobFromSnapshot(_ context.Context, _ string, body []byte) (uuid.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jobs = append(f.jobs, append([]byte(nil), body...))
	return uuid.New(), nil
}

type converterWaiterFake struct {
	mu       sync.Mutex
	statuses []syncapply.ApplyJobTerminal
	err      error
	calls    int
}

func (f *converterWaiterFake) GetApplyJobTerminal(_ context.Context, _ uuid.UUID) (syncapply.ApplyJobTerminal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return syncapply.ApplyJobTerminal{}, f.err
	}
	if len(f.statuses) == 0 {
		return syncapply.ApplyJobTerminal{Status: "queued"}, nil
	}
	status := f.statuses[0]
	if len(f.statuses) > 1 {
		f.statuses = f.statuses[1:]
	}
	return status, nil
}

type converterBlockingWaiter struct {
	started chan struct{}
	once    sync.Once
}

func (f *converterBlockingWaiter) GetApplyJobTerminal(ctx context.Context, _ uuid.UUID) (syncapply.ApplyJobTerminal, error) {
	f.once.Do(func() { close(f.started) })
	<-ctx.Done()
	return syncapply.ApplyJobTerminal{}, ctx.Err()
}

type converterIdentityStore struct {
	mu      sync.Mutex
	current identity.Identity
}

func (s *converterIdentityStore) Get(context.Context) (identity.Identity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.current, nil
}

func (s *converterIdentityStore) Ensure(_ context.Context, installationID domain.InstallationID, nodeID domain.NodeID) (identity.Identity, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current.InstallationID.IsValid() {
		return s.current, false, nil
	}
	s.current = identity.Identity{InstallationID: installationID, NodeID: nodeID, Role: domain.TopologyRoleStandalone, LeadershipGeneration: 1, LatestKnownGeneration: 1}
	return s.current, true, nil
}

func (s *converterIdentityStore) UpdateRole(_ context.Context, role domain.TopologyRole) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.current.Role = role
	return nil
}

func (s *converterIdentityStore) UpdateLeadershipGeneration(_ context.Context, generation domain.LeadershipGeneration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.current.LeadershipGeneration = generation
	if generation > s.current.LatestKnownGeneration {
		s.current.LatestKnownGeneration = generation
	}
	return nil
}

func (s *converterIdentityStore) UpdateLatestKnownGeneration(_ context.Context, generation domain.LeadershipGeneration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if generation > s.current.LatestKnownGeneration {
		s.current.LatestKnownGeneration = generation
	}
	return nil
}

func (s *converterIdentityStore) UpdateClusterKeyID(_ context.Context, keyID *uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.current.ClusterKeyID = keyID
	return nil
}

func TestNodeConverterHappyPathReturnsPostTransitionLineage(t *testing.T) {
	archive := &converterArchiveFake{}
	enqueuer := &converterEnqueuerFake{}
	service := newConverterIdentityService(t, domain.TopologyRoleStandalone)
	waiter := &converterWaiterFake{statuses: []syncapply.ApplyJobTerminal{{Status: syncapply.ApplyTerminalStatusApplied}}}
	converter := newTestNodeConverter(archive, enqueuer, service, waiter)

	input := NodeConversionInput{
		Envelope:         converterEnvelope(t),
		LocalNodeID:      service.Current().NodeID,
		LocalIngress:     domain.Ingress{IPv4: "198.51.100.20"},
		LocalInstallId:   service.Current().InstallationID,
		ArchiveTTL:       24 * time.Hour,
		ApplyWaitTimeout: time.Second,
	}
	result, err := converter.Convert(context.Background(), input)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if result.ArchiveID == uuid.Nil || result.ApplyJobID == uuid.Nil {
		t.Fatalf("result ids = %+v", result)
	}
	if result.Identity.Role != domain.TopologyRoleNode || service.Current().Role != domain.TopologyRoleNode {
		t.Fatalf("identity result=%+v current=%+v", result.Identity, service.Current())
	}
	if result.NodeLineage.NodeID != service.Current().NodeID || result.NodeLineage.Role != domain.TopologyRoleNode || result.NodeLineage.Generation != uint64(service.Current().LeadershipGeneration) {
		t.Fatalf("lineage=%+v current=%+v", result.NodeLineage, service.Current())
	}
	if len(archive.archives) != 1 || len(enqueuer.jobs) != 1 {
		t.Fatalf("archive count=%d jobs=%d, want one each", len(archive.archives), len(enqueuer.jobs))
	}
}

func TestNodeConverterAbortsOnFailedOrRolledBackApply(t *testing.T) {
	for _, status := range []syncapply.ApplyTerminalStatus{"failed", "rolled-back"} {
		t.Run(string(status), func(t *testing.T) {
			archive := &converterArchiveFake{}
			enqueuer := &converterEnqueuerFake{}
			service := newConverterIdentityService(t, domain.TopologyRoleStandalone)
			converter := newTestNodeConverter(archive, enqueuer, service, &converterWaiterFake{
				statuses: []syncapply.ApplyJobTerminal{{Status: status}},
			})
			_, err := converter.Convert(context.Background(), NodeConversionInput{
				Envelope:    converterEnvelope(t),
				LocalNodeID: service.Current().NodeID,
			})
			if err == nil || !errors.Is(err, ErrNodeConversionAborted) {
				t.Fatalf("error=%v, want ErrNodeConversionAborted", err)
			}
			var aborted *NodeConversionAbortedError
			if !errors.As(err, &aborted) || aborted.Status != status || aborted.ArchiveID == uuid.Nil {
				t.Fatalf("abort error=%T %+v", err, err)
			}
			if service.Current().Role != domain.TopologyRoleStandalone || len(archive.archives) != 1 || len(enqueuer.jobs) != 1 {
				t.Fatalf("role=%s archives=%d jobs=%d", service.Current().Role, len(archive.archives), len(enqueuer.jobs))
			}
		})
	}
}

func TestNodeConverterReturnsWaitTimeoutWithoutTransition(t *testing.T) {
	archive := &converterArchiveFake{}
	enqueuer := &converterEnqueuerFake{}
	service := newConverterIdentityService(t, domain.TopologyRoleStandalone)
	waitErr := &syncapply.ApplyWaitTimeoutError{Last: syncapply.ApplyJobTerminal{Status: "running"}}
	converter := newTestNodeConverter(archive, enqueuer, service, &converterWaiterFake{err: waitErr})
	_, err := converter.Convert(context.Background(), NodeConversionInput{Envelope: converterEnvelope(t), LocalNodeID: service.Current().NodeID})
	if !errors.Is(err, syncapply.ErrApplyWaitTimeout) {
		t.Fatalf("error=%v, want wait timeout", err)
	}
	if service.Current().Role != domain.TopologyRoleStandalone || len(archive.archives) != 1 || len(enqueuer.jobs) != 1 {
		t.Fatalf("role=%s archives=%d jobs=%d", service.Current().Role, len(archive.archives), len(enqueuer.jobs))
	}
}

func TestNodeConverterWrapsIdentityTransitionRefusal(t *testing.T) {
	archive := &converterArchiveFake{}
	enqueuer := &converterEnqueuerFake{}
	// PRIMARY -> NODE is a permitted transition, so the refusal is exercised
	// with a role the state machine does not recognize. The eligibility gate
	// refuses it before anything is archived or applied.
	unknownRole := domain.TopologyRole("unrecognized")
	service := newConverterIdentityService(t, unknownRole)
	converter := newTestNodeConverter(archive, enqueuer, service, &converterWaiterFake{
		statuses: []syncapply.ApplyJobTerminal{{Status: syncapply.ApplyTerminalStatusApplied}},
	})
	_, err := converter.Convert(context.Background(), NodeConversionInput{Envelope: converterEnvelope(t), LocalNodeID: service.Current().NodeID})
	if err == nil || !errors.Is(err, ErrNodeConversionDenied) {
		t.Fatalf("error=%v, want ErrNodeConversionDenied", err)
	}
	if service.Current().Role != unknownRole || len(archive.archives) != 0 || len(enqueuer.jobs) != 0 {
		t.Fatalf("role=%s archives=%d jobs=%d", service.Current().Role, len(archive.archives), len(enqueuer.jobs))
	}
}

func TestNodeConverterCommitsNodeRoleFromPrimaryAndFromDemotedNode(t *testing.T) {
	for _, role := range []domain.TopologyRole{
		domain.TopologyRolePrimary, domain.TopologyRoleStalePrimary, domain.TopologyRoleNode,
	} {
		archive := &converterArchiveFake{}
		enqueuer := &converterEnqueuerFake{}
		service := newConverterIdentityService(t, role)
		converter := newTestNodeConverter(archive, enqueuer, service, &converterWaiterFake{
			statuses: []syncapply.ApplyJobTerminal{{Status: syncapply.ApplyTerminalStatusApplied}},
		})
		result, err := converter.Convert(context.Background(), NodeConversionInput{Envelope: converterEnvelope(t), LocalNodeID: service.Current().NodeID})
		if err != nil {
			t.Fatalf("role=%s: Convert: %v", role, err)
		}
		if result.Identity.Role != domain.TopologyRoleNode || service.Current().Role != domain.TopologyRoleNode {
			t.Fatalf("role=%s: converted role=%s", role, service.Current().Role)
		}
	}
}

func TestNodeConverterRefusesBoundNodeAndPrimaryWithNodesBeforeImport(t *testing.T) {
	key := uuid.New()
	for _, test := range []struct {
		name string
		role domain.TopologyRole
		key  *uuid.UUID
	}{
		{"primary with nodes", domain.TopologyRolePrimaryWithNodes, &key},
		{"bound node", domain.TopologyRoleNode, &key},
	} {
		t.Run(test.name, func(t *testing.T) {
			archive := &converterArchiveFake{}
			enqueuer := &converterEnqueuerFake{}
			service := newConverterIdentityServiceWithKey(t, test.role, test.key)
			converter := newTestNodeConverter(archive, enqueuer, service, &converterWaiterFake{
				statuses: []syncapply.ApplyJobTerminal{{Status: syncapply.ApplyTerminalStatusApplied}},
			})
			_, err := converter.Convert(context.Background(), NodeConversionInput{Envelope: converterEnvelope(t), LocalNodeID: service.Current().NodeID})
			if !errors.Is(err, ErrNodeConversionDenied) {
				t.Fatalf("error=%v, want ErrNodeConversionDenied", err)
			}
			if len(archive.archives) != 0 || len(enqueuer.jobs) != 0 || service.Current().Role != test.role {
				t.Fatalf("archives=%d jobs=%d role=%s", len(archive.archives), len(enqueuer.jobs), service.Current().Role)
			}
		})
	}
}

func TestNodeConverterPersistsImportedClusterKeyWithNodeRole(t *testing.T) {
	archive := &converterArchiveFake{}
	enqueuer := &converterEnqueuerFake{}
	service := newConverterIdentityService(t, domain.TopologyRolePrimary)
	converter := newTestNodeConverter(archive, enqueuer, service, &converterWaiterFake{
		statuses: []syncapply.ApplyJobTerminal{{Status: syncapply.ApplyTerminalStatusApplied}},
	})
	key := uuid.New()
	result, err := converter.Convert(context.Background(), NodeConversionInput{Envelope: converterEnvelope(t), LocalNodeID: service.Current().NodeID, ClusterKeyID: &key})
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	current := service.Current()
	if current.Role != domain.TopologyRoleNode || current.ClusterKeyID == nil || *current.ClusterKeyID != key {
		t.Fatalf("identity=%+v", current)
	}
	// The node adopts the joined primary's generation (envelope generation 9)
	// so a later promotion lands strictly above it.
	if current.LeadershipGeneration != 9 || current.LatestKnownGeneration != 9 || current.IsStalePrimary() {
		t.Fatalf("generation not adopted: %+v", current)
	}
	if result.NodeLineage.ClusterKeyID == nil || *result.NodeLineage.ClusterKeyID != key {
		t.Fatalf("lineage key=%v", result.NodeLineage.ClusterKeyID)
	}
}

func TestNodeConverterReturnsContextCancellationFromWait(t *testing.T) {
	archive := &converterArchiveFake{}
	enqueuer := &converterEnqueuerFake{}
	service := newConverterIdentityService(t, domain.TopologyRoleStandalone)
	waiter := &converterBlockingWaiter{started: make(chan struct{})}
	converter := newTestNodeConverter(archive, enqueuer, service, waiter)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := converter.Convert(ctx, NodeConversionInput{Envelope: converterEnvelope(t), LocalNodeID: service.Current().NodeID})
		result <- err
	}()
	waitForConverterSignal(t, waiter.started)
	cancel()
	if err := waitForConverterResult(t, result); !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v, want context.Canceled", err)
	}
	if service.Current().Role != domain.TopologyRoleStandalone || len(archive.archives) != 1 || len(enqueuer.jobs) != 1 {
		t.Fatalf("role=%s archives=%d jobs=%d", service.Current().Role, len(archive.archives), len(enqueuer.jobs))
	}
}

func newTestNodeConverter(archive *converterArchiveFake, enqueuer *converterEnqueuerFake, service *identity.Service, waiter syncapply.TerminalApplyReader) *NodeConverter {
	return NewNodeConverter(NodeConverterOptions{
		Importer:      replicationsnapshot.NewImporter(archive, enqueuer, func() time.Time { return time.Date(2026, 5, 10, 11, 12, 13, 0, time.UTC) }),
		Archive:       archive,
		Identity:      service,
		ApplyWaiter:   waiter,
		ApplyEnqueuer: enqueuer,
	})
}

func newConverterIdentityService(t *testing.T, role domain.TopologyRole) *identity.Service {
	t.Helper()
	return newConverterIdentityServiceWithKey(t, role, nil)
}

func newConverterIdentityServiceWithKey(t *testing.T, role domain.TopologyRole, key *uuid.UUID) *identity.Service {
	t.Helper()
	store := &converterIdentityStore{current: identity.Identity{
		InstallationID:        domain.NewInstallationID(),
		NodeID:                domain.NewNodeID(),
		Role:                  role,
		LeadershipGeneration:  7,
		LatestKnownGeneration: 7,
		ClusterKeyID:          key,
	}}
	service := identity.NewService(store)
	if _, err := service.Load(context.Background()); err != nil {
		t.Fatalf("identity Load: %v", err)
	}
	return service
}

func converterEnvelope(t *testing.T) *replicationsnapshot.Envelope {
	t.Helper()
	env := &replicationsnapshot.Envelope{
		Transient: replicationsnapshot.TransientFields{
			SnapshotVersion:      domain.SnapshotVersionV1,
			ReplicationVersion:   domain.ReplicationVersionV1,
			SourcePrimaryID:      uuid.New(),
			LeadershipGeneration: 9,
			CapturedAt:           time.Date(2026, 5, 10, 11, 12, 13, 0, time.UTC),
		},
		NodeLocal: replicationsnapshot.NodeLocalFields{
			NodeID: domain.NewNodeID(),
			Role:   domain.TopologyRolePrimary,
		},
		Replicated: replicationsnapshot.ReplicatedFields{
			Configuration: map[string]any{"settings": map[string]any{}},
			Secrets:       []replicationsnapshot.ReplicatedSecret{},
			Owners:        []replicationsnapshot.ReplicatedOwner{},
		},
	}
	var err error
	env.ContentHash, err = env.ExpectedHash()
	if err != nil {
		t.Fatalf("Envelope.ExpectedHash: %v", err)
	}
	return env
}

func waitForConverterSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case <-signal:
	case <-timer.C:
		t.Fatal("converter did not reach apply wait")
	}
}

func waitForConverterResult(t *testing.T, result <-chan error) error {
	t.Helper()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case err := <-result:
		return err
	case <-timer.C:
		t.Fatal("converter did not return after cancellation")
		return nil
	}
}
