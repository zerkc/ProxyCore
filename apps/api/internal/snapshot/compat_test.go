package snapshot

import (
	"errors"
	"strings"
	"testing"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
)

func TestCheckCompatibilityAcceptsV1(t *testing.T) {
	env := newValidEnvelope()
	if err := CheckCompatibility(env); err != nil {
		t.Errorf("expected v1 envelope to be accepted, got %v", err)
	}
}

func TestCheckCompatibilityRejectsUnsupportedSnapshotVersion(t *testing.T) {
	env := newValidEnvelope()
	env.Transient.SnapshotVersion = 99
	err := CheckCompatibility(env)
	if err == nil {
		t.Fatalf("expected CompatibilityError for snapshotVersion=99")
	}
	var compatErr *CompatibilityError
	if !errors.As(err, &compatErr) {
		t.Fatalf("expected *CompatibilityError, got %T", err)
	}
	if compatErr.Field != "transient.snapshotVersion" {
		t.Errorf("expected Field transient.snapshotVersion, got %q", compatErr.Field)
	}
	if compatErr.Declared != 99 {
		t.Errorf("expected Declared=99, got %d", compatErr.Declared)
	}
	if !strings.Contains(err.Error(), "snapshotVersion=99") {
		t.Errorf("error message should mention snapshotVersion=99, got %q", err.Error())
	}
}

func TestCheckCompatibilityRejectsUnsupportedReplicationVersion(t *testing.T) {
	env := newValidEnvelope()
	env.Transient.ReplicationVersion = 99
	err := CheckCompatibility(env)
	if err == nil {
		t.Fatalf("expected CompatibilityError for replicationVersion=99")
	}
	var compatErr *CompatibilityError
	if !errors.As(err, &compatErr) {
		t.Fatalf("expected *CompatibilityError, got %T", err)
	}
	if compatErr.Field != "transient.replicationVersion" {
		t.Errorf("expected Field transient.replicationVersion, got %q", compatErr.Field)
	}
	if compatErr.Declared != 99 {
		t.Errorf("expected Declared=99, got %d", compatErr.Declared)
	}
}

func TestCheckCompatibilityRejectsSnapshotVersionBeforeSnapshotValidation(t *testing.T) {
	// An envelope with an unsupported snapshot version is rejected even
	// when the rest of the shape is correct; compatibility check is the
	// first thing the validator does after Validate.
	env := newValidEnvelope()
	env.Transient.SnapshotVersion = 0
	if err := CheckCompatibility(env); err == nil {
		t.Errorf("expected CheckCompatibility to reject zero snapshotVersion")
	}
}

func TestAddSnapshotVersionExtendsAcceptList(t *testing.T) {
	original := append([]domain.SnapshotVersion(nil), SupportedSnapshotVersions...)
	defer func() { SupportedSnapshotVersions = original }()

	added := domain.SnapshotVersion(7)
	if err := AddSnapshotVersion(added); err != nil {
		t.Fatalf("AddSnapshotVersion: %v", err)
	}
	if !containsSnapshotVersion(SupportedSnapshotVersions, added) {
		t.Errorf("expected %d in accept-list", added)
	}

	env := newValidEnvelope()
	env.Transient.SnapshotVersion = added
	if err := CheckCompatibility(env); err != nil {
		t.Errorf("expected accepted snapshotVersion=%d, got %v", added, err)
	}
}

func TestAddSnapshotVersionRejectsInvalid(t *testing.T) {
	if err := AddSnapshotVersion(0); err == nil {
		t.Errorf("expected AddSnapshotVersion(0) to fail")
	}
}

func TestAddSnapshotVersionIsIdempotent(t *testing.T) {
	original := append([]domain.SnapshotVersion(nil), SupportedSnapshotVersions...)
	defer func() { SupportedSnapshotVersions = original }()

	added := domain.SnapshotVersion(7)
	if err := AddSnapshotVersion(added); err != nil {
		t.Fatalf("AddSnapshotVersion (first): %v", err)
	}
	size := len(SupportedSnapshotVersions)
	if err := AddSnapshotVersion(added); err != nil {
		t.Fatalf("AddSnapshotVersion (second): %v", err)
	}
	if len(SupportedSnapshotVersions) != size {
		t.Errorf("expected idempotent add, size changed %d -> %d", size, len(SupportedSnapshotVersions))
	}
}

func TestAddReplicationVersionExtendsAcceptList(t *testing.T) {
	original := append([]domain.ReplicationVersion(nil), SupportedReplicationVersions...)
	defer func() { SupportedReplicationVersions = original }()

	added := domain.ReplicationVersion(7)
	if err := AddReplicationVersion(added); err != nil {
		t.Fatalf("AddReplicationVersion: %v", err)
	}
	env := newValidEnvelope()
	env.Transient.ReplicationVersion = added
	if err := CheckCompatibility(env); err != nil {
		t.Errorf("expected accepted replicationVersion=%d, got %v", added, err)
	}
}

func TestAddReplicationVersionRejectsInvalid(t *testing.T) {
	if err := AddReplicationVersion(0); err == nil {
		t.Errorf("expected AddReplicationVersion(0) to fail")
	}
}
