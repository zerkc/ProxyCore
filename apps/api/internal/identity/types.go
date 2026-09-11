// Package identity models the durable PRIMARY/NODE identity of a ProxyCore
// installation: installation ID, node ID, role, and leadership generation.
//
// The package follows a small port pattern so the service can be exercised
// against an in-memory store in unit tests; the Postgres-backed store is the
// production implementation.
package identity

import (
	"time"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
)

// Identity is the durable state of one installation's topology role.
type Identity struct {
	// InstallationID is the UUIDv4 assigned at first boot. Stable for the
	// lifetime of the installation.
	InstallationID domain.InstallationID
	// NodeID is the UUIDv4 assigned at first boot. Stable across the same
	// installation; on promotion the same NodeID is preserved.
	NodeID domain.NodeID
	// Role is the durable topology role of the installation.
	Role domain.TopologyRole
	// LeadershipGeneration is the generation this installation currently
	// recognizes for itself.
	LeadershipGeneration domain.LeadershipGeneration
	// LatestKnownGeneration is the largest leadership generation this
	// installation has ever observed locally or via an imported snapshot.
	LatestKnownGeneration domain.LeadershipGeneration
	// ClusterKeyID points to the active cluster KEK row, if any. Nil for
	// fresh installations and standalone primaries that have not enrolled.
	ClusterKeyID *uuid.UUID
	// UpdatedAt is the last write timestamp recorded by the store.
	UpdatedAt time.Time
}

// IsStalePrimary reports whether the installation is in the stale-primary
// guarded state: it observed a newer leadership generation elsewhere but its
// own leadership generation has not yet caught up.
func (i Identity) IsStalePrimary() bool {
	return i.Role == domain.TopologyRoleStalePrimary ||
		i.LeadershipGeneration < i.LatestKnownGeneration
}

// IsWritable reports whether ordinary configuration mutations should be
// accepted. Stale-primary and node roles block writes.
func (i Identity) IsWritable() bool {
	return i.Role == domain.TopologyRoleStandalone ||
		i.Role == domain.TopologyRolePrimary ||
		i.Role == domain.TopologyRolePrimaryWithNodes
}
