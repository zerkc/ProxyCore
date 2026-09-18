package configuration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/snapshot"
)

var _ snapshot.ApplyJobEnqueuer = (*Store)(nil)
var _ snapshot.ApplyJobEnqueuer = (*PgPhase2Store)(nil)
var _ snapshot.ArchiveStore = (*Store)(nil)
var _ snapshot.ArchiveStore = (*PgPhase2Store)(nil)

type importTerminalReader interface {
	GetApplyJobTerminal(context.Context, uuid.UUID) (ApplyJobTerminal, error)
}

var _ importTerminalReader = (*Store)(nil)
var _ importTerminalReader = (*PgPhase2Store)(nil)

func TestImportedSnapshotMetadataUsesEnvelopeFieldsAndRawChecksum(t *testing.T) {
	env := importTestEnvelope(t)
	body, err := snapshot.Marshal(env)
	if err != nil {
		t.Fatalf("snapshot.Marshal: %v", err)
	}
	metadata, err := importedSnapshotMetadata(body)
	if err != nil {
		t.Fatalf("importedSnapshotMetadata: %v", err)
	}
	sum := sha256.Sum256(body)
	if metadata.sourcePrimaryID != env.Transient.SourcePrimaryID || metadata.sourceNodeID != uuid.MustParse(env.NodeLocal.NodeID.String()) {
		t.Fatalf("source metadata=%+v, want primary=%s node=%s", metadata, env.Transient.SourcePrimaryID, env.NodeLocal.NodeID)
	}
	if metadata.snapshotHash != hex.EncodeToString(sum[:]) || metadata.contentHash != env.ContentHash || metadata.snapshotVersion != int(env.Transient.SnapshotVersion) ||
		metadata.replicationVersion != int(env.Transient.ReplicationVersion) || metadata.leadershipGeneration != int64(env.Transient.LeadershipGeneration) {
		t.Fatalf("metadata=%+v, want raw checksum and transient versions", metadata)
	}
}

func TestImportedSnapshotMetadataRejectsMalformedEnvelopeBytes(t *testing.T) {
	if _, err := importedSnapshotMetadata([]byte("not-json")); err == nil {
		t.Fatal("malformed envelope unexpectedly accepted")
	}
}

func importTestEnvelope(t *testing.T) snapshot.Envelope {
	t.Helper()
	env := snapshot.Envelope{
		Transient: snapshot.TransientFields{
			SnapshotVersion:      domain.SnapshotVersionV1,
			ReplicationVersion:   domain.ReplicationVersionV1,
			SourcePrimaryID:      uuid.New(),
			LeadershipGeneration: domain.LeadershipGeneration(7),
			CapturedAt:           time.Date(2026, 5, 10, 11, 12, 13, 0, time.UTC),
		},
		NodeLocal: snapshot.NodeLocalFields{
			Ingress: domain.Ingress{IPv4: "198.51.100.10"},
			NodeID:  domain.NewNodeID(),
			Role:    domain.TopologyRoleNode,
		},
		Replicated: snapshot.ReplicatedFields{
			Configuration: map[string]any{"settings": map[string]any{}},
			Secrets:       []snapshot.ReplicatedSecret{},
			Owners:        []snapshot.ReplicatedOwner{},
		},
	}
	var err error
	env.ContentHash, err = env.ExpectedHash()
	if err != nil {
		t.Fatalf("Envelope.ExpectedHash: %v", err)
	}
	return env
}

func TestImportSnapshotMetadataCopiesNoCallerBytes(t *testing.T) {
	env := importTestEnvelope(t)
	body, err := snapshot.Marshal(env)
	if err != nil {
		t.Fatalf("snapshot.Marshal: %v", err)
	}
	copyBefore := append([]byte(nil), body...)
	if _, err := importedSnapshotMetadata(body); err != nil {
		t.Fatalf("importedSnapshotMetadata: %v", err)
	}
	if !bytes.Equal(body, copyBefore) {
		t.Fatal("metadata parsing mutated caller bytes")
	}
}
