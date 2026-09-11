package snapshot

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/cluster"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
)

// fakeArchive is an in-memory ArchiveStore for tests.
type fakeArchive struct {
	mu        sync.Mutex
	archives  []StandaloneArchive
	saveErr   error
	purgeErr  error
}

func (f *fakeArchive) SaveStandaloneArchive(_ context.Context, archive StandaloneArchive) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.saveErr != nil {
		return f.saveErr
	}
	f.archives = append(f.archives, archive)
	return nil
}

func (f *fakeArchive) PurgeExpiredArchives(_ context.Context, now time.Time) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.purgeErr != nil {
		return 0, f.purgeErr
	}
	kept := f.archives[:0]
	purged := 0
	for _, a := range f.archives {
		if a.ExpiresAt.Before(now) {
			purged++
			continue
		}
		kept = append(kept, a)
	}
	f.archives = kept
	return purged, nil
}

// fakeEnqueuer is an in-memory ApplyJobEnqueuer.
type fakeEnqueuer struct {
	mu   sync.Mutex
	jobs []fakeJob
}

type fakeJob struct {
	id      uuid.UUID
	source  string
	snap    []byte
}

func (f *fakeEnqueuer) CreateApplyJobFromSnapshot(_ context.Context, sourceLabel string, snapshotBytes []byte) (uuid.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := uuid.New()
	f.jobs = append(f.jobs, fakeJob{id: id, source: sourceLabel, snap: append([]byte(nil), snapshotBytes...)})
	return id, nil
}

func TestImportArchivesLocalStateAndEnqueuesApply(t *testing.T) {
	archive := &fakeArchive{}
	enqueuer := &fakeEnqueuer{}
	pinnedNow := time.Date(2026, 5, 10, 11, 12, 13, 0, time.UTC)
	imp := NewImporter(archive, enqueuer, func() time.Time { return pinnedNow })

	kekBytes, err := cluster.GenerateKEK()
	if err != nil {
		t.Fatalf("GenerateKEK: %v", err)
	}
	kek, err := cluster.NewKEK(kekBytes)
	if err != nil {
		t.Fatalf("NewKEK: %v", err)
	}

	// Build a sealed envelope via the exporter path.
	cfg := &fakeConfig{data: domain.ConfigurationSnapshot{
		Settings: domain.Settings{RetentionMaxAgeDays: 7, RetentionMaxSizeMb: 50},
		Zones:    []domain.ZoneState{},
	}}
	exp := NewExporter(cfg, &fakeSecrets{}, &fakeOwners{})
	env, err := exp.Export(context.Background(), ExporterInput{
		InstallationID:       domain.InstallationID(uuid.New().String()),
		NodeID:               domain.NodeID(uuid.New().String()),
		Role:                 domain.TopologyRolePrimary,
		Ingress:              domain.Ingress{IPv4: "192.0.2.10"},
		LeadershipGeneration: 4,
		ClusterKEK:           kek,
	})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}

	in := ImporterInput{
		LocalInstallationID: domain.InstallationID(uuid.New().String()),
		LocalNodeID:         domain.NodeID(uuid.New().String()),
		LocalIngress:        domain.Ingress{IPv4: "198.51.100.5"},
		ArchiveTTL:          30 * 24 * time.Hour,
	}
	result, err := imp.Import(context.Background(), &env, in)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if result.ArchiveID == uuid.Nil {
		t.Errorf("expected non-zero archive id")
	}
	if result.ApplyJobID == uuid.Nil {
		t.Errorf("expected non-zero apply job id")
	}

	if got := len(archive.archives); got != 1 {
		t.Fatalf("expected 1 archive, got %d", got)
	}
	a := archive.archives[0]
	if a.CapturedAt != pinnedNow {
		t.Errorf("archive capturedAt mismatch: %s != %s", a.CapturedAt, pinnedNow)
	}
	expected := pinnedNow.Add(30 * 24 * time.Hour)
	if !a.ExpiresAt.Equal(expected) {
		t.Errorf("archive expiresAt mismatch: %s != %s", a.ExpiresAt, expected)
	}
	if a.Reason != "transition-to-node" {
		t.Errorf("archive reason: %q", a.Reason)
	}

	if got := len(enqueuer.jobs); got != 1 {
		t.Fatalf("expected 1 apply job, got %d", got)
	}
	j := enqueuer.jobs[0]
	if j.source != "import" {
		t.Errorf("expected apply job source 'import', got %q", j.source)
	}
}

func TestImportOverwritesNodeLocalFields(t *testing.T) {
	archive := &fakeArchive{}
	enqueuer := &fakeEnqueuer{}
	imp := NewImporter(archive, enqueuer, nil)

	kekBytes, err := cluster.GenerateKEK()
	if err != nil {
		t.Fatalf("GenerateKEK: %v", err)
	}
	kek, err := cluster.NewKEK(kekBytes)
	if err != nil {
		t.Fatalf("NewKEK: %v", err)
	}
	cfg := &fakeConfig{data: domain.ConfigurationSnapshot{}}
	exp := NewExporter(cfg, &fakeSecrets{}, &fakeOwners{})
	env, err := exp.Export(context.Background(), ExporterInput{
		InstallationID:       domain.InstallationID(uuid.New().String()),
		NodeID:               domain.NodeID(uuid.New().String()),
		Role:                 domain.TopologyRolePrimary,
		Ingress:              domain.Ingress{IPv4: "192.0.2.10"},
		LeadershipGeneration: 2,
		ClusterKEK:           kek,
	})

	in := ImporterInput{
		LocalNodeID:  domain.NodeID(uuid.New().String()),
		LocalIngress: domain.Ingress{IPv4: "203.0.113.7"},
	}
	_, err = imp.Import(context.Background(), &env, in)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if env.NodeLocal.Ingress.IPv4 != "203.0.113.7" {
		t.Errorf("expected ingress to be overwritten locally: got %s", env.NodeLocal.Ingress.IPv4)
	}
	if env.NodeLocal.NodeID != in.LocalNodeID {
		t.Errorf("expected nodeId to be overwritten locally")
	}
}

func TestImportDefaultsArchiveTTLAndReason(t *testing.T) {
	archive := &fakeArchive{}
	enqueuer := &fakeEnqueuer{}
	pinned := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	imp := NewImporter(archive, enqueuer, func() time.Time { return pinned })

	kekBytes, err := cluster.GenerateKEK()
	if err != nil {
		t.Fatalf("GenerateKEK: %v", err)
	}
	kek, err := cluster.NewKEK(kekBytes)
	if err != nil {
		t.Fatalf("NewKEK: %v", err)
	}
	cfg := &fakeConfig{data: domain.ConfigurationSnapshot{}}
	exp := NewExporter(cfg, &fakeSecrets{}, &fakeOwners{})
	env, err := exp.Export(context.Background(), ExporterInput{
		InstallationID:       domain.InstallationID(uuid.New().String()),
		NodeID:               domain.NodeID(uuid.New().String()),
		Role:                 domain.TopologyRolePrimary,
		Ingress:              domain.Ingress{},
		LeadershipGeneration: 1,
		ClusterKEK:           kek,
	})

	in := ImporterInput{
		LocalNodeID:  domain.NodeID(uuid.New().String()),
		LocalIngress: domain.Ingress{},
	}
	if _, err := imp.Import(context.Background(), &env, in); err != nil {
		t.Fatalf("Import: %v", err)
	}
	a := archive.archives[0]
	expected := pinned.Add(ArchiveTTLDefault)
	if !a.ExpiresAt.Equal(expected) {
		t.Errorf("default TTL: %s != %s", a.ExpiresAt, expected)
	}
	if a.Reason != "transition-to-node" {
		t.Errorf("default reason: %q", a.Reason)
	}
}

func TestImportPropagatesArchiveError(t *testing.T) {
	archive := &fakeArchive{saveErr: errors.New("disk full")}
	enqueuer := &fakeEnqueuer{}
	imp := NewImporter(archive, enqueuer, nil)

	kekBytes, err := cluster.GenerateKEK()
	if err != nil {
		t.Fatalf("GenerateKEK: %v", err)
	}
	kek, err := cluster.NewKEK(kekBytes)
	if err != nil {
		t.Fatalf("NewKEK: %v", err)
	}
	cfg := &fakeConfig{data: domain.ConfigurationSnapshot{}}
	exp := NewExporter(cfg, &fakeSecrets{}, &fakeOwners{})
	env, err := exp.Export(context.Background(), ExporterInput{
		InstallationID:       domain.InstallationID(uuid.New().String()),
		NodeID:               domain.NodeID(uuid.New().String()),
		Role:                 domain.TopologyRolePrimary,
		Ingress:              domain.Ingress{},
		LeadershipGeneration: 1,
		ClusterKEK:           kek,
	})
	if _, err := imp.Import(context.Background(), &env, ImporterInput{LocalNodeID: domain.NodeID(uuid.New().String())}); err == nil {
		t.Errorf("expected Import to surface archive error")
	}
}

func TestPurgeExpiredArchives(t *testing.T) {
	archive := &fakeArchive{}
	enqueuer := &fakeEnqueuer{}
	imp := NewImporter(archive, enqueuer, nil)
	_ = imp

	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	archive.archives = []StandaloneArchive{
		{ID: uuid.New(), CapturedAt: now.Add(-31 * 24 * time.Hour), ExpiresAt: now.Add(-1 * 24 * time.Hour)},
		{ID: uuid.New(), CapturedAt: now.Add(-10 * 24 * time.Hour), ExpiresAt: now.Add(20 * 24 * time.Hour)},
		{ID: uuid.New(), CapturedAt: now.Add(-40 * 24 * time.Hour), ExpiresAt: now.Add(-10 * 24 * time.Hour)},
	}
	purged, err := archive.PurgeExpiredArchives(context.Background(), now)
	if err != nil {
		t.Fatalf("PurgeExpiredArchives: %v", err)
	}
	if purged != 2 {
		t.Errorf("expected 2 purged, got %d", purged)
	}
	if got := len(archive.archives); got != 1 {
		t.Errorf("expected 1 remaining, got %d", got)
	}
}
