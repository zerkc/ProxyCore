package identity

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
)

func TestServiceWithAtomicEnrollmentUpdatesLiveCache(t *testing.T) {
	store := newFakeStore()
	svc := NewService(store)
	if _, _, err := svc.EnsureBootstrapped(context.Background()); err != nil {
		t.Fatalf("EnsureBootstrapped: %v", err)
	}
	id, err := svc.WithAtomicEnrollment(context.Background(), func(ctx context.Context) (domain.TopologyRole, error) {
		if err := store.UpdateRole(ctx, domain.TopologyRolePrimary); err != nil {
			return "", err
		}
		return domain.TopologyRolePrimary, nil
	})
	if err != nil {
		t.Fatalf("WithAtomicEnrollment: %v", err)
	}
	if id.Role != domain.TopologyRolePrimary || svc.Current().Role != domain.TopologyRolePrimary {
		t.Fatalf("live identity role = %s, want primary", svc.Current().Role)
	}
	if id.UpdatedAt.IsZero() {
		t.Fatal("successful activation did not update UpdatedAt")
	}
}

func TestServiceWithAtomicEnrollmentFailureKeepsPreloadedStandalone(t *testing.T) {
	preloaded := Identity{
		InstallationID:        domain.NewInstallationID(),
		NodeID:                domain.NewNodeID(),
		Role:                  domain.TopologyRoleStandalone,
		LeadershipGeneration:  1,
		LatestKnownGeneration: 1,
		UpdatedAt:             time.Date(2026, 10, 2, 3, 4, 5, 0, time.UTC),
	}
	store := newFakeStore()
	store.seed(preloaded)
	svc := NewService(store)
	if _, err := svc.Load(context.Background()); err != nil {
		t.Fatalf("Load: %v", err)
	}
	cause := errors.New("transaction rolled back")
	if _, err := svc.WithAtomicEnrollment(context.Background(), func(context.Context) (domain.TopologyRole, error) {
		return "", cause
	}); !errors.Is(err, cause) {
		t.Fatalf("WithAtomicEnrollment error = %v, want callback error", err)
	}
	got := svc.Current()
	if got.Role != domain.TopologyRoleStandalone || !got.UpdatedAt.Equal(preloaded.UpdatedAt) {
		t.Fatalf("preloaded identity after failure = %#v", got)
	}
}

func TestServiceWithAtomicEnrollmentInvalidRoleRefreshesCache(t *testing.T) {
	store := newFakeStore()
	svc := NewService(store)
	if _, _, err := svc.EnsureBootstrapped(context.Background()); err != nil {
		t.Fatalf("EnsureBootstrapped: %v", err)
	}
	_, err := svc.WithAtomicEnrollment(context.Background(), func(ctx context.Context) (domain.TopologyRole, error) {
		if err := store.UpdateRole(ctx, domain.TopologyRolePrimary); err != nil {
			return "", err
		}
		return domain.TopologyRoleNode, nil
	})
	if !errors.Is(err, ErrInvalidEnrollmentActivation) {
		t.Fatalf("invalid activation error = %v", err)
	}
	if got := svc.Current().Role; got != domain.TopologyRolePrimary {
		t.Fatalf("cache role after invalid callback = %s, want primary", got)
	}
}

func TestServiceWithAtomicEnrollmentBlocksReadsUntilCacheUpdated(t *testing.T) {
	store := newFakeStore()
	svc := NewService(store)
	if _, _, err := svc.EnsureBootstrapped(context.Background()); err != nil {
		t.Fatalf("EnsureBootstrapped: %v", err)
	}
	durable := make(chan struct{})
	release := make(chan struct{})
	activationDone := make(chan error, 1)
	go func() {
		_, err := svc.WithAtomicEnrollment(context.Background(), func(ctx context.Context) (domain.TopologyRole, error) {
			if err := store.UpdateRole(ctx, domain.TopologyRolePrimary); err != nil {
				return "", err
			}
			close(durable)
			<-release
			return domain.TopologyRolePrimary, nil
		})
		activationDone <- err
	}()
	<-durable
	if svc.mu.TryRLock() {
		svc.mu.RUnlock()
		t.Fatal("Current could read while durable activation awaited cache update")
	}
	close(release)
	if err := <-activationDone; err != nil {
		t.Fatalf("WithAtomicEnrollment: %v", err)
	}
	if got := svc.Current().Role; got != domain.TopologyRolePrimary {
		t.Fatalf("role after activation = %s, want primary", got)
	}
}
