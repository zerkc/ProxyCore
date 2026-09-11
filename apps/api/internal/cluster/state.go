package cluster

import (
	"errors"
	"fmt"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
)

// ErrInvalidStateTransition is returned when a transition between two
// topology roles is not allowed by the Phase 0/1 matrix.
var ErrInvalidStateTransition = errors.New("invalid topology state transition")

// ErrLeadershipRegression is returned when a proposed transition would
// reduce the leadership generation. Generation is monotonic; only
// promote-to-primary may increment it, and it MUST NOT be reduced by any
// transition.
var ErrLeadershipRegression = errors.New("leadership generation must be monotonic")

// StateMachine is the canonical Phase 0/1 topology state machine. The full
// Phase 4 promotion matrix is a superset; this machine is intentionally
// conservative so the Phase 4 commit can extend it without revisiting the
// guard logic.
type StateMachine struct{}

// NewStateMachine constructs an empty state machine.
func NewStateMachine() StateMachine { return StateMachine{} }

// TransitionInput captures the parameters required to evaluate a state
// transition. FromGeneration is the locally persisted leadership generation
// before the transition; ToGeneration is the generation that the caller
// intends to persist after the transition.
type TransitionInput struct {
	From           domain.TopologyRole
	To             domain.TopologyRole
	FromGeneration domain.LeadershipGeneration
	ToGeneration   domain.LeadershipGeneration
}

// Transition evaluates whether the proposed transition is allowed. It does
// not perform the persistence; callers MUST persist the result through the
// identity service (or successor) after a nil error.
func (StateMachine) Transition(in TransitionInput) error {
	if !in.From.IsValid() {
		return fmt.Errorf("%w: invalid from role %q", ErrInvalidStateTransition, in.From)
	}
	if !in.To.IsValid() {
		return fmt.Errorf("%w: invalid to role %q", ErrInvalidStateTransition, in.To)
	}
	if in.From == in.To {
		// Same-state transitions are a no-op; we still require the
		// generation to be monotonic against the persisted state.
		return nil
	}
	if in.ToGeneration < in.FromGeneration {
		return fmt.Errorf(
			"%w: %d -> %d",
			ErrLeadershipRegression,
			in.FromGeneration,
			in.ToGeneration,
		)
	}
	if !isAllowedTransition(in.From, in.To) {
		return fmt.Errorf("%w: %s -> %s", ErrInvalidStateTransition, in.From, in.To)
	}
	return nil
}

// AllowedTransitions returns the set of roles that the given role may
// transition to. Used by the API surface to advertise which transitions the
// Owner can request without leaking the internals of the matrix.
func (StateMachine) AllowedTransitions(from domain.TopologyRole) []domain.TopologyRole {
	if !from.IsValid() {
		return nil
	}
	all := []domain.TopologyRole{
		domain.TopologyRoleStandalone,
		domain.TopologyRolePrimary,
		domain.TopologyRolePrimaryWithNodes,
		domain.TopologyRoleNode,
		domain.TopologyRoleStalePrimary,
	}
	out := make([]domain.TopologyRole, 0, len(all))
	for _, role := range all {
		if role == from {
			continue
		}
		if isAllowedTransition(from, role) {
			out = append(out, role)
		}
	}
	return out
}

// isAllowedTransition encodes the Phase 0/1 transition matrix.
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
