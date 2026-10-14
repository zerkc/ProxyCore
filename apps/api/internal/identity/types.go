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
	// NodeUnlinked is an in-memory overlay (never persisted here): the
	// persisted node-sync state says the primary explicitly unlinked this
	// NODE. It only relaxes the "bound NODE must not re-enroll" refusal.
	// Service.SetNodeUnlinked is its only writer; it is restored from the
	// persisted sync state at startup, so a store error leaves it false.
	NodeUnlinked bool
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

// RoleCanEnrollAsNode reports whether an installation with the given role may
// be enrolled against a primary (converted to NODE) right now. hasClusterKey
// says whether the installation currently points at a cluster KEK.
//
//   - standalone-primary, primary and stale-primary may enroll; their own
//     cluster key (if any) is replaced by the one the grant delivers;
//   - a NODE may enroll only while it is unbound. Demotion clears
//     cluster_key_id, so a demoted installation is exactly "node without a
//     cluster key"; a NODE that already holds a cluster key is bound to a
//     primary and must not be silently re-pointed;
//   - primary-with-nodes must be demoted first so its nodes lose their
//     credentials through the audited workflow.
func RoleCanEnrollAsNode(role domain.TopologyRole, hasClusterKey bool) bool {
	switch role {
	case domain.TopologyRoleStandalone, domain.TopologyRolePrimary, domain.TopologyRoleStalePrimary:
		return true
	case domain.TopologyRoleNode:
		return !hasClusterKey
	default:
		return false
	}
}

// RoleCanReEnrollAsNode is RoleCanEnrollAsNode plus one exception: a NODE the
// primary explicitly unlinked (unlinked=true) may enroll again even though it
// still holds its cluster key, because that key is what keeps it serving. A
// linked NODE (unlinked=false) keeps the original refusal.
func RoleCanReEnrollAsNode(role domain.TopologyRole, hasClusterKey, unlinked bool) bool {
	if role == domain.TopologyRoleNode && unlinked {
		return true
	}
	return RoleCanEnrollAsNode(role, hasClusterKey)
}

// CanEnrollAsNode applies RoleCanReEnrollAsNode to the identity.
func (i Identity) CanEnrollAsNode() bool {
	return RoleCanReEnrollAsNode(i.Role, i.ClusterKeyID != nil, i.NodeUnlinked)
}

// SetNodeUnlinked records (or clears) the unlinked overlay on the cached
// identity. It never touches the store and never changes role or key.
func (s *Service) SetNodeUnlinked(unlinked bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cached.NodeUnlinked = unlinked
}
