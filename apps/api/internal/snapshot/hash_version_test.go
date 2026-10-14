package snapshot

import (
	"crypto/sha256"
	"encoding/hex"
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

func TestExporterStampsHashV2(t *testing.T) {
	if HashVersionCurrent != HashVersionIngressExcluded {
		t.Fatalf("new snapshots must use the ingress-excluded definition")
	}
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
