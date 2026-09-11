package snapshot

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/cluster"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
)

// ConfigurationSource is the read-only port the Exporter uses to pull the
// desired state from the local configuration store.
type ConfigurationSource interface {
	Snapshot(ctx context.Context) (domain.ConfigurationSnapshot, error)
}

// SecretLister returns the local secrets that the Exporter must wrap with
// the cluster KEK. The list is intentionally typed here rather than reusing
// the secrets.Store interface because Phase 1 round-trip tests provide a
// deterministic fake without dragging in PostgreSQL.
type SecretLister interface {
	ListSecrets(ctx context.Context) ([]PlainSecret, error)
}

// PlainSecret is the exporter-facing representation of a locally stored
// secret. The Exporter wraps the Value with the cluster KEK and produces a
// ReplicatedSecret whose envelope is the cluster-KEK ciphertext.
type PlainSecret struct {
	ID     uuid.UUID
	Purpose string
	Value  []byte
}

// OwnerLister returns the admin identities (Owners and Operators) that the
// Exporter must replicate so a promoted node can administer the cluster
// without re-bootstrapping. Only the password hash and metadata are
// replicated; plaintext is never included.
type OwnerLister interface {
	ListReplicableOwners(ctx context.Context) ([]ReplicableOwner, error)
}

// ReplicableOwner is the exporter-facing shape of an admin identity.
// PasswordHash is the scrypt-encoded value stored locally; the Exporter
// passes it through unchanged.
type ReplicableOwner struct {
	UserID       uuid.UUID
	Username     string
	PasswordHash string
	Role         string
}

// ExporterInput carries the producer-side identity fields the Exporter
// stamps onto every emitted envelope. Callers pass the local installation
// identity (the producer is the local installation acting as primary) and
// the active cluster KEK.
type ExporterInput struct {
	InstallationID        domain.InstallationID
	NodeID                domain.NodeID
	Role                  domain.TopologyRole
	Ingress               domain.Ingress
	LeadershipGeneration  domain.LeadershipGeneration
	ClusterKEK            *cluster.KEK
	ClusterKeyRef         *uuid.UUID
}

// Exporter turns a desired-state snapshot into a sealed replication
// envelope. The same envelope is the unit consumed by the Phase 1
// validator and importer; the Envelope.Validate step is a precondition for
// the returned value to be considered publishable.
type Exporter struct {
	config  ConfigurationSource
	secrets SecretLister
	owners  OwnerLister
}

// NewExporter constructs an Exporter over the given sources. The cluster KEK
// is passed per-call so the same Exporter can publish from installations
// holding different cluster keys (Phase 4 promotes the role).
func NewExporter(config ConfigurationSource, secrets SecretLister, owners OwnerLister) *Exporter {
	return &Exporter{config: config, secrets: secrets, owners: owners}
}

// Export produces a sealed Envelope. The exporter:
//   1. Reads the desired configuration snapshot via ConfigurationSource.
//   2. Lists local secrets and wraps each value with the cluster KEK.
//   3. Lists admin identities and includes the hashes unchanged.
//   4. Stamps the producer's identity into Transient and NodeLocal fields.
//   5. Computes the content hash and seals the envelope.
func (e *Exporter) Export(ctx context.Context, in ExporterInput) (Envelope, error) {
	if in.ClusterKEK == nil {
		return Envelope{}, errors.New("exporter: cluster KEK is required")
	}
	if !in.InstallationID.IsValid() {
		return Envelope{}, errors.New("exporter: installation id is invalid")
	}
	if !in.NodeID.IsValid() {
		return Envelope{}, errors.New("exporter: node id is invalid")
	}
	if !in.Role.IsValid() {
		return Envelope{}, errors.New("exporter: role is invalid")
	}

	configuration, err := e.config.Snapshot(ctx)
	if err != nil {
		return Envelope{}, fmt.Errorf("exporter: read configuration: %w", err)
	}

	plainSecrets, err := e.secrets.ListSecrets(ctx)
	if err != nil {
		return Envelope{}, fmt.Errorf("exporter: list secrets: %w", err)
	}
	replicatedSecrets := make([]ReplicatedSecret, 0, len(plainSecrets))
	for _, s := range plainSecrets {
		envelope, err := in.ClusterKEK.Wrap(s.Value)
		if err != nil {
			return Envelope{}, fmt.Errorf("exporter: wrap secret %s: %w", s.ID, err)
		}
		replicatedSecrets = append(replicatedSecrets, ReplicatedSecret{
			ID:       s.ID,
			Purpose:  s.Purpose,
			Envelope: envelope,
		})
	}

	owners, err := e.owners.ListReplicableOwners(ctx)
	if err != nil {
		return Envelope{}, fmt.Errorf("exporter: list owners: %w", err)
	}
	replicatedOwners := make([]ReplicatedOwner, 0, len(owners))
	for _, owner := range owners {
		replicatedOwners = append(replicatedOwners, ReplicatedOwner{
			UserID:       owner.UserID,
			Username:     owner.Username,
			PasswordHash: owner.PasswordHash,
			Role:         owner.Role,
		})
	}

	now := time.Now().UTC()
	env := Envelope{
		Transient: TransientFields{
			SnapshotVersion:      domain.SnapshotVersionV1,
			ReplicationVersion:   domain.ReplicationVersionV1,
			SourcePrimaryID:      uuid.MustParse(string(in.InstallationID)),
			LeadershipGeneration: in.LeadershipGeneration,
			CapturedAt:           now,
		},
		NodeLocal: NodeLocalFields{
			Ingress:             in.Ingress,
			NodeID:              in.NodeID,
			Role:                in.Role,
			LeadershipGeneration: in.LeadershipGeneration,
			ClusterKeyRef:       in.ClusterKeyRef,
		},
		Replicated: ReplicatedFields{
			Configuration: configuration,
			Secrets:       replicatedSecrets,
			Owners:        replicatedOwners,
		},
	}
	hash, err := env.ExpectedHash()
	if err != nil {
		return Envelope{}, fmt.Errorf("exporter: compute content hash: %w", err)
	}
	env.ContentHash = hash
	return env, nil
}
