package auth

import (
	"context"
	"errors"
)

// UserPatch holds optional user fields to update.
type UserPatch struct {
	Role         *Role
	Active       *bool
	PasswordHash *string
}

// ErrUserNotFound is returned when a target user does not exist.
var ErrUserNotFound = errors.New("User not found")

// ListUsers returns all users (handlers gate this to owners).
func (s *Service) ListUsers(ctx context.Context) ([]User, error) {
	return s.store.ListUsers(ctx)
}

// CreateUser creates a new user acting as the given owner.
func (s *Service) CreateUser(ctx context.Context, actor User, username, password string, role Role) (User, error) {
	user, err := s.buildUser(username, password, role)
	if err != nil {
		return User{}, err
	}
	created, err := s.store.CreateUser(ctx, user)
	if err != nil {
		return User{}, err
	}
	actorID := actor.ID
	_ = s.audit(ctx, "user.create", "user", &created.ID, &actorID, "success", map[string]any{"role": created.Role})
	return created, nil
}

// UpdateUser updates a user, protecting the last active owner.
func (s *Service) UpdateUser(ctx context.Context, actor User, userID string, patch UserPatch) (User, error) {
	target, err := s.store.FindUserByID(ctx, userID)
	if err != nil {
		return User{}, err
	}
	if target == nil {
		return User{}, ErrUserNotFound
	}
	demoting := (patch.Role != nil && *patch.Role == RoleOperator) || (patch.Active != nil && !*patch.Active)
	if target.Role == RoleOwner && demoting {
		count, err := s.activeOwnerCount(ctx)
		if err != nil {
			return User{}, err
		}
		if count <= 1 {
			return User{}, errors.New("Cannot remove or demote the last Owner")
		}
	}

	storePatch := UserPatch{Role: patch.Role, Active: patch.Active}
	if patch.PasswordHash != nil {
		storePatch.PasswordHash = patch.PasswordHash
	}
	updated, err := s.store.UpdateUser(ctx, userID, storePatch)
	if err != nil {
		return User{}, err
	}
	actorID := actor.ID
	_ = s.audit(ctx, "user.update", "user", &userID, &actorID, "success", nil)
	return updated, nil
}

// DeleteUser removes a user, protecting the last active owner.
func (s *Service) DeleteUser(ctx context.Context, actor User, userID string) error {
	target, err := s.store.FindUserByID(ctx, userID)
	if err != nil {
		return err
	}
	if target == nil {
		return ErrUserNotFound
	}
	if target.Role == RoleOwner {
		count, err := s.activeOwnerCount(ctx)
		if err != nil {
			return err
		}
		if count <= 1 {
			return errors.New("Cannot delete the last Owner")
		}
	}
	if err := s.store.DeleteUser(ctx, userID); err != nil {
		return err
	}
	actorID := actor.ID
	_ = s.audit(ctx, "user.delete", "user", &userID, &actorID, "success", nil)
	return nil
}

// HashPasswordForUpdate hashes a password for a patch or returns an error.
func HashPasswordForUpdate(password string) (string, error) {
	return HashPassword(password)
}

func (s *Service) activeOwnerCount(ctx context.Context) (int, error) {
	users, err := s.store.ListUsers(ctx)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, user := range users {
		if user.Active && user.Role == RoleOwner {
			count++
		}
	}
	return count, nil
}
