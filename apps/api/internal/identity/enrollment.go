package identity

import (
	"context"
	"errors"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
)

var (
	ErrIdentityNotLoaded           = errors.New("installation identity is not loaded")
	ErrInvalidEnrollmentActivation = errors.New("invalid enrollment activation role")
)

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
