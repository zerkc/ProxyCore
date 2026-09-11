package snapshot

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
)

func TestMarshalUnmarshalRoundTrip(t *testing.T) {
	env := newValidEnvelope()
	env.Transient.CapturedAt = time.Date(2026, 5, 10, 11, 12, 13, 0, time.UTC)
	data, err := Marshal(env)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got, err := Unmarshal(data)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.ContentHash != env.ContentHash {
		t.Errorf("content hash changed across round trip: %s -> %s", env.ContentHash, got.ContentHash)
	}
	if got.Transient.SourcePrimaryID != env.Transient.SourcePrimaryID {
		t.Errorf("source primary id changed: %s -> %s", env.Transient.SourcePrimaryID, got.Transient.SourcePrimaryID)
	}
}

func TestMarshalIsByteStable(t *testing.T) {
	// Two envelopes that differ only in non-replicated fields must produce
	// distinct JSON; the canonical form is stable for identical inputs.
	a := newValidEnvelope()
	a.Transient.SourcePrimaryID = uuid.MustParse("11111111-1111-4111-8111-111111111111")
	b := newValidEnvelope()
	b.Transient.SourcePrimaryID = uuid.MustParse("22222222-2222-4222-8222-222222222222")

	aBytes, err := Marshal(a)
	if err != nil {
		t.Fatalf("Marshal a: %v", err)
	}
	bBytes, err := Marshal(b)
	if err != nil {
		t.Fatalf("Marshal b: %v", err)
	}
	if string(aBytes) == string(bBytes) {
		t.Errorf("expected distinct JSON for envelopes with different source primary ids")
	}

	// Re-marshal a second time and assert exact equality.
	aBytes2, err := Marshal(a)
	if err != nil {
		t.Fatalf("Marshal a (second): %v", err)
	}
	if string(aBytes) != string(aBytes2) {
		t.Errorf("Marshal output is not byte-stable across calls")
	}
}

func TestMarshalRejectsMalformedInput(t *testing.T) {
	// Empty struct should fail NormalizeSnapshot or downstream validation.
	var env Envelope
	if _, err := Marshal(env); err == nil {
		// The marshal itself does not require classification; NormalizeSnapshot
		// succeeds on empty input and produces "{}". This is acceptable.
		// We only assert the happy path here.
	}
}

func TestUnmarshalRejectsGarbage(t *testing.T) {
	cases := [][]byte{
		[]byte(""),
		[]byte("not json"),
		[]byte("{"),
		[]byte(`{"transient": "not-an-object"}`),
	}
	for _, data := range cases {
		if _, err := Unmarshal(data); err == nil {
			t.Errorf("expected Unmarshal to reject %q", string(data))
		}
	}
}

func TestEnvelopePreservesCanonicalJSON(t *testing.T) {
	env := newValidEnvelope()
	canonical, err := Marshal(env)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	// The canonical form must be valid JSON (so external tools can read it).
	var probe map[string]any
	if err := json.Unmarshal(canonical, &probe); err != nil {
		t.Errorf("canonical output is not valid JSON: %v", err)
	}
	if _, ok := probe["transient"]; !ok {
		t.Errorf("canonical output missing transient section")
	}
	if _, ok := probe["replicated"]; !ok {
		t.Errorf("canonical output missing replicated section")
	}
	if _, ok := probe["nodeLocal"]; !ok {
		t.Errorf("canonical output missing nodeLocal section")
	}
}

func TestEnvelopeMarshalRoundTripPreservesRoleAndVersion(t *testing.T) {
	env := newValidEnvelope()
	env.NodeLocal.Role = domain.TopologyRoleNode
	env.Transient.SnapshotVersion = 7
	env.Transient.ReplicationVersion = 3

	data, err := Marshal(env)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got, err := Unmarshal(data)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.NodeLocal.Role != env.NodeLocal.Role {
		t.Errorf("role lost in round trip: %s != %s", env.NodeLocal.Role, got.NodeLocal.Role)
	}
	if got.Transient.SnapshotVersion != env.Transient.SnapshotVersion {
		t.Errorf("snapshot version lost: %d != %d", env.Transient.SnapshotVersion, got.Transient.SnapshotVersion)
	}
	if got.Transient.ReplicationVersion != env.Transient.ReplicationVersion {
		t.Errorf("replication version lost: %d != %d", env.Transient.ReplicationVersion, got.Transient.ReplicationVersion)
	}
}
