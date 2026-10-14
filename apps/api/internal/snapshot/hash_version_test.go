package snapshot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"github.com/google/uuid"
	"testing"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
)

func newV2Envelope(t *testing.T, ingress domain.Ingress) Envelope {
	t.Helper()
	env := newValidEnvelope()
	env.HashVersion = HashVersionIngressExcluded
	env.Replicated.Configuration = domain.ConfigurationSnapshot{
		Settings: domain.Settings{
			Ingress:             ingress,
			RetentionMaxAgeDays: 7,
			RetentionMaxSizeMb:  50,
		},
	}
	hash, err := env.ExpectedHash()
	if err != nil {
		t.Fatalf("ExpectedHash: %v", err)
	}
	env.ContentHash = hash
	return env
}

func TestHashV2IgnoresSettingsIngress(t *testing.T) {
	a := newV2Envelope(t, domain.Ingress{IPv4: "192.0.2.1", IPv6: "2001:db8::1"})
	b := newV2Envelope(t, domain.Ingress{IPv4: "198.51.100.9"})
	if a.ContentHash != b.ContentHash {
		t.Fatalf("ingress-only difference changed the v2 hash: %s vs %s", a.ContentHash, b.ContentHash)
	}
	if err := b.VerifyHash(); err != nil {
		t.Fatalf("VerifyHash: %v", err)
	}
}

func TestHashV2ChangesWithOtherReplicatedFields(t *testing.T) {
	base := newV2Envelope(t, domain.Ingress{IPv4: "192.0.2.1"})
	mutated := base
	mutated.Replicated.Configuration = domain.ConfigurationSnapshot{
		Settings: domain.Settings{
			Ingress:             domain.Ingress{IPv4: "192.0.2.1"},
			RetentionMaxAgeDays: 8,
			RetentionMaxSizeMb:  50,
		},
	}
	got, err := mutated.ExpectedHash()
	if err != nil {
		t.Fatal(err)
	}
	if got == base.ContentHash {
		t.Fatalf("non-ingress change did not change the v2 hash")
	}
	if err := mutated.VerifyHash(); err == nil {
		t.Fatalf("tampered v2 envelope verified")
	}
}

func TestHashLegacyEnvelopeStillVerifies(t *testing.T) {
	env := newV2Envelope(t, domain.Ingress{IPv4: "192.0.2.1"})
	env.HashVersion = 0
	legacy := domain.StableStringify(env.Replicated)
	env.ContentHash = sha256Hex([]byte(legacy))
	if err := env.VerifyHash(); err != nil {
		t.Fatalf("legacy envelope must verify: %v", err)
	}
	other := env
	other.Replicated.Configuration = domain.ConfigurationSnapshot{
		Settings: domain.Settings{Ingress: domain.Ingress{IPv4: "203.0.113.5"}, RetentionMaxAgeDays: 7, RetentionMaxSizeMb: 50},
	}
	if err := other.VerifyHash(); err == nil {
		t.Fatalf("legacy hash must still cover ingress")
	}
	env.HashVersion = 1
	if err := env.VerifyHash(); err != nil {
		t.Fatalf("explicit v1 must verify: %v", err)
	}
}

func TestHashRejectsUnknownVersion(t *testing.T) {
	env := newV2Envelope(t, domain.Ingress{})
	env.HashVersion = 99
	if err := env.VerifyHash(); err == nil {
		t.Fatalf("unknown hash version must fail")
	}
}

func TestHashV2SurvivesMarshalRoundTrip(t *testing.T) {
	env := newV2Envelope(t, domain.Ingress{IPv4: "192.0.2.1"})
	raw, err := Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	back, err := Unmarshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if back.HashVersion != HashVersionIngressExcluded {
		t.Fatalf("hashVersion lost: %d", back.HashVersion)
	}
	if err := back.Validate(); err != nil {
		t.Fatalf("round-tripped v2 envelope must validate: %v", err)
	}
	if err := CheckCompatibility(back); err != nil {
		t.Fatalf("compat: %v", err)
	}
}

func TestExporterStampsCurrentHashVersion(t *testing.T) {
	if HashVersionCurrent != HashVersionStableSecrets {
		t.Fatalf("new snapshots must use the stable-secrets definition")
	}
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func newV3Envelope(t *testing.T, secrets ...ReplicatedSecret) Envelope {
	t.Helper()
	env := newV2Envelope(t, domain.Ingress{IPv4: "192.0.2.1"})
	env.HashVersion = HashVersionStableSecrets
	env.Replicated.Secrets = secrets
	hash, err := env.ExpectedHash()
	if err != nil {
		t.Fatalf("ExpectedHash: %v", err)
	}
	env.ContentHash = hash
	return env
}

func TestHashV3IgnoresSecretEnvelopeNonce(t *testing.T) {
	id := uuid.New()
	a := newV3Envelope(t, ReplicatedSecret{ID: id, Purpose: "dns", Envelope: "v1.kek:nonce-a", Digest: "d1"})
	b := newV3Envelope(t, ReplicatedSecret{ID: id, Purpose: "dns", Envelope: "v1.kek:nonce-b", Digest: "d1"})
	if a.ContentHash != b.ContentHash {
		t.Fatalf("re-wrapped envelope changed the v3 hash")
	}
	if err := b.VerifyHash(); err != nil {
		t.Fatal(err)
	}
}

func TestHashV3CoversSecretDigestIdentityAndRejectsTamper(t *testing.T) {
	id := uuid.New()
	base := newV3Envelope(t, ReplicatedSecret{ID: id, Purpose: "dns", Envelope: "e", Digest: "d1"})
	for name, mutate := range map[string]func(*Envelope){
		"digest":  func(e *Envelope) { e.Replicated.Secrets[0].Digest = "d2" },
		"id":      func(e *Envelope) { e.Replicated.Secrets[0].ID = uuid.New() },
		"purpose": func(e *Envelope) { e.Replicated.Secrets[0].Purpose = "tls" },
		"added": func(e *Envelope) {
			e.Replicated.Secrets = append(e.Replicated.Secrets, ReplicatedSecret{ID: uuid.New(), Purpose: "x", Digest: "d"})
		},
	} {
		t.Run(name, func(t *testing.T) {
			env := base
			env.Replicated.Secrets = append([]ReplicatedSecret(nil), base.Replicated.Secrets...)
			mutate(&env)
			if err := env.VerifyHash(); err == nil {
				t.Fatalf("tampered v3 envelope (%s) verified", name)
			}
		})
	}
	missing := base
	missing.Replicated.Secrets = []ReplicatedSecret{{ID: id, Purpose: "dns", Envelope: "e"}}
	if err := missing.VerifyHash(); err == nil {
		t.Fatal("v3 secret without a digest must be rejected")
	}
}

func TestHashV1AndV2StillCoverSecretEnvelope(t *testing.T) {
	id := uuid.New()
	for _, version := range []int{0, HashVersionLegacy, HashVersionIngressExcluded} {
		env := newV2Envelope(t, domain.Ingress{IPv4: "192.0.2.1"})
		env.HashVersion = version
		env.Replicated.Secrets = []ReplicatedSecret{{ID: id, Purpose: "dns", Envelope: "e1"}}
		hash, err := env.ExpectedHash()
		if err != nil {
			t.Fatal(err)
		}
		env.ContentHash = hash
		if err := env.VerifyHash(); err != nil {
			t.Fatalf("v%d must verify: %v", version, err)
		}
		env.Replicated.Secrets[0].Envelope = "e2"
		if err := env.VerifyHash(); err == nil {
			t.Fatalf("v%d must still cover the envelope", version)
		}
	}
}

func TestExportTwiceYieldsIdenticalHashAndSecretChangeChangesIt(t *testing.T) {
	exporter, _, secrets, _ := newTestExporter()
	in := mustExporterInput(t)
	first, err := exporter.Export(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	second, err := exporter.Export(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if first.HashVersion != HashVersionStableSecrets || len(first.Replicated.Secrets) == 0 {
		t.Fatalf("version=%d secrets=%d", first.HashVersion, len(first.Replicated.Secrets))
	}
	if first.Replicated.Secrets[0].Envelope == second.Replicated.Secrets[0].Envelope {
		t.Fatal("test premise: envelopes should differ per export")
	}
	if first.ContentHash != second.ContentHash {
		t.Fatal("unchanged configuration produced different hashes")
	}
	if err := second.Validate(); err != nil {
		t.Fatal(err)
	}
	secrets.entries[0].Value = []byte("changed-value")
	third, err := exporter.Export(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if third.ContentHash == first.ContentHash {
		t.Fatal("changing a secret value must change the hash")
	}
}
