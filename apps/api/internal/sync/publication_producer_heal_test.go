package sync

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	replicationsnapshot "github.com/zerkc/ProxyCore/apps/api/internal/snapshot"
)

type healTransactionFake struct {
	producerTransactionFake
	records   []configuration.CanonicalSnapshotRecord
	discarded []string
}

func (f *healTransactionFake) ListRecentCanonicalSnapshots(context.Context, int) ([]configuration.CanonicalSnapshotRecord, error) {
	return f.records, nil
}

func (f *healTransactionFake) DiscardCanonicalSnapshot(_ context.Context, id string) error {
	f.discarded = append(f.discarded, id)
	return nil
}

type healStoreFake struct{ tx *healTransactionFake }

func (f *healStoreFake) ListCanonicalSnapshotCandidates(context.Context) ([]configuration.CanonicalSnapshotApplyRef, error) {
	return nil, nil
}

func (f *healStoreFake) WithCanonicalSnapshot(_ context.Context, work func(configuration.CanonicalSnapshotTransaction) error) error {
	return work(f.tx)
}

// healFixture holds a valid stored snapshot record for the fake primary.
func healFixture(t *testing.T) (*healStoreFake, configuration.CanonicalSnapshotRecord) {
	t.Helper()
	keyID := uuid.New()
	identity := configuration.PrimaryIdentityRecord{
		InstallationID: uuid.NewString(), NodeID: uuid.NewString(), Role: domain.TopologyRolePrimary,
		LeadershipGeneration: 3, LatestKnownGeneration: 3, ClusterKeyID: &keyID,
	}
	at := time.Date(2026, 10, 2, 3, 4, 5, 0, time.UTC)
	source, _ := uuid.Parse(identity.InstallationID)
	env := replicationsnapshot.Envelope{
		Transient:  replicationsnapshot.TransientFields{SnapshotVersion: 1, ReplicationVersion: 1, SourcePrimaryID: source, LeadershipGeneration: 3, CapturedAt: at},
		NodeLocal:  replicationsnapshot.NodeLocalFields{NodeID: domain.NodeID(identity.NodeID), Role: identity.Role, LeadershipGeneration: 3, ClusterKeyRef: &keyID},
		Replicated: replicationsnapshot.ReplicatedFields{Configuration: domain.ConfigurationSnapshot{Zones: []domain.ZoneState{}}},
	}
	env.ContentHash, _ = env.ExpectedHash()
	body, err := replicationsnapshot.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	revisionID, jobID := uuid.NewString(), uuid.NewString()
	record := configuration.CanonicalSnapshotRecord{AppliedSnapshotRecord: configuration.AppliedSnapshotRecord{
		ID: uuid.NewString(), SourcePrimaryID: identity.InstallationID, LeadershipGeneration: 3,
		SnapshotVersion: 1, ReplicationVersion: 1, ContentHash: env.ContentHash, SnapshotBody: body,
		RevisionID: &revisionID, Status: configuration.AppliedSnapshotApplied, ApplyJobID: &jobID, AppliedAt: at,
	}}
	return &healStoreFake{tx: &healTransactionFake{
		producerTransactionFake: producerTransactionFake{identity: identity, materialID: keyID, materialKey: bytes.Repeat([]byte{0x42}, 32)},
		records:                 []configuration.CanonicalSnapshotRecord{record},
	}}, record
}

func TestReconcileLeavesHealthyStoredSnapshotUntouched(t *testing.T) {
	store, _ := healFixture(t)
	healed := 0
	producer := NewCanonicalSnapshotProducer(store, CanonicalSnapshotProducerOptions{OnHeal: func(n int) { healed += n }})
	if _, err := producer.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(store.tx.discarded) != 0 || healed != 0 {
		t.Fatalf("healthy snapshot discarded=%v healed=%d, want none", store.tx.discarded, healed)
	}
}

func TestReconcileDiscardsStoredSnapshotTheValidatorRejects(t *testing.T) {
	store, record := healFixture(t)
	stale := record
	stale.ContentHash = "stale-hash-that-no-longer-matches-the-body"
	foreign := record
	foreign.ID = uuid.NewString()
	foreign.SourcePrimaryID = uuid.NewString()
	foreign.ContentHash = "foreign"
	store.tx.records = []configuration.CanonicalSnapshotRecord{record, stale, foreign}
	// Give the stale row its own id so the assertion is unambiguous.
	store.tx.records[1].ID = uuid.NewString()
	staleID := store.tx.records[1].ID

	healed := 0
	producer := NewCanonicalSnapshotProducer(store, CanonicalSnapshotProducerOptions{OnHeal: func(n int) { healed += n }})
	if _, err := producer.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(store.tx.discarded) != 1 || store.tx.discarded[0] != staleID || healed != 1 {
		t.Fatalf("discarded=%v healed=%d, want only %s", store.tx.discarded, healed, staleID)
	}
}

func TestProduceRegeneratesFromJobWhenStoredSnapshotIsDiscarded(t *testing.T) {
	store, record := healFixture(t)
	finishedAt := record.AppliedAt
	discardedAt := finishedAt.Add(time.Minute)
	record.DiscardedAt = &discardedAt
	desiredBody := []byte(`{"settings":{},"zones":[],"streams":[],"certificates":[]}`)
	store.tx.existing = &record
	store.tx.apply = configuration.CanonicalSnapshotApply{
		JobID: *record.ApplyJobID, RevisionID: *record.RevisionID, DesiredSnapshot: desiredBody,
		Status: "applied", Target: "combined", Source: configuration.PersistenceSourceOrdinary,
		RevisionSource: configuration.PersistenceSourceOrdinary, FinishedAt: &finishedAt, RevisionAppliedAt: &finishedAt,
	}
	producer := NewCanonicalSnapshotProducer(store, CanonicalSnapshotProducerOptions{
		ExporterFactory: func(got domain.ConfigurationSnapshot, _ configuration.CanonicalSnapshotTransaction) CanonicalSnapshotExporter {
			return producerExporterFunc(func(_ context.Context, input replicationsnapshot.ExporterInput) (replicationsnapshot.Envelope, error) {
				source, _ := uuid.Parse(input.InstallationID.String())
				env := replicationsnapshot.Envelope{
					Transient:  replicationsnapshot.TransientFields{SnapshotVersion: 1, ReplicationVersion: 1, SourcePrimaryID: source, LeadershipGeneration: input.LeadershipGeneration, CapturedAt: finishedAt},
					NodeLocal:  replicationsnapshot.NodeLocalFields{NodeID: input.NodeID, Role: input.Role, LeadershipGeneration: input.LeadershipGeneration, ClusterKeyRef: input.ClusterKeyRef},
					Replicated: replicationsnapshot.ReplicatedFields{Configuration: got},
				}
				env.ContentHash, _ = env.ExpectedHash()
				return env, nil
			})
		},
	})
	if err := producer.Produce(context.Background(), *record.ApplyJobID); err != nil {
		t.Fatalf("Produce over discarded snapshot: %v", err)
	}
	if store.tx.published != 1 {
		t.Fatalf("published = %d, want one regenerated snapshot", store.tx.published)
	}
}
