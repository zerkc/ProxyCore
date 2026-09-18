package sync

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	replicationsnapshot "github.com/zerkc/ProxyCore/apps/api/internal/snapshot"
)

func TestCanonicalSnapshotProducerRejectsNonTerminalApply(t *testing.T) {
	identity := configuration.PrimaryIdentityRecord{
		InstallationID:        uuid.NewString(),
		NodeID:                uuid.NewString(),
		Role:                  domain.TopologyRolePrimary,
		LeadershipGeneration:  3,
		LatestKnownGeneration: 3,
	}
	store := &producerStoreFake{
		tx: &producerTransactionFake{
			identity: identity,
			apply: configuration.CanonicalSnapshotApply{
				JobID:          "job-1",
				RevisionID:     uuid.NewString(),
				Status:         "applying",
				Target:         "combined",
				Source:         configuration.PersistenceSourceOrdinary,
				FinishedAt:     nil,
				RevisionSource: configuration.PersistenceSourceOrdinary,
				RevisionAppliedAt: func() *time.Time {
					at := time.Now().UTC()
					return &at
				}(),
				DesiredSnapshot: []byte(`{"settings":{},"zones":[],"streams":[],"certificates":[]}`),
			},
		},
	}
	producer := NewCanonicalSnapshotProducer(store, CanonicalSnapshotProducerOptions{})

	if err := producer.Produce(context.Background(), "job-1"); err != ErrCanonicalSnapshotProducerNotReady {
		t.Fatalf("Produce error = %v, want %v", err, ErrCanonicalSnapshotProducerNotReady)
	}
	if store.tx.published != 0 {
		t.Fatalf("published = %d, want 0", store.tx.published)
	}
}

type producerStoreFake struct {
	tx *producerTransactionFake
}

func (f *producerStoreFake) ListCanonicalSnapshotCandidates(context.Context) ([]configuration.CanonicalSnapshotApplyRef, error) {
	return []configuration.CanonicalSnapshotApplyRef{{JobID: f.tx.apply.JobID}}, nil
}

func (f *producerStoreFake) WithCanonicalSnapshot(_ context.Context, work func(configuration.CanonicalSnapshotTransaction) error) error {
	return work(f.tx)
}

type producerTransactionFake struct {
	identity    configuration.PrimaryIdentityRecord
	apply       configuration.CanonicalSnapshotApply
	materialID  uuid.UUID
	materialKey []byte
	existing    *configuration.CanonicalSnapshotRecord
	published   int
	writes      []configuration.CanonicalSnapshotWrite
}

func (f *producerTransactionFake) LockPrimaryIdentity(context.Context) (configuration.PrimaryIdentityRecord, error) {
	return f.identity, nil
}

func (f *producerTransactionFake) LoadOrCreateClusterKEK(context.Context, string) (configuration.ClusterKeyMaterial, error) {
	if f.materialID != uuid.Nil {
		return configuration.NewClusterKeyMaterial(f.materialID.String(), f.materialKey)
	}
	return configuration.NewClusterKeyMaterial(uuid.NewString(), make([]byte, 32))
}

func (f *producerTransactionFake) GetCanonicalSnapshotApply(context.Context, string) (configuration.CanonicalSnapshotApply, error) {
	return f.apply, nil
}

func (f *producerTransactionFake) FindCanonicalSnapshotForJob(context.Context, string) (*configuration.CanonicalSnapshotRecord, error) {
	return f.existing, nil
}

func (f *producerTransactionFake) ListSecrets(context.Context) ([]replicationsnapshot.PlainSecret, error) {
	return nil, nil
}

func (f *producerTransactionFake) ListReplicableOwners(context.Context) ([]replicationsnapshot.ReplicableOwner, error) {
	return nil, nil
}

func (f *producerTransactionFake) PublishCanonicalSnapshot(_ context.Context, write configuration.CanonicalSnapshotWrite) error {
	f.published++
	f.writes = append(f.writes, write)
	return nil
}

func TestCanonicalSnapshotProducerStoresExactEnvelopeBytesAndPreservesRevision(t *testing.T) {
	clusterKeyID := uuid.New()
	identity := configuration.PrimaryIdentityRecord{
		InstallationID:        uuid.NewString(),
		NodeID:                uuid.NewString(),
		Role:                  domain.TopologyRolePrimary,
		LeadershipGeneration:  3,
		LatestKnownGeneration: 3,
		ClusterKeyID:          &clusterKeyID,
	}
	finishedAt := time.Date(2026, 10, 2, 3, 4, 5, 0, time.UTC)
	desired := domain.ConfigurationSnapshot{Settings: domain.Settings{Ingress: domain.Ingress{IPv4: "192.0.2.10"}}, Zones: []domain.ZoneState{}}
	desiredBody, err := json.Marshal(desired)
	if err != nil {
		t.Fatalf("marshal desired: %v", err)
	}
	key := bytes.Repeat([]byte{0x42}, 32)
	store := &producerStoreFake{tx: &producerTransactionFake{
		identity:    identity,
		materialID:  clusterKeyID,
		materialKey: key,
		apply: configuration.CanonicalSnapshotApply{
			JobID: "00000000-0000-4000-8000-000000000101", RevisionID: "00000000-0000-4000-8000-000000000102",
			DesiredSnapshot: desiredBody, Status: "applied", Target: "combined", Source: configuration.PersistenceSourceOrdinary,
			RevisionSource: configuration.PersistenceSourceOrdinary, FinishedAt: &finishedAt, RevisionAppliedAt: &finishedAt,
		},
	}}
	var exported replicationsnapshot.Envelope
	exportCalls := 0
	producer := NewCanonicalSnapshotProducer(store, CanonicalSnapshotProducerOptions{
		ExporterFactory: func(got domain.ConfigurationSnapshot, _ configuration.CanonicalSnapshotTransaction) CanonicalSnapshotExporter {
			if got.Settings.Ingress.IPv4 != desired.Settings.Ingress.IPv4 {
				t.Fatalf("exporter desired ingress=%q, want %q", got.Settings.Ingress.IPv4, desired.Settings.Ingress.IPv4)
			}
			exportCalls++
			return producerExporterFunc(func(_ context.Context, input replicationsnapshot.ExporterInput) (replicationsnapshot.Envelope, error) {
				source, _ := uuid.Parse(input.InstallationID.String())
				exported = replicationsnapshot.Envelope{
					Transient:  replicationsnapshot.TransientFields{SnapshotVersion: 1, ReplicationVersion: 1, SourcePrimaryID: source, LeadershipGeneration: input.LeadershipGeneration, CapturedAt: finishedAt},
					NodeLocal:  replicationsnapshot.NodeLocalFields{Ingress: input.Ingress, NodeID: input.NodeID, Role: input.Role, LeadershipGeneration: input.LeadershipGeneration, ClusterKeyRef: input.ClusterKeyRef},
					Replicated: replicationsnapshot.ReplicatedFields{Configuration: got},
				}
				exported.ContentHash, _ = exported.ExpectedHash()
				return exported, nil
			})
		},
	})
	if err := producer.Produce(context.Background(), store.tx.apply.JobID); err != nil {
		t.Fatalf("Produce: %v", err)
	}
	if store.tx.published != 1 || len(store.tx.writes) != 1 {
		t.Fatalf("published=%d writes=%d, want one", store.tx.published, len(store.tx.writes))
	}
	wantBody, err := replicationsnapshot.Marshal(exported)
	if err != nil {
		t.Fatalf("marshal exported envelope: %v", err)
	}
	if !bytes.Equal(store.tx.writes[0].SnapshotBody, wantBody) {
		t.Fatal("producer did not persist exact snapshot.Marshal bytes")
	}
	if !bytes.Equal(store.tx.apply.DesiredSnapshot, desiredBody) {
		t.Fatal("producer mutated the revision desired-config body")
	}
	if store.tx.writes[0].SourcePrimaryID != identity.InstallationID ||
		store.tx.writes[0].LeadershipGeneration != int64(identity.LeadershipGeneration) ||
		store.tx.writes[0].RevisionID != store.tx.apply.RevisionID ||
		store.tx.writes[0].ApplyJobID != store.tx.apply.JobID ||
		!store.tx.writes[0].AppliedAt.Equal(finishedAt) {
		t.Fatalf("proof tuple=%+v", store.tx.writes[0])
	}
	bodyCopy := append([]byte(nil), store.tx.writes[0].SnapshotBody...)
	revisionID := store.tx.apply.RevisionID
	jobID := store.tx.apply.JobID
	store.tx.existing = &configuration.CanonicalSnapshotRecord{AppliedSnapshotRecord: configuration.AppliedSnapshotRecord{
		ID: uuid.NewString(), SourcePrimaryID: identity.InstallationID, LeadershipGeneration: int64(identity.LeadershipGeneration),
		SnapshotVersion: 1, ReplicationVersion: 1, ContentHash: exported.ContentHash, SnapshotBody: bodyCopy,
		RevisionID: &revisionID, Status: configuration.AppliedSnapshotApplied, ApplyJobID: &jobID, AppliedAt: finishedAt,
	}}
	if err := producer.Produce(context.Background(), jobID); err != nil {
		t.Fatalf("idempotent Produce: %v", err)
	}
	if store.tx.published != 1 || exportCalls != 1 {
		t.Fatalf("idempotent published=%d exportCalls=%d, want 1 and 1", store.tx.published, exportCalls)
	}
}

type producerExporterFunc func(context.Context, replicationsnapshot.ExporterInput) (replicationsnapshot.Envelope, error)

func (f producerExporterFunc) Export(ctx context.Context, input replicationsnapshot.ExporterInput) (replicationsnapshot.Envelope, error) {
	return f(ctx, input)
}
