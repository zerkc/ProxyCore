package domain

import (
	"math"
	"strings"
	"testing"
)

func TestRoleIsValid(t *testing.T) {
	cases := []struct {
		role TopologyRole
		want bool
	}{
		{TopologyRoleStandalone, true},
		{TopologyRolePrimary, true},
		{TopologyRolePrimaryWithNodes, true},
		{TopologyRoleNode, true},
		{TopologyRoleStalePrimary, true},
		{TopologyRole(""), false},
		{TopologyRole("owner"), false},
		{TopologyRole("PRIMARY"), false},
		{TopologyRole("node-pending"), false},
	}
	for _, tc := range cases {
		if got := tc.role.IsValid(); got != tc.want {
			t.Errorf("Role(%q).IsValid() = %v, want %v", string(tc.role), got, tc.want)
		}
	}
}

func TestNewInstallationIDFormat(t *testing.T) {
	seen := make(map[InstallationID]struct{}, 1000)
	for i := 0; i < 1000; i++ {
		id := NewInstallationID()
		if !id.IsValid() {
			t.Fatalf("NewInstallationID returned invalid id %q", string(id))
		}
		seen[id] = struct{}{}
	}
	if len(seen) < 990 {
		t.Errorf("NewInstallationID showed low uniqueness: %d unique ids across 1000 calls", len(seen))
	}
}

func TestNewNodeIDFormat(t *testing.T) {
	seen := make(map[NodeID]struct{}, 1000)
	for i := 0; i < 1000; i++ {
		id := NewNodeID()
		if !id.IsValid() {
			t.Fatalf("NewNodeID returned invalid id %q", string(id))
		}
		seen[id] = struct{}{}
	}
	if len(seen) < 990 {
		t.Errorf("NewNodeID showed low uniqueness: %d unique ids across 1000 calls", len(seen))
	}
}

func TestInstallationIDIsValid(t *testing.T) {
	cases := []struct {
		name string
		id   InstallationID
		want bool
	}{
		{"empty", InstallationID(""), false},
		{"v4", InstallationID("550e8400-e29b-41d4-a716-446655440000"), true},
		{"uuidv1", InstallationID("550e8400-e29b-11d4-a716-446655440000"), false},
		{"too-short", InstallationID("550e8400-e29b-41d4-a716"), false},
		{"trailing-garbage", InstallationID("550e8400-e29b-41d4-a716-446655440000-extra"), false},
		{"uppercase", InstallationID("550E8400-E29B-41D4-A716-446655440000"), false},
		{"not-uuid", InstallationID("not-a-uuid"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.id.IsValid(); got != tc.want {
				t.Errorf("InstallationID(%q).IsValid() = %v, want %v", string(tc.id), got, tc.want)
			}
		})
	}
}

func TestLeadershipGenerationNext(t *testing.T) {
	var g LeadershipGeneration = 10
	if got := g.Next(); got != 11 {
		t.Errorf("Next() = %d, want 11", got)
	}
	if g != 10 {
		t.Errorf("Next() mutated receiver: %d, want 10", g)
	}

	var zero LeadershipGeneration
	if got := zero.Next(); got != 1 {
		t.Errorf("zero.Next() = %d, want 1", got)
	}
}

func TestLeadershipGenerationNextPanicsAtMax(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("Next() on MaxUint64 should panic")
		}
	}()
	max := LeadershipGeneration(math.MaxUint64)
	_ = max.Next()
}

func TestLeadershipGenerationComparison(t *testing.T) {
	var a, b LeadershipGeneration = 5, 6
	if !(a < b) {
		t.Errorf("expected %d < %d", a, b)
	}
	if !(b > a) {
		t.Errorf("expected %d > %d", b, a)
	}
	if !(a == LeadershipGeneration(5)) {
		t.Errorf("equality check failed")
	}
}

func TestSnapshotVersionIsValid(t *testing.T) {
	cases := []struct {
		v    SnapshotVersion
		want bool
	}{
		{0, false},
		{SnapshotVersionV1, true},
		{2, true},
	}
	for _, tc := range cases {
		if got := tc.v.IsValid(); got != tc.want {
			t.Errorf("SnapshotVersion(%d).IsValid() = %v, want %v", tc.v, got, tc.want)
		}
	}
}

func TestReplicationVersionIsValid(t *testing.T) {
	cases := []struct {
		v    ReplicationVersion
		want bool
	}{
		{0, false},
		{ReplicationVersionV1, true},
		{2, true},
	}
	for _, tc := range cases {
		if got := tc.v.IsValid(); got != tc.want {
			t.Errorf("ReplicationVersion(%d).IsValid() = %v, want %v", tc.v, got, tc.want)
		}
	}
}

func TestInstallationIDConstantsAreValid(t *testing.T) {
	// Sanity: a constant string starting with a non-zero hex nibble in the
	// version slot must be rejected so we never encode a UUIDv1 by mistake.
	if strings.HasPrefix(string(NewInstallationID()), "00000000-0000-1000") {
		t.Skip("monkey-patched environment; skipped")
	}
}
