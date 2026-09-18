package sync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/cluster"
	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	replicationsnapshot "github.com/zerkc/ProxyCore/apps/api/internal/snapshot"
)

var (
	ErrCanonicalSnapshotProducerNotReady = errors.New("canonical snapshot producer not ready")
	ErrCanonicalSnapshotProducerConflict = errors.New("canonical snapshot producer conflict")
)

const (
	defaultCanonicalSnapshotBackoff = time.Second
	maxCanonicalSnapshotBackoff     = time.Minute
)

type CanonicalSnapshotExporter interface {
	Export(context.Context, replicationsnapshot.ExporterInput) (replicationsnapshot.Envelope, error)
}

type CanonicalSnapshotExporterFactory func(
	domain.ConfigurationSnapshot,
	configuration.CanonicalSnapshotTransaction,
) CanonicalSnapshotExporter

type CanonicalSnapshotProducerOptions struct {
	ExporterFactory CanonicalSnapshotExporterFactory
	InitialBackoff  time.Duration
	MaxBackoff      time.Duration
	Wait            func(context.Context, time.Duration) error
	OnError         func(error)
}

type CanonicalSnapshotProducer struct {
	store           configuration.CanonicalSnapshotStore
	exporterFactory CanonicalSnapshotExporterFactory
	initialBackoff  time.Duration
	maxBackoff      time.Duration
	wait            func(context.Context, time.Duration) error
	onError         func(error)
}

func NewCanonicalSnapshotProducer(
	store configuration.CanonicalSnapshotStore,
	opts CanonicalSnapshotProducerOptions,
) *CanonicalSnapshotProducer {
	initial := opts.InitialBackoff
	if initial <= 0 {
		initial = defaultCanonicalSnapshotBackoff
	}
	max := opts.MaxBackoff
	if max <= 0 || max < initial {
		max = maxCanonicalSnapshotBackoff
		if max < initial {
			max = initial
		}
	}
	factory := opts.ExporterFactory
	if factory == nil {
		factory = func(desired domain.ConfigurationSnapshot, tx configuration.CanonicalSnapshotTransaction) CanonicalSnapshotExporter {
			return replicationsnapshot.NewExporter(
				fixedConfigurationSource{snapshot: desired},
				tx,
				tx,
			)
		}
	}
	wait := opts.Wait
	if wait == nil {
		wait = waitCanonicalSnapshotBackoff
	}
	return &CanonicalSnapshotProducer{
		store:           store,
		exporterFactory: factory,
		initialBackoff:  initial,
		maxBackoff:      max,
		wait:            wait,
		onError:         opts.OnError,
	}
}

func (p *CanonicalSnapshotProducer) Produce(ctx context.Context, jobID string) error {
	if p == nil || p.store == nil || ctx == nil || jobID == "" {
		return ErrCanonicalSnapshotProducerNotReady
	}
	return p.store.WithCanonicalSnapshot(ctx, func(tx configuration.CanonicalSnapshotTransaction) error {
		identity, err := tx.LockPrimaryIdentity(ctx)
		if err != nil {
			return err
		}
		if !eligibleCanonicalPrimary(identity) {
			return ErrCanonicalSnapshotProducerNotReady
		}
		apply, err := tx.GetCanonicalSnapshotApply(ctx, jobID)
		if err != nil {
			return err
		}
		if !terminalOrdinaryCombinedApply(apply) {
			return ErrCanonicalSnapshotProducerNotReady
		}
		if identity.ClusterKeyID == nil {
			return ErrCanonicalSnapshotProducerNotReady
		}
		material, err := tx.LoadOrCreateClusterKEK(ctx, identity.ClusterKeyID.String())
		if err != nil {
			return err
		}
		defer material.Destroy()
		keyBytes := material.CopyBytes()
		kek, err := cluster.NewKEK(keyBytes)
		zeroCanonicalBytes(keyBytes)
		if err != nil {
			return err
		}
		defer kek.Destroy()

		existing, err := tx.FindCanonicalSnapshotForJob(ctx, jobID)
		if err != nil {
			return err
		}
		if existing != nil {
			if existing.RevisionID == nil || *existing.RevisionID != apply.RevisionID ||
				existing.ApplyJobID == nil || *existing.ApplyJobID != apply.JobID ||
				apply.FinishedAt == nil || !existing.AppliedAt.Equal(*apply.FinishedAt) {
				return ErrCanonicalSnapshotProducerConflict
			}
			if canonicalPublicationIsValid(ctx, existing.AppliedSnapshotRecord, identity, kek) {
				return nil
			}
			return ErrCanonicalSnapshotProducerConflict
		}

		var desired domain.ConfigurationSnapshot
		if err := json.Unmarshal(apply.DesiredSnapshot, &desired); err != nil {
			return fmt.Errorf("decode desired revision: %w", err)
		}
		exporter := p.exporterFactory(desired, tx)
		if exporter == nil {
			return ErrCanonicalSnapshotProducerNotReady
		}
		env, err := exporter.Export(ctx, replicationsnapshot.ExporterInput{
			InstallationID:       domain.InstallationID(identity.InstallationID),
			NodeID:               domain.NodeID(identity.NodeID),
			Role:                 identity.Role,
			Ingress:              desired.Settings.Ingress,
			LeadershipGeneration: domain.LeadershipGeneration(identity.LeadershipGeneration),
			ClusterKEK:           kek,
			ClusterKeyRef:        identity.ClusterKeyID,
		})
		if err != nil {
			return err
		}
		if err := env.Validate(); err != nil {
			return fmt.Errorf("canonical envelope validation: %w", err)
		}
		if err := replicationsnapshot.CheckCompatibility(env); err != nil {
			return err
		}
		body, err := replicationsnapshot.Marshal(env)
		if err != nil || len(body) == 0 || len(body) > configuration.SnapshotPublicationMaxBytes {
			if err != nil {
				return err
			}
			return ErrCanonicalSnapshotProducerConflict
		}
		if apply.FinishedAt == nil {
			return ErrCanonicalSnapshotProducerNotReady
		}
		return tx.PublishCanonicalSnapshot(ctx, configuration.CanonicalSnapshotWrite{
			ID:                   uuid.NewString(),
			SnapshotBody:         append([]byte(nil), body...),
			SourcePrimaryID:      identity.InstallationID,
			LeadershipGeneration: int64(identity.LeadershipGeneration),
			SnapshotVersion:      int(env.Transient.SnapshotVersion),
			ReplicationVersion:   int(env.Transient.ReplicationVersion),
			ContentHash:          env.ContentHash,
			RevisionID:           apply.RevisionID,
			ApplyJobID:           apply.JobID,
			AppliedAt:            apply.FinishedAt.UTC(),
		})
	})
}

func (p *CanonicalSnapshotProducer) Reconcile(ctx context.Context) (int, error) {
	if p == nil || p.store == nil || ctx == nil {
		return 0, ErrCanonicalSnapshotProducerNotReady
	}
	candidates, err := p.store.ListCanonicalSnapshotCandidates(ctx)
	if err != nil {
		return 0, err
	}
	published := 0
	for _, candidate := range candidates {
		if err := p.Produce(ctx, candidate.JobID); err != nil {
			return published, err
		}
		published++
	}
	return published, nil
}

func (p *CanonicalSnapshotProducer) Run(ctx context.Context) {
	if p == nil || ctx == nil {
		return
	}
	backoff := p.initialBackoff
	for {
		if _, err := p.Reconcile(ctx); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			if p.onError != nil {
				p.onError(err)
			}
			if backoff < p.maxBackoff {
				backoff *= 2
				if backoff > p.maxBackoff {
					backoff = p.maxBackoff
				}
			}
		} else {
			backoff = p.initialBackoff
		}
		if err := p.wait(ctx, backoff); err != nil {
			return
		}
	}
}

func eligibleCanonicalPrimary(identity configuration.PrimaryIdentityRecord) bool {
	return domain.InstallationID(identity.InstallationID).IsValid() &&
		domain.NodeID(identity.NodeID).IsValid() &&
		(identity.Role == domain.TopologyRolePrimary || identity.Role == domain.TopologyRolePrimaryWithNodes) &&
		identity.LeadershipGeneration > 0 &&
		identity.LeadershipGeneration == identity.LatestKnownGeneration &&
		identity.LeadershipGeneration <= uint64(1<<63-1)
}

func terminalOrdinaryCombinedApply(apply configuration.CanonicalSnapshotApply) bool {
	return apply.Status == "applied" && apply.Target == "combined" &&
		apply.Source == configuration.PersistenceSourceOrdinary &&
		apply.RevisionSource == configuration.PersistenceSourceOrdinary &&
		apply.FinishedAt != nil && apply.RevisionAppliedAt != nil &&
		len(apply.DesiredSnapshot) > 0 && apply.RevisionID != "" && apply.JobID != ""
}

func canonicalPublicationIsValid(
	ctx context.Context,
	record configuration.AppliedSnapshotRecord,
	identity configuration.PrimaryIdentityRecord,
	kek *cluster.KEK,
) bool {
	if record.Status != configuration.AppliedSnapshotApplied || record.DiscardedAt != nil ||
		record.SnapshotBody == nil || record.SourcePrimaryID != identity.InstallationID ||
		record.LeadershipGeneration != int64(identity.LeadershipGeneration) ||
		record.AppliedAt.IsZero() {
		return false
	}
	env, err := replicationsnapshot.Unmarshal(record.SnapshotBody)
	if err != nil || env.Validate() != nil || replicationsnapshot.CheckCompatibility(env) != nil ||
		env.ContentHash != record.ContentHash || env.Transient.SnapshotVersion != domain.SnapshotVersion(record.SnapshotVersion) ||
		env.Transient.ReplicationVersion != domain.ReplicationVersion(record.ReplicationVersion) ||
		env.Transient.SourcePrimaryID.String() != identity.InstallationID ||
		env.Transient.LeadershipGeneration != domain.LeadershipGeneration(identity.LeadershipGeneration) ||
		env.NodeLocal.NodeID.String() != identity.NodeID || env.NodeLocal.Role != identity.Role ||
		env.NodeLocal.ClusterKeyRef == nil || identity.ClusterKeyID == nil ||
		*env.NodeLocal.ClusterKeyRef != *identity.ClusterKeyID {
		return false
	}
	return replicationsnapshot.NewValidator(kek).Validate(ctx, env).OK
}

type fixedConfigurationSource struct {
	snapshot domain.ConfigurationSnapshot
}

func (s fixedConfigurationSource) Snapshot(context.Context) (domain.ConfigurationSnapshot, error) {
	return s.snapshot, nil
}

func waitCanonicalSnapshotBackoff(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func zeroCanonicalBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}

var _ configuration.CanonicalSnapshotStore = (*configuration.PgPhase2Store)(nil)
var _ CanonicalSnapshotExporter = (*replicationsnapshot.Exporter)(nil)
