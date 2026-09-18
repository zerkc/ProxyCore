package sync

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	replicationsnapshot "github.com/zerkc/ProxyCore/apps/api/internal/snapshot"
)

func canonicalPublicationEnvelope(
	t *testing.T,
	primaryID domain.InstallationID,
	nodeID domain.NodeID,
	role domain.TopologyRole,
	generation domain.LeadershipGeneration,
	revision int,
	clusterKeyID *uuid.UUID,
) ([]byte, string) {
	t.Helper()
	source, err := uuid.Parse(primaryID.String())
	if err != nil {
		t.Fatalf("parse primary id: %v", err)
	}
	envelope := replicationsnapshot.Envelope{
		Transient: replicationsnapshot.TransientFields{
			SnapshotVersion:      domain.SnapshotVersionV1,
			ReplicationVersion:   domain.ReplicationVersionV1,
			SourcePrimaryID:      source,
			LeadershipGeneration: generation,
			CapturedAt:           time.Date(2026, 10, 2, 3, 4, 5, 0, time.UTC),
		},
		NodeLocal: replicationsnapshot.NodeLocalFields{
			NodeID:               nodeID,
			Role:                 role,
			LeadershipGeneration: generation,
			ClusterKeyRef:        clusterKeyID,
		},
		Replicated: replicationsnapshot.ReplicatedFields{
			Configuration: map[string]any{
				"revision":     revision,
				"settings":     map[string]any{"retentionMaxAgeDays": 7, "retentionMaxSizeMb": 50},
				"zones":        []any{},
				"streams":      []any{},
				"certificates": []any{},
			},
			Secrets: []replicationsnapshot.ReplicatedSecret{},
			Owners:  []replicationsnapshot.ReplicatedOwner{},
		},
	}
	hash, err := envelope.ExpectedHash()
	if err != nil {
		t.Fatalf("expected envelope hash: %v", err)
	}
	envelope.ContentHash = hash
	body, err := replicationsnapshot.Marshal(envelope)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return body, hash
}

func mutatePublicationEnvelope(t *testing.T, body []byte, mutate func(*replicationsnapshot.Envelope)) ([]byte, string) {
	t.Helper()
	envelope, err := replicationsnapshot.Unmarshal(body)
	if err != nil {
		t.Fatalf("unmarshal fixture envelope: %v", err)
	}
	mutate(&envelope)
	body, err = replicationsnapshot.Marshal(envelope)
	if err != nil {
		t.Fatalf("marshal mutated envelope: %v", err)
	}
	return body, envelope.ContentHash
}
