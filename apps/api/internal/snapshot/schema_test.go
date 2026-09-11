package snapshot

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
)

func newValidEnvelope() Envelope {
	primaryID := uuid.New()
	env := Envelope{
		Transient: TransientFields{
			SnapshotVersion:      domain.SnapshotVersionV1,
			ReplicationVersion:   domain.ReplicationVersionV1,
			SourcePrimaryID:      primaryID,
			LeadershipGeneration: 5,
			CapturedAt:           time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		},
		NodeLocal: NodeLocalFields{
			Ingress: domain.Ingress{IPv4: "192.0.2.10"},
			NodeID:  domain.NodeID(uuid.New().String()),
			Role:    domain.TopologyRoleNode,
		},
		Replicated: ReplicatedFields{
			Configuration: map[string]any{
				"settings": domain.Settings{
					RetentionMaxAgeDays: 7,
					RetentionMaxSizeMb:  50,
				},
				"zones": []any{},
			},
			Secrets: []ReplicatedSecret{},
			Owners:  []ReplicatedOwner{},
		},
	}
	hash, err := env.ExpectedHash()
	if err != nil {
		panic(err)
	}
	env.ContentHash = hash
	return env
}

func TestEnvelopeValidateAcceptsValidShape(t *testing.T) {
	env := newValidEnvelope()
	if err := env.Validate(); err != nil {
		t.Errorf("Validate on valid envelope: %v", err)
	}
}

func TestEnvelopeValidateRejectsMissingConfiguration(t *testing.T) {
	env := newValidEnvelope()
	env.Replicated.Configuration = nil
	if err := env.Validate(); err == nil {
		t.Errorf("expected Validate to reject missing configuration")
	}
}

func TestEnvelopeValidateRejectsInvalidRole(t *testing.T) {
	env := newValidEnvelope()
	env.NodeLocal.Role = domain.TopologyRole("not-a-role")
	if err := env.Validate(); err == nil {
		t.Errorf("expected Validate to reject invalid role")
	}
}

func TestEnvelopeValidateRejectsZeroCapturedAt(t *testing.T) {
	env := newValidEnvelope()
	env.Transient.CapturedAt = time.Time{}
	if err := env.Validate(); err == nil {
		t.Errorf("expected Validate to reject zero capturedAt")
	}
}

func TestEnvelopeValidateRejectsInvalidSnapshotVersion(t *testing.T) {
	env := newValidEnvelope()
	env.Transient.SnapshotVersion = 0
	if err := env.Validate(); err == nil {
		t.Errorf("expected Validate to reject snapshotVersion=0")
	}
}

func TestEnvelopeVerifyHashDetectsTampering(t *testing.T) {
	env := newValidEnvelope()
	originalHash := env.ContentHash
	env.Replicated.Owners = append(env.Replicated.Owners, ReplicatedOwner{
		UserID:       uuid.New(),
		Username:     "imposter",
		PasswordHash: "scrypt$16384$8$1$injected$injected",
		Role:         "owner",
	})
	if err := env.VerifyHash(); err == nil {
		t.Errorf("expected VerifyHash to detect tampered replicated fields")
	}
	if env.ContentHash != originalHash {
		// Re-anchoring: the tampered envelope retains the stale hash so the
		// receiver can detect drift; the ExpectedHash() would re-compute.
		originalHash = ""
	}
}

func TestEnvelopeVerifyHashRoundTrip(t *testing.T) {
	env := newValidEnvelope()
	if err := env.VerifyHash(); err != nil {
		t.Errorf("round-trip VerifyHash: %v", err)
	}
}

func TestEnvelopeExpectedHashIsStableAcrossEquivalentInputs(t *testing.T) {
	a := newValidEnvelope()
	b := newValidEnvelope()
	if a.ContentHash == "" || b.ContentHash == "" {
		t.Fatalf("expected non-empty hashes")
	}
	// The content hash covers ReplicatedFields only. Two envelopes with
	// identical ReplicatedFields BUT different Transient and NodeLocal
	// metadata (different source primary ids, captured-at timestamps,
	// node ids) MUST produce the same content hash.
	if a.ContentHash != b.ContentHash {
		t.Errorf("expected stable hash across equivalent replicated payloads; got %s and %s", a.ContentHash, b.ContentHash)
	}
}

func TestEnvelopeExpectedHashIgnoresTransientAndNodeLocal(t *testing.T) {
	env := newValidEnvelope()
	hashBefore, err := env.ExpectedHash()
	if err != nil {
		t.Fatalf("ExpectedHash: %v", err)
	}
	// Mutate transient and node-local fields; the content hash must not
	// change because the hash only covers ReplicatedFields.
	env.Transient.CapturedAt = time.Date(2030, 6, 7, 8, 9, 10, 0, time.UTC)
	env.Transient.SourcePrimaryID = uuid.New()
	env.Transient.LeadershipGeneration = 99
	env.NodeLocal.Ingress = domain.Ingress{IPv4: "198.51.100.5", IPv6: "2001:db8::5"}
	env.NodeLocal.Role = domain.TopologyRolePrimary
	hashAfter, err := env.ExpectedHash()
	if err != nil {
		t.Fatalf("ExpectedHash after mutation: %v", err)
	}
	if hashBefore != hashAfter {
		t.Errorf("ExpectedHash changed after transient/node-local mutation: %s -> %s", hashBefore, hashAfter)
	}
}

func TestEnvelopeExpectedHashChangesWithReplicatedMutation(t *testing.T) {
	env := newValidEnvelope()
	hashBefore, err := env.ExpectedHash()
	if err != nil {
		t.Fatalf("ExpectedHash: %v", err)
	}
	env.Replicated.Secrets = append(env.Replicated.Secrets, ReplicatedSecret{
		ID:       uuid.New(),
		Purpose:  "tls-key",
		Envelope: "v1.kek:AAA:BBB:CCC",
	})
	hashAfter, err := env.ExpectedHash()
	if err != nil {
		t.Fatalf("ExpectedHash after mutation: %v", err)
	}
	if hashBefore == hashAfter {
		t.Errorf("ExpectedHash unchanged after replicated mutation")
	}
}

func TestEnvelopeValidateRejectsBadContentHash(t *testing.T) {
	env := newValidEnvelope()
	env.ContentHash = "deadbeef"
	if err := env.Validate(); err == nil {
		t.Errorf("expected Validate to reject mismatched content hash")
	}
}

func TestEnvelopeValidateRejectsMissingNodeID(t *testing.T) {
	env := newValidEnvelope()
	env.NodeLocal.NodeID = ""
	hash, err := env.ExpectedHash()
	if err == nil {
		env.ContentHash = hash
		if vErr := env.Validate(); vErr == nil {
			t.Errorf("expected Validate to reject missing nodeLocal.nodeId")
		}
	}
}

func TestEnvelopeValidateAcceptsOptionalClusterKeyRef(t *testing.T) {
	env := newValidEnvelope()
	if env.NodeLocal.ClusterKeyRef != nil {
		t.Fatalf("expected ClusterKeyRef to default to nil")
	}
	env.NodeLocal.ClusterKeyRef = func() *uuid.UUID { id := uuid.New(); return &id }()
	hash, err := env.ExpectedHash()
	if err != nil {
		t.Fatalf("ExpectedHash: %v", err)
	}
	env.ContentHash = hash
	if err := env.Validate(); err != nil {
		t.Errorf("Validate with cluster key ref: %v", err)
	}
}
