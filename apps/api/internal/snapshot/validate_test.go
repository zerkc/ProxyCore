package snapshot

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/cluster"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
)

func sealedEnvelope(t *testing.T) Envelope {
	t.Helper()
	primaryID := uuid.New()
	env := Envelope{
		Transient: TransientFields{
			SnapshotVersion:      domain.SnapshotVersionV1,
			ReplicationVersion:   domain.ReplicationVersionV1,
			SourcePrimaryID:      primaryID,
			LeadershipGeneration: 5,
			CapturedAt:           time.Date(2026, 5, 10, 11, 12, 13, 0, time.UTC),
		},
		NodeLocal: NodeLocalFields{
			Ingress: domain.Ingress{IPv4: "192.0.2.10"},
			NodeID:  domain.NodeID(uuid.New().String()),
			Role:    domain.TopologyRoleNode,
		},
		Replicated: ReplicatedFields{
			Configuration: map[string]any{"settings": "ok"},
			Secrets:       []ReplicatedSecret{},
			Owners:        []ReplicatedOwner{},
		},
	}
	hash, err := env.ExpectedHash()
	if err != nil {
		t.Fatalf("ExpectedHash: %v", err)
	}
	env.ContentHash = hash
	return env
}

func TestValidatorAcceptsSealedEnvelope(t *testing.T) {
	kekBytes, err := cluster.GenerateKEK()
	if err != nil {
		t.Fatalf("GenerateKEK: %v", err)
	}
	kek, err := cluster.NewKEK(kekBytes)
	if err != nil {
		t.Fatalf("NewKEK: %v", err)
	}
	v := NewValidator(kek)
	env := sealedEnvelope(t)
	result := v.Validate(context.Background(), env)
	if !result.OK {
		t.Errorf("expected OK on sealed envelope, got issues: %+v", result.Issues)
	}
}

func TestValidatorRejectsCorruptedHash(t *testing.T) {
	kekBytes, err := cluster.GenerateKEK()
	if err != nil {
		t.Fatalf("GenerateKEK: %v", err)
	}
	kek, err := cluster.NewKEK(kekBytes)
	if err != nil {
		t.Fatalf("NewKEK: %v", err)
	}
	v := NewValidator(kek)
	env := sealedEnvelope(t)
	env.ContentHash = "deadbeef"
	result := v.Validate(context.Background(), env)
	if result.OK {
		t.Errorf("expected rejection on corrupted hash")
	}
	if !hasCode(result, "STRUCTURE") {
		t.Errorf("expected STRUCTURE issue, got %+v", result.Issues)
	}
}

func TestValidatorRejectsUnsupportedSnapshotVersion(t *testing.T) {
	kekBytes, err := cluster.GenerateKEK()
	if err != nil {
		t.Fatalf("GenerateKEK: %v", err)
	}
	kek, err := cluster.NewKEK(kekBytes)
	if err != nil {
		t.Fatalf("NewKEK: %v", err)
	}
	v := NewValidator(kek)
	env := sealedEnvelope(t)
	env.Transient.SnapshotVersion = 99
	// Re-anchor the hash so the structural check does not short-circuit
	// before the compatibility check.
	if _, err := env.ExpectedHash(); err == nil {
		if hash, hashErr := env.ExpectedHash(); hashErr == nil {
			env.ContentHash = hash
		}
	}
	result := v.Validate(context.Background(), env)
	if result.OK {
		t.Errorf("expected rejection on unsupported snapshotVersion")
	}
	if !hasCode(result, "COMPATIBILITY") {
		t.Errorf("expected COMPATIBILITY issue, got %+v", result.Issues)
	}
}

func TestValidatorRejectsUndecryptableSecret(t *testing.T) {
	producerKEK, err := cluster.GenerateKEK()
	if err != nil {
		t.Fatalf("GenerateKEK: %v", err)
	}
	producerKEKObj, err := cluster.NewKEK(producerKEK)
	if err != nil {
		t.Fatalf("NewKEK: %v", err)
	}
	envelope, err := producerKEKObj.Wrap([]byte("original secret"))
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}

	consumerKEK, err := cluster.GenerateKEK()
	if err != nil {
		t.Fatalf("GenerateKEK: %v", err)
	}
	consumerKEKObj, err := cluster.NewKEK(consumerKEK)
	if err != nil {
		t.Fatalf("NewKEK: %v", err)
	}

	v := NewValidator(consumerKEKObj)
	env := sealedEnvelope(t)
	env.Replicated.Secrets = []ReplicatedSecret{
		{ID: uuid.New(), Purpose: "tls-key", Envelope: envelope},
	}
	// Re-anchor content hash for the new payload.
	if hash, err := env.ExpectedHash(); err == nil {
		env.ContentHash = hash
	}
	result := v.Validate(context.Background(), env)
	if result.OK {
		t.Fatalf("expected rejection on undecryptable secret")
	}
	if !hasCode(result, "SECRET_DECRYPT") {
		t.Errorf("expected SECRET_DECRYPT issue, got %+v", result.Issues)
	}
}

func TestValidatorAcceptsDecryptableSecret(t *testing.T) {
	kekBytes, err := cluster.GenerateKEK()
	if err != nil {
		t.Fatalf("GenerateKEK: %v", err)
	}
	kek, err := cluster.NewKEK(kekBytes)
	if err != nil {
		t.Fatalf("NewKEK: %v", err)
	}
	v := NewValidator(kek)
	env := sealedEnvelope(t)
	wrapped, err := kek.Wrap([]byte("round-trip value"))
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	env.Replicated.Secrets = []ReplicatedSecret{
		{ID: uuid.New(), Purpose: "tls-key", Envelope: wrapped},
	}
	if hash, err := env.ExpectedHash(); err == nil {
		env.ContentHash = hash
	}
	result := v.Validate(context.Background(), env)
	if !result.OK {
		t.Errorf("expected OK on decryptable secret, got issues: %+v", result.Issues)
	}
}

func TestValidatorRejectsEmptySecretEnvelope(t *testing.T) {
	kekBytes, err := cluster.GenerateKEK()
	if err != nil {
		t.Fatalf("GenerateKEK: %v", err)
	}
	kek, err := cluster.NewKEK(kekBytes)
	if err != nil {
		t.Fatalf("NewKEK: %v", err)
	}
	v := NewValidator(kek)
	env := sealedEnvelope(t)
	env.Replicated.Secrets = []ReplicatedSecret{
		{ID: uuid.New(), Purpose: "tls-key", Envelope: "not-a-kek-envelope"},
	}
	if hash, err := env.ExpectedHash(); err == nil {
		env.ContentHash = hash
	}
	result := v.Validate(context.Background(), env)
	if !hasCode(result, "SECRET_ENVELOPE") {
		t.Errorf("expected SECRET_ENVELOPE issue, got %+v", result.Issues)
	}
}

func TestValidatorRejectsOwnerWithBadHash(t *testing.T) {
	kekBytes, err := cluster.GenerateKEK()
	if err != nil {
		t.Fatalf("GenerateKEK: %v", err)
	}
	kek, err := cluster.NewKEK(kekBytes)
	if err != nil {
		t.Fatalf("NewKEK: %v", err)
	}
	v := NewValidator(kek)
	env := sealedEnvelope(t)
	env.Replicated.Owners = []ReplicatedOwner{
		{
			UserID:       uuid.New(),
			Username:     "imposter",
			PasswordHash: "not-scrypt",
			Role:         "owner",
		},
	}
	if hash, err := env.ExpectedHash(); err == nil {
		env.ContentHash = hash
	}
	result := v.Validate(context.Background(), env)
	if !hasCode(result, "OWNER_HASH") {
		t.Errorf("expected OWNER_HASH issue, got %+v", result.Issues)
	}
}

func TestValidatorRejectsOwnerWithUnknownRole(t *testing.T) {
	kekBytes, err := cluster.GenerateKEK()
	if err != nil {
		t.Fatalf("GenerateKEK: %v", err)
	}
	kek, err := cluster.NewKEK(kekBytes)
	if err != nil {
		t.Fatalf("NewKEK: %v", err)
	}
	v := NewValidator(kek)
	env := sealedEnvelope(t)
	env.Replicated.Owners = []ReplicatedOwner{
		{
			UserID:       uuid.New(),
			Username:     "auditor",
			PasswordHash: "scrypt$16384$8$1$aa$bb",
			Role:         "auditor",
		},
	}
	if hash, err := env.ExpectedHash(); err == nil {
		env.ContentHash = hash
	}
	result := v.Validate(context.Background(), env)
	if !hasCode(result, "OWNER_ROLE") {
		t.Errorf("expected OWNER_ROLE issue, got %+v", result.Issues)
	}
}

func TestValidatorWithoutKEKReportsNoKey(t *testing.T) {
	v := NewValidator(nil)
	env := sealedEnvelope(t)
	result := v.Validate(context.Background(), env)
	// The KEK check fires only when secrets are present; with an empty
	// secrets list the validator should pass.
	if !result.OK {
		t.Errorf("expected OK when no secrets are present and KEK is nil, got %+v", result.Issues)
	}

	// Add a secret to force the NO_KEY branch.
	env.Replicated.Secrets = []ReplicatedSecret{
		{ID: uuid.New(), Purpose: "tls-key", Envelope: "v1.kek:AA:BB:CC"},
	}
	if hash, err := env.ExpectedHash(); err == nil {
		env.ContentHash = hash
	}
	result = v.Validate(context.Background(), env)
	if !hasCode(result, "NO_KEY") {
		t.Errorf("expected NO_KEY issue, got %+v", result.Issues)
	}
}

func TestValidatorAcceptsOperatorRole(t *testing.T) {
	kekBytes, err := cluster.GenerateKEK()
	if err != nil {
		t.Fatalf("GenerateKEK: %v", err)
	}
	kek, err := cluster.NewKEK(kekBytes)
	if err != nil {
		t.Fatalf("NewKEK: %v", err)
	}
	v := NewValidator(kek)
	env := sealedEnvelope(t)
	env.Replicated.Owners = []ReplicatedOwner{
		{
			UserID:       uuid.New(),
			Username:     "operator1",
			PasswordHash: "scrypt$16384$8$1$aa$bb",
			Role:         "operator",
		},
	}
	if hash, err := env.ExpectedHash(); err == nil {
		env.ContentHash = hash
	}
	result := v.Validate(context.Background(), env)
	if !result.OK {
		t.Errorf("expected OK with operator role, got %+v", result.Issues)
	}
}

func hasCode(r ValidationResult, code string) bool {
	for _, issue := range r.Issues {
		if issue.Code == code {
			return true
		}
	}
	return false
}
