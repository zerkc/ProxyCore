package snapshot

import (
	"errors"
	"fmt"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
)

// SupportedSnapshotVersions is the local node's accept-list for snapshot
// envelope versions. New versions are appended as Phase 1+ work units
// introduce them; the list MUST be monotonic and never shrink without a
// migration plan because nodes that upgrade would reject their own future
// snapshots.
var SupportedSnapshotVersions = []domain.SnapshotVersion{
	domain.SnapshotVersionV1,
}

// SupportedReplicationVersions is the local node's accept-list for
// replication-control versions.
var SupportedReplicationVersions = []domain.ReplicationVersion{
	domain.ReplicationVersionV1,
}

// CompatibilityError is returned by CheckCompatibility when the snapshot
// envelope's declared versions are outside the local accept-list.
type CompatibilityError struct {
	Field              string
	Declared           uint32
	Supported          []uint32
}

func (e *CompatibilityError) Error() string {
	return fmt.Sprintf(
		"snapshot compatibility error: %s=%d is not in the local accept-list %v",
		e.Field,
		e.Declared,
		e.Supported,
	)
}

// CheckCompatibility verifies that the envelope's declared versions are in
// the local accept-list. It does NOT validate the structural shape of the
// envelope (use Envelope.Validate for that) and does NOT verify the content
// hash (use Envelope.VerifyHash for that).
func CheckCompatibility(env Envelope) error {
	if !containsSnapshotVersion(SupportedSnapshotVersions, env.Transient.SnapshotVersion) {
		return &CompatibilityError{
			Field:     "transient.snapshotVersion",
			Declared:  uint32(env.Transient.SnapshotVersion),
			Supported: snapshotVersionList(SupportedSnapshotVersions),
		}
	}
	if !containsReplicationVersion(SupportedReplicationVersions, env.Transient.ReplicationVersion) {
		return &CompatibilityError{
			Field:     "transient.replicationVersion",
			Declared:  uint32(env.Transient.ReplicationVersion),
			Supported: replicationVersionList(SupportedReplicationVersions),
		}
	}
	return nil
}

// AddSnapshotVersion extends the local accept-list at runtime. Phase 0
// rejects new entries; Phase 5 hardening introduces the migration logic
// that promotes a newer version into the list after the local node has
// been upgraded.
func AddSnapshotVersion(v domain.SnapshotVersion) error {
	if !v.IsValid() {
		return errors.New("invalid snapshot version")
	}
	if containsSnapshotVersion(SupportedSnapshotVersions, v) {
		return nil
	}
	SupportedSnapshotVersions = append(SupportedSnapshotVersions, v)
	return nil
}

// AddReplicationVersion extends the local replication accept-list.
func AddReplicationVersion(v domain.ReplicationVersion) error {
	if !v.IsValid() {
		return errors.New("invalid replication version")
	}
	if containsReplicationVersion(SupportedReplicationVersions, v) {
		return nil
	}
	SupportedReplicationVersions = append(SupportedReplicationVersions, v)
	return nil
}

func containsSnapshotVersion(list []domain.SnapshotVersion, v domain.SnapshotVersion) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}

func containsReplicationVersion(list []domain.ReplicationVersion, v domain.ReplicationVersion) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}

func snapshotVersionList(in []domain.SnapshotVersion) []uint32 {
	out := make([]uint32, 0, len(in))
	for _, v := range in {
		out = append(out, uint32(v))
	}
	return out
}

func replicationVersionList(in []domain.ReplicationVersion) []uint32 {
	out := make([]uint32, 0, len(in))
	for _, v := range in {
		out = append(out, uint32(v))
	}
	return out
}
