package identity

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
)

// ErrAlreadyBootstrapped is returned by Service.EnsureBootstrapped when an
// existing identity is present and cannot be replaced by the caller. Use
// Service.Load when you want to attach to an existing identity.
var ErrAlreadyBootstrapped = errors.New("installation identity already bootstrapped")

// ErrInvalidTransition is returned by role transition methods when the
// requested transition is not allowed by the topology state machine.
var ErrInvalidTransition = errors.New("invalid topology role transition")

// ErrAutomaticRenewalNotPermitted is returned when the loaded identity is not
// an eligible writable standalone or PRIMARY role for automatic renewal.
var ErrAutomaticRenewalNotPermitted = errors.New("automatic renewal is not permitted")

// ErrAutomaticRenewalCallbackRequired is returned when no renewal work is
// supplied to the identity lease.
var ErrAutomaticRenewalCallbackRequired = errors.New("automatic renewal callback is required")

// ErrAutomaticRenewalCallbackPanic is returned when the renewal callback
// panics. The panic value is intentionally never exposed.
var ErrAutomaticRenewalCallbackPanic = errors.New("automatic renewal callback failed")

// Service is the in-memory identity coordinator. It loads the persisted
// identity at startup, caches it for cheap reads, and persists changes
// through the underlying Store.
type Service struct {
	store Store

	mu     sync.RWMutex
	cached Identity
	loaded bool
}

// NewService constructs a Service over the given Store. The identity is not
// loaded until Load or EnsureBootstrapped is called.
func NewService(store Store) *Service {
	return &Service{store: store}
}

// Load attaches to an existing identity. Returns ErrNotFound if the
// installation_identity row is missing; the caller should normally fall back
// to EnsureBootstrapped in that case.
func (s *Service) Load(ctx context.Context) (Identity, error) {
	id, err := s.store.Get(ctx)
	if err != nil {
		return Identity{}, err
	}
	s.mu.Lock()
	s.cached = id
	s.loaded = true
	s.mu.Unlock()
	return id, nil
}

// EnsureBootstrapped loads an existing identity or, when none exists,
// generates fresh identifiers and persists a singleton row with role
// standalone-primary and leadership generation 1.
//
// Calling EnsureBootstrapped on a fresh installation is the canonical boot
// path; calling it on an already-bootstrapped installation is a no-op.
func (s *Service) EnsureBootstrapped(ctx context.Context) (Identity, bool, error) {
	id, err := s.store.Get(ctx)
	if err == nil {
		s.mu.Lock()
		s.cached = id
		s.loaded = true
		s.mu.Unlock()
		return id, false, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return Identity{}, false, err
	}
	installationID := domain.NewInstallationID()
	nodeID := domain.NewNodeID()
	id, created, err := s.store.Ensure(ctx, installationID, nodeID)
	if err != nil {
		return Identity{}, false, fmt.Errorf("bootstrap installation identity: %w", err)
	}
	s.mu.Lock()
	s.cached = id
	s.loaded = true
	s.mu.Unlock()
	return id, created, nil
}

// Current returns the cached identity. Panics if the service has not been
// loaded or bootstrapped yet; callers MUST call Load or EnsureBootstrapped
// during startup before serving traffic.
func (s *Service) Current() Identity {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.loaded {
		panic("identity.Service.Current called before Load/EnsureBootstrapped")
	}
	return s.cached
}

// SnapshotPublicationIdentity returns the loaded identity without panicking.
// Snapshot publication fails closed until startup has loaded or bootstrapped
// the durable identity.
func (s *Service) SnapshotPublicationIdentity() (Identity, error) {
	if s == nil {
		return Identity{}, ErrIdentityNotLoaded
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.loaded {
		return Identity{}, ErrIdentityNotLoaded
	}
	return s.cached, nil
}

// WithAutomaticRenewalLease validates the live cached identity and runs one
// renewal operation while holding the identity read lock. Role transitions
// acquire the write side of this lock, so they cannot complete during the
// callback and the next work item observes the new role. The callback MUST NOT
// call identity mutation methods such as TransitionTo or ActivateEnrollment;
// reentrant write acquisition is unsupported. The callback must honor ctx.
func (s *Service) WithAutomaticRenewalLease(ctx context.Context, work func(context.Context) error) error {
	if s == nil || ctx == nil {
		return ErrIdentityNotLoaded
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if work == nil {
		return ErrAutomaticRenewalCallbackRequired
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if !s.loaded {
		return ErrIdentityNotLoaded
	}
	if !eligibleForAutomaticRenewal(s.cached) {
		return ErrAutomaticRenewalNotPermitted
	}
	return runAutomaticRenewalCallback(work, ctx)
}

func runAutomaticRenewalCallback(work func(context.Context) error, ctx context.Context) (err error) {
	defer func() {
		if recover() != nil {
			err = ErrAutomaticRenewalCallbackPanic
		}
	}()
	return work(ctx)
}

func eligibleForAutomaticRenewal(current Identity) bool {
	if current.IsStalePrimary() || !current.IsWritable() {
		return false
	}
	switch current.Role {
	case domain.TopologyRoleStandalone, domain.TopologyRolePrimary, domain.TopologyRolePrimaryWithNodes:
		return true
	default:
		return false
	}
}

// IsStalePrimary reports whether the cached identity is in the stale-primary
// guarded state. A stale-primary must keep serving its last valid local data
// plane but MUST block ordinary configuration writes.
func (s *Service) IsStalePrimary() bool {
	return s.Current().IsStalePrimary()
}

// IsWritable reports whether ordinary configuration mutations should be
// accepted by the API boundary given the cached identity.
func (s *Service) IsWritable() bool {
	return s.Current().IsWritable()
}

// TransitionTo attempts to move the local installation into the requested
// role. Returns ErrInvalidTransition when the transition is not allowed.
func (s *Service) TransitionTo(ctx context.Context, role domain.TopologyRole) (Identity, error) {
	if !role.IsValid() {
		return Identity{}, errors.New("invalid role: " + string(role))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	from := s.cached.Role
	if !isAllowedTransition(from, role) {
		return Identity{}, fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, from, role)
	}
	if err := s.store.UpdateRole(ctx, role); err != nil {
		return Identity{}, err
	}
	s.cached.Role = role
	return s.cached, nil
}

// RecordImportedSnapshot records that a snapshot with the given source
// primary id and leadership generation was applied locally. The service
// updates latest_known_generation when the observed generation is greater
// and returns the (possibly new) stale-primary state.
//
// If the observed generation is strictly greater than the cached
// leadership generation, the service moves the local installation into
// stale-primary guarded state; the operator must explicitly recover.
func (s *Service) RecordImportedSnapshot(ctx context.Context, sourcePrimary uuid.UUID, observedGeneration domain.LeadershipGeneration) (Identity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	previouslyKnown := s.cached.LatestKnownGeneration
	ownGeneration := s.cached.LeadershipGeneration
	role := s.cached.Role

	if err := s.store.UpdateLatestKnownGeneration(ctx, observedGeneration); err != nil {
		return Identity{}, err
	}
	if observedGeneration > s.cached.LatestKnownGeneration {
		s.cached.LatestKnownGeneration = observedGeneration
	}
	// Move to stale-primary only when we observed a generation that is
	// strictly newer than what we had ever seen AND strictly newer than our
	// own leadership generation. Re-observing an older generation must not
	// demote a recovered primary back into stale-primary.
	shouldEnterStalePrimary := observedGeneration > previouslyKnown &&
		observedGeneration > ownGeneration &&
		role != domain.TopologyRoleStalePrimary
	if shouldEnterStalePrimary {
		if err := s.store.UpdateRole(ctx, domain.TopologyRoleStalePrimary); err != nil {
			return Identity{}, err
		}
		s.cached.Role = domain.TopologyRoleStalePrimary
	}
	return s.cached, nil
}

// PromoteToPrimary rotates the leadership generation to the next value and
// moves the installation into RolePrimary. Phase 4 implements the full
// promotion workflow; this method is the contract-level entry point used by
// Phase 0/1 tests and is intentionally minimal.
func (s *Service) PromoteToPrimary(ctx context.Context) (Identity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	current := s.cached
	if current.Role != domain.TopologyRoleNode &&
		current.Role != domain.TopologyRoleStalePrimary &&
		current.Role != domain.TopologyRoleStandalone {
		return Identity{}, fmt.Errorf("%w: cannot promote from %s", ErrInvalidTransition, current.Role)
	}
	next := current.LeadershipGeneration.Next()
	if err := s.store.UpdateLeadershipGeneration(ctx, next); err != nil {
		return Identity{}, err
	}
	if err := s.store.UpdateRole(ctx, domain.TopologyRolePrimary); err != nil {
		return Identity{}, err
	}
	s.cached.Role = domain.TopologyRolePrimary
	s.cached.LeadershipGeneration = next
	if next > s.cached.LatestKnownGeneration {
		s.cached.LatestKnownGeneration = next
	}
	return s.cached, nil
}

// isAllowedTransition encodes the Phase 0/1 transition matrix. The full
// Phase 4 promotion matrix is a superset; this matrix is intentionally
// conservative so a later work unit can extend it without revisiting the
// guard logic.
func isAllowedTransition(from, to domain.TopologyRole) bool {
	switch from {
	case domain.TopologyRoleStandalone:
		return to == domain.TopologyRoleNode
	case domain.TopologyRolePrimary:
		return to == domain.TopologyRolePrimaryWithNodes ||
			to == domain.TopologyRoleStalePrimary
	case domain.TopologyRolePrimaryWithNodes:
		return to == domain.TopologyRolePrimary ||
			to == domain.TopologyRoleStalePrimary
	case domain.TopologyRoleNode:
		return to == domain.TopologyRolePrimary
	case domain.TopologyRoleStalePrimary:
		return to == domain.TopologyRolePrimary ||
			to == domain.TopologyRoleNode
	}
	return false
}
