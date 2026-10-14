package identity

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
)

var (
	// ErrGenerationMismatch is returned when the caller's expected leadership
	// generation does not match the durable one. It guards promotion and
	// demotion against acting on a stale view of the topology.
	ErrGenerationMismatch = errors.New("leadership generation does not match the expected generation")
	// ErrTopologyWorkflowUnavailable is returned when the backing store cannot
	// run the transactional promote/demote workflow.
	ErrTopologyWorkflowUnavailable = errors.New("topology workflow is unavailable")
)

// PromoteOptions parameterizes the transactional promotion workflow.
type PromoteOptions struct {
	// ActorUserID is recorded on the audit event; empty means no actor.
	ActorUserID string
	// ExpectedGeneration, when non-nil, must equal the current leadership
	// generation or the promotion is refused with ErrGenerationMismatch.
	ExpectedGeneration *domain.LeadershipGeneration
}

// DemoteOptions parameterizes the transactional PRIMARY -> NODE demotion.
type DemoteOptions struct {
	ActorUserID        string
	ExpectedGeneration *domain.LeadershipGeneration
}

// PromoteInput is what the service hands the store: the validated source
// state and the generation the store must persist. The store re-checks the
// source state inside its transaction.
type PromoteInput struct {
	ActorUserID    string
	FromRole       domain.TopologyRole
	FromGeneration domain.LeadershipGeneration
	NextGeneration domain.LeadershipGeneration
}

// DemoteInput is the demotion counterpart of PromoteInput. Demotion keeps the
// leadership generation unchanged.
type DemoteInput struct {
	ActorUserID    string
	FromRole       domain.TopologyRole
	FromGeneration domain.LeadershipGeneration
}

// TopologyChange reports the durable outcome of one workflow.
type TopologyChange struct {
	Role         domain.TopologyRole
	Generation   domain.LeadershipGeneration
	ClusterKeyID *uuid.UUID
	// RevokedCredentials counts node credentials revoked by the workflow.
	RevokedCredentials int
}

// TopologyStore is the optional persistence port for the transactional
// promote/demote workflows. Implementations MUST apply every effect (role,
// generation, cluster key retirement, credential revocation, relationship
// reset, audit record) atomically or none of them.
type TopologyStore interface {
	PromoteTopology(ctx context.Context, in PromoteInput) (TopologyChange, error)
	DemoteTopology(ctx context.Context, in DemoteInput) (TopologyChange, error)
}

// PromoteResult is the cache-consistent outcome of a promotion.
type PromoteResult struct {
	Identity           Identity
	PreviousRole       domain.TopologyRole
	PreviousGeneration domain.LeadershipGeneration
	RevokedCredentials int
}

// DemoteResult is the cache-consistent outcome of a demotion.
type DemoteResult struct {
	Identity           Identity
	PreviousRole       domain.TopologyRole
	RevokedCredentials int
}

func promotableFrom(role domain.TopologyRole) bool {
	return role == domain.TopologyRoleNode ||
		role == domain.TopologyRoleStalePrimary ||
		role == domain.TopologyRoleStandalone
}

// Promote runs the full promotion workflow: bump the leadership generation,
// become PRIMARY, retire the cluster key (a fresh one is minted on first use),
// revoke every node credential and reset the primary relationship, and write
// an audit record. Nodes of the former primary are not repointed; they must
// be re-enrolled against this installation.
func (s *Service) Promote(ctx context.Context, opts PromoteOptions) (PromoteResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	current := s.cached
	if !promotableFrom(current.Role) {
		return PromoteResult{}, fmt.Errorf("%w: cannot promote from %s", ErrInvalidTransition, current.Role)
	}
	if opts.ExpectedGeneration != nil && *opts.ExpectedGeneration != current.LeadershipGeneration {
		return PromoteResult{}, fmt.Errorf("%w: expected %d, current %d",
			ErrGenerationMismatch, *opts.ExpectedGeneration, current.LeadershipGeneration)
	}
	next := current.LeadershipGeneration.Next()
	if latest := current.LatestKnownGeneration; latest >= next {
		// Never promote below a generation already observed elsewhere.
		next = latest.Next()
	}
	result := PromoteResult{PreviousRole: current.Role, PreviousGeneration: current.LeadershipGeneration}

	topology, transactional := s.store.(TopologyStore)
	if !transactional {
		if err := s.store.UpdateLeadershipGeneration(ctx, next); err != nil {
			return PromoteResult{}, err
		}
		if err := s.store.UpdateRole(ctx, domain.TopologyRolePrimary); err != nil {
			return PromoteResult{}, err
		}
		s.cached.Role = domain.TopologyRolePrimary
		s.cached.LeadershipGeneration = next
		if next > s.cached.LatestKnownGeneration {
			s.cached.LatestKnownGeneration = next
		}
		result.Identity = s.cached
		return result, nil
	}

	change, err := topology.PromoteTopology(ctx, PromoteInput{
		ActorUserID:    opts.ActorUserID,
		FromRole:       current.Role,
		FromGeneration: current.LeadershipGeneration,
		NextGeneration: next,
	})
	if err != nil {
		return PromoteResult{}, err
	}
	s.cached.Role = domain.TopologyRolePrimary
	s.cached.LeadershipGeneration = change.Generation
	if change.Generation > s.cached.LatestKnownGeneration {
		s.cached.LatestKnownGeneration = change.Generation
	}
	s.cached.ClusterKeyID = change.ClusterKeyID
	result.Identity = s.cached
	result.RevokedCredentials = change.RevokedCredentials
	return result, nil
}

// Demote runs the PRIMARY -> NODE workflow: revoke every node credential this
// primary issued, reset the primary relationship, and move to the NODE role so
// the enrollment flow can attach this installation to another primary.
func (s *Service) Demote(ctx context.Context, opts DemoteOptions) (DemoteResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	current := s.cached
	if current.Role != domain.TopologyRolePrimary && current.Role != domain.TopologyRolePrimaryWithNodes {
		return DemoteResult{}, fmt.Errorf("%w: cannot demote from %s", ErrInvalidTransition, current.Role)
	}
	if opts.ExpectedGeneration != nil && *opts.ExpectedGeneration != current.LeadershipGeneration {
		return DemoteResult{}, fmt.Errorf("%w: expected %d, current %d",
			ErrGenerationMismatch, *opts.ExpectedGeneration, current.LeadershipGeneration)
	}
	topology, transactional := s.store.(TopologyStore)
	if !transactional {
		return DemoteResult{}, ErrTopologyWorkflowUnavailable
	}
	change, err := topology.DemoteTopology(ctx, DemoteInput{
		ActorUserID:    opts.ActorUserID,
		FromRole:       current.Role,
		FromGeneration: current.LeadershipGeneration,
	})
	if err != nil {
		return DemoteResult{}, err
	}
	previous := current.Role
	s.cached.Role = domain.TopologyRoleNode
	s.cached.ClusterKeyID = change.ClusterKeyID
	return DemoteResult{Identity: s.cached, PreviousRole: previous, RevokedCredentials: change.RevokedCredentials}, nil
}
