package domain

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math"
	"strings"
)

// TopologyRole is the durable PRIMARY/NODE role of a ProxyCore installation.
//
// Distinct from the admin Role used by the auth package.
type TopologyRole string

const (
	// TopologyRoleStandalone is a fresh installation that has not enrolled
	// with or been enrolled by another installation. Behaves as a writable
	// primary until it transitions.
	TopologyRoleStandalone TopologyRole = "standalone-primary"
	// TopologyRolePrimary is an installation that owns editable desired
	// configuration but currently has no enrolled nodes.
	TopologyRolePrimary TopologyRole = "primary"
	// TopologyRolePrimaryWithNodes is an installation that owns editable
	// desired configuration and has at least one enrolled node.
	TopologyRolePrimaryWithNodes TopologyRole = "primary-with-nodes"
	// TopologyRoleNode is an installation that receives and applies
	// snapshots from an enrolled primary; ordinary configuration mutations
	// are rejected at the API boundary.
	TopologyRoleNode TopologyRole = "node"
	// TopologyRoleStalePrimary is an installation that returned after a
	// newer leadership generation was promoted elsewhere; it serves its
	// last valid local data plane but blocks configuration writes.
	TopologyRoleStalePrimary TopologyRole = "stale-primary"
)

// knownTopologyRoles is the accept-list used by TopologyRole.IsValid.
var knownTopologyRoles = map[TopologyRole]struct{}{
	TopologyRoleStandalone:      {},
	TopologyRolePrimary:         {},
	TopologyRolePrimaryWithNodes: {},
	TopologyRoleNode:            {},
	TopologyRoleStalePrimary:    {},
}

// IsValid reports whether the role is one of the five known durable roles.
func (r TopologyRole) IsValid() bool {
	_, ok := knownTopologyRoles[r]
	return ok
}

// String renders the role as the canonical string.
func (r TopologyRole) String() string {
	return string(r)
}

// InstallationID is the durable identifier of a ProxyCore installation.
// Format: UUIDv4 (RFC 4122) with hyphens, lowercase.
type InstallationID string

// NewInstallationID generates a fresh UUIDv4 installation identifier.
func NewInstallationID() InstallationID {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("generate installation id: %v", err))
	}
	// RFC 4122 v4 layout.
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	hexBuf := make([]byte, 36)
	hex.Encode(hexBuf[0:8], b[0:4])
	hexBuf[8] = '-'
	hex.Encode(hexBuf[9:13], b[4:6])
	hexBuf[13] = '-'
	hex.Encode(hexBuf[14:18], b[6:8])
	hexBuf[18] = '-'
	hex.Encode(hexBuf[19:23], b[8:10])
	hexBuf[23] = '-'
	hex.Encode(hexBuf[24:36], b[10:16])
	return InstallationID(strings.ToLower(string(hexBuf)))
}

// IsValid reports whether the installation id is a syntactically valid
// UUIDv4 (lowercase, hyphenated, version nibble = 4, variant nibble in
// {8,9,a,b}).
func (id InstallationID) IsValid() bool {
	s := string(id)
	if len(s) != 36 {
		return false
	}
	if s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return false
	}
	for i, r := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			continue
		}
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'f':
		default:
			return false
		}
	}
	if s[14] != '4' {
		return false
	}
	switch s[19] {
	case '8', '9', 'a', 'b':
	default:
		return false
	}
	return true
}

// String renders the installation id as the canonical string.
func (id InstallationID) String() string { return string(id) }

// NodeID is the durable identifier of a node within a ProxyCore cluster.
// Format: UUIDv4 (RFC 4122) with hyphens, lowercase.
type NodeID string

// NewNodeID generates a fresh UUIDv4 node identifier.
func NewNodeID() NodeID {
	return NodeID(NewInstallationID())
}

// IsValid reports whether the node id is a syntactically valid UUIDv4.
func (id NodeID) IsValid() bool {
	return InstallationID(id).IsValid()
}

// String renders the node id as the canonical string.
func (id NodeID) String() string { return string(id) }

// LeadershipGeneration is the monotonically increasing counter that
// orders primaries over time. Snapshot acceptance and stale-primary
// detection both compare this counter.
type LeadershipGeneration uint64

// Next returns the next leadership generation, leaving the receiver
// untouched. It panics if the receiver is math.MaxUint64; callers must
// rotate the installation to a fresh generation rather than wrap.
func (g LeadershipGeneration) Next() LeadershipGeneration {
	if g == LeadershipGeneration(math.MaxUint64) {
		panic("LeadershipGeneration overflow")
	}
	return g + 1
}

// SnapshotVersion is the snapshot envelope version this installation
// understands. v1 is the only version defined at this point.
type SnapshotVersion uint32

const (
	// SnapshotVersionV1 is the initial snapshot envelope version.
	SnapshotVersionV1 SnapshotVersion = 1
)

// IsValid reports whether the snapshot version is non-zero.
func (v SnapshotVersion) IsValid() bool {
	return v != 0
}

// ReplicationVersion is the replication-control version embedded in a
// snapshot. It increments only on backward-incompatible changes to the
// replication envelope.
type ReplicationVersion uint32

const (
	// ReplicationVersionV1 is the initial replication version.
	ReplicationVersionV1 ReplicationVersion = 1
)

// IsValid reports whether the replication version is non-zero.
func (v ReplicationVersion) IsValid() bool {
	return v != 0
}
