package cluster

import (
	"errors"
	"testing"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
)

func TestStateMachineAllowsListedTransitions(t *testing.T) {
	sm := NewStateMachine()
	allowed := []struct {
		from domain.TopologyRole
		to   domain.TopologyRole
	}{
		{domain.TopologyRoleStandalone, domain.TopologyRoleNode},
		{domain.TopologyRolePrimary, domain.TopologyRolePrimaryWithNodes},
		{domain.TopologyRolePrimary, domain.TopologyRoleStalePrimary},
		{domain.TopologyRolePrimaryWithNodes, domain.TopologyRolePrimary},
		{domain.TopologyRolePrimaryWithNodes, domain.TopologyRoleStalePrimary},
		{domain.TopologyRoleNode, domain.TopologyRolePrimary},
		{domain.TopologyRoleStalePrimary, domain.TopologyRolePrimary},
		{domain.TopologyRoleStalePrimary, domain.TopologyRoleNode},
	}
	for _, tc := range allowed {
		err := sm.Transition(TransitionInput{
			From:           tc.from,
			To:             tc.to,
			FromGeneration: 1,
			ToGeneration:   2,
		})
		if err != nil {
			t.Errorf("expected %s -> %s allowed, got %v", tc.from, tc.to, err)
		}
	}
}

func TestStateMachineRejectsUnlistedTransitions(t *testing.T) {
	sm := NewStateMachine()
	rejected := []struct {
		from domain.TopologyRole
		to   domain.TopologyRole
	}{
		{domain.TopologyRoleStandalone, domain.TopologyRolePrimary},
		{domain.TopologyRoleStandalone, domain.TopologyRolePrimaryWithNodes},
		{domain.TopologyRoleStandalone, domain.TopologyRoleStalePrimary},
		{domain.TopologyRolePrimary, domain.TopologyRoleNode},
		{domain.TopologyRolePrimaryWithNodes, domain.TopologyRoleNode},
		{domain.TopologyRoleNode, domain.TopologyRoleStandalone},
		{domain.TopologyRoleNode, domain.TopologyRolePrimaryWithNodes},
		{domain.TopologyRoleNode, domain.TopologyRoleStalePrimary},
	}
	for _, tc := range rejected {
		err := sm.Transition(TransitionInput{
			From:           tc.from,
			To:             tc.to,
			FromGeneration: 1,
			ToGeneration:   2,
		})
		if err == nil {
			t.Errorf("expected %s -> %s rejected", tc.from, tc.to)
		}
		if !errors.Is(err, ErrInvalidStateTransition) {
			t.Errorf("expected ErrInvalidStateTransition, got %v", err)
		}
	}
}

func TestStateMachineAllowsSameState(t *testing.T) {
	sm := NewStateMachine()
	err := sm.Transition(TransitionInput{
		From:           domain.TopologyRolePrimary,
		To:             domain.TopologyRolePrimary,
		FromGeneration: 5,
		ToGeneration:   5,
	})
	if err != nil {
		t.Errorf("expected same-state transition to be a no-op, got %v", err)
	}
}

func TestStateMachineRejectsGenerationRegression(t *testing.T) {
	sm := NewStateMachine()
	err := sm.Transition(TransitionInput{
		From:           domain.TopologyRoleStalePrimary,
		To:             domain.TopologyRolePrimary,
		FromGeneration: 7,
		ToGeneration:   5,
	})
	if err == nil {
		t.Fatalf("expected ErrLeadershipRegression")
	}
	if !errors.Is(err, ErrLeadershipRegression) {
		t.Errorf("expected ErrLeadershipRegression, got %v", err)
	}
}

func TestStateMachineRejectsInvalidRoles(t *testing.T) {
	sm := NewStateMachine()
	err := sm.Transition(TransitionInput{
		From:           domain.TopologyRole("not-a-role"),
		To:             domain.TopologyRolePrimary,
		FromGeneration: 1,
		ToGeneration:   2,
	})
	if !errors.Is(err, ErrInvalidStateTransition) {
		t.Errorf("expected ErrInvalidStateTransition for invalid from, got %v", err)
	}
	err = sm.Transition(TransitionInput{
		From:           domain.TopologyRolePrimary,
		To:             domain.TopologyRole("not-a-role"),
		FromGeneration: 1,
		ToGeneration:   2,
	})
	if !errors.Is(err, ErrInvalidStateTransition) {
		t.Errorf("expected ErrInvalidStateTransition for invalid to, got %v", err)
	}
}

func TestStateMachineAllowedTransitionsForEachRole(t *testing.T) {
	sm := NewStateMachine()
	cases := []struct {
		role     domain.TopologyRole
		expected []domain.TopologyRole
	}{
		{domain.TopologyRoleStandalone, []domain.TopologyRole{domain.TopologyRoleNode}},
		{domain.TopologyRolePrimary, []domain.TopologyRole{domain.TopologyRolePrimaryWithNodes, domain.TopologyRoleStalePrimary}},
		{domain.TopologyRolePrimaryWithNodes, []domain.TopologyRole{domain.TopologyRolePrimary, domain.TopologyRoleStalePrimary}},
		{domain.TopologyRoleNode, []domain.TopologyRole{domain.TopologyRolePrimary}},
		{domain.TopologyRoleStalePrimary, []domain.TopologyRole{domain.TopologyRolePrimary, domain.TopologyRoleNode}},
	}
	for _, tc := range cases {
		got := sm.AllowedTransitions(tc.role)
		if len(got) != len(tc.expected) {
			t.Errorf("role=%s allowed=%v want %v", tc.role, got, tc.expected)
			continue
		}
		seen := make(map[domain.TopologyRole]struct{}, len(got))
		for _, role := range got {
			seen[role] = struct{}{}
		}
		for _, want := range tc.expected {
			if _, ok := seen[want]; !ok {
				t.Errorf("role=%s missing allowed transition %s", tc.role, want)
			}
		}
	}
}

func TestStateMachineAllowedTransitionsForInvalidRoleIsNil(t *testing.T) {
	sm := NewStateMachine()
	if got := sm.AllowedTransitions(domain.TopologyRole("invalid")); got != nil {
		t.Errorf("expected nil for invalid role, got %v", got)
	}
}
