package identity

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
)

var (
	ErrIdentityNotLoaded           = errors.New("installation identity is not loaded")
	ErrInvalidEnrollmentActivation = errors.New("invalid enrollment activation role")
)

// SetClusterKeyProvisioner registers the hook that mints (or loads) the
// cluster KEK a PRIMARY needs before it can publish a snapshot or serve the
// enrollment snapshot-by-token read. It is invoked, under the identity lock,
// the first time enrollment is activated on a PRIMARY-capable identity that has
// no cluster key yet. Call it during startup, before serving traffic.
func (s *Service) SetClusterKeyProvisioner(provision func(context.Context) (uuid.UUID, error)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.provisionClusterKey = provision
}

// ensureClusterKeyLocked persists a cluster key id for a PRIMARY-capable
// identity that has none. Callers must hold s.mu for writing.
func (s *Service) ensureClusterKeyLocked(ctx context.Context) error {
	if s.provisionClusterKey == nil || s.cached.ClusterKeyID != nil {
		return nil
	}
	switch s.cached.Role {
	case domain.TopologyRoleStandalone, domain.TopologyRolePrimary, domain.TopologyRolePrimaryWithNodes:
	default:
		return nil
	}
	id, err := s.provisionClusterKey(ctx)
	if err != nil {
		return err
	}
	if id == uuid.Nil {
		return ErrInvalidEnrollmentActivation
	}
	if err := s.store.UpdateClusterKeyID(ctx, &id); err != nil {
		return err
	}
	s.cached.ClusterKeyID = &id
	return nil
}

// WithAtomicEnrollment holds the identity lock through the durable enrollment
// callback and the cache update. An invalid callback result refreshes the
// cache, or fails closed as stale-primary if that refresh is unavailable.
func (s *Service) WithAtomicEnrollment(ctx context.Context, activate func(context.Context) (domain.TopologyRole, error)) (Identity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.loaded {
		return Identity{}, ErrIdentityNotLoaded
	}
	if activate == nil {
		return Identity{}, ErrInvalidEnrollmentActivation
	}
	if err := s.ensureClusterKeyLocked(ctx); err != nil {
		return Identity{}, err
	}
	role, err := activate(ctx)
	if err != nil {
		return Identity{}, err
	}
	if role != domain.TopologyRolePrimary && role != domain.TopologyRolePrimaryWithNodes {
		if current, refreshErr := s.store.Get(ctx); refreshErr == nil {
			s.cached = current
		} else {
			s.cached.Role = domain.TopologyRoleStalePrimary
		}
		return Identity{}, ErrInvalidEnrollmentActivation
	}
	s.cached.Role = role
	s.cached.UpdatedAt = time.Now().UTC()
	return s.cached, nil
}

// ActivateEnrollment adapts the cache bridge to enrollment's narrow role port.
func (s *Service) ActivateEnrollment(ctx context.Context, activate func(context.Context) (domain.TopologyRole, error)) (domain.TopologyRole, error) {
	id, err := s.WithAtomicEnrollment(ctx, activate)
	if err != nil {
		return "", err
	}
	return id.Role, nil
}

// ErrEnrollmentIneligible means the identity may not be enrolled as a NODE in
// its current role/binding.
var ErrEnrollmentIneligible = errors.New("identity cannot be enrolled as node")

// CommitNodeEnrollment is the commit point of an enrollment conversion: it
// moves the installation to NODE and records the imported cluster key so the
// local KEK loader resolves the key the primary delivered. The role is written
// first; a crash before the key write leaves an unbound NODE, which is
// eligible for a retry, never a primary pointing at a foreign key. A nil
// clusterKeyID only moves the role. observedGeneration is the leadership
// generation of the primary being joined: the NODE adopts it (never lowering
// its own) so a later promotion always lands above that primary's generation.
func (s *Service) CommitNodeEnrollment(ctx context.Context, clusterKeyID *uuid.UUID, observedGeneration domain.LeadershipGeneration) (Identity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.loaded {
		return Identity{}, ErrIdentityNotLoaded
	}
	if clusterKeyID != nil && *clusterKeyID == uuid.Nil {
		return Identity{}, ErrInvalidEnrollmentActivation
	}
	current := s.cached
	alreadyCommitted := current.Role == domain.TopologyRoleNode && clusterKeyID != nil &&
		current.ClusterKeyID != nil && *current.ClusterKeyID == *clusterKeyID &&
		current.LeadershipGeneration >= observedGeneration
	if alreadyCommitted {
		return current, nil
	}
	if !current.CanEnrollAsNode() {
		return Identity{}, ErrEnrollmentIneligible
	}
	if current.Role != domain.TopologyRoleNode {
		if !isAllowedTransition(current.Role, domain.TopologyRoleNode) {
			return Identity{}, ErrInvalidTransition
		}
		if err := s.store.UpdateRole(ctx, domain.TopologyRoleNode); err != nil {
			return Identity{}, err
		}
		s.cached.Role = domain.TopologyRoleNode
	}
	if observedGeneration > s.cached.LeadershipGeneration {
		if err := s.store.UpdateLeadershipGeneration(ctx, observedGeneration); err != nil {
			return Identity{}, err
		}
		s.cached.LeadershipGeneration = observedGeneration
		if observedGeneration > s.cached.LatestKnownGeneration {
			s.cached.LatestKnownGeneration = observedGeneration
		}
	}
	if clusterKeyID != nil {
		key := *clusterKeyID
		if err := s.store.UpdateClusterKeyID(ctx, &key); err != nil {
			return Identity{}, err
		}
		s.cached.ClusterKeyID = &key
	}
	return s.cached, nil
}
