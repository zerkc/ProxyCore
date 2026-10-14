package identity

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

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

func TestServiceWithAtomicEnrollmentProvisionsClusterKeyBeforeActivation(t *testing.T) {
	store := newFakeStore()
	svc := NewService(store)
	if _, _, err := svc.EnsureBootstrapped(context.Background()); err != nil {
		t.Fatalf("EnsureBootstrapped: %v", err)
	}
	key := uuid.New()
	calls := 0
	svc.SetClusterKeyProvisioner(func(context.Context) (uuid.UUID, error) {
		calls++
		return key, nil
	})
	activate := func(ctx context.Context) (domain.TopologyRole, error) {
		return domain.TopologyRolePrimary, store.UpdateRole(ctx, domain.TopologyRolePrimary)
	}
	id, err := svc.WithAtomicEnrollment(context.Background(), activate)
	if err != nil {
		t.Fatalf("WithAtomicEnrollment: %v", err)
	}
	if id.ClusterKeyID == nil || *id.ClusterKeyID != key {
		t.Fatalf("returned cluster key = %v, want %s", id.ClusterKeyID, key)
	}
	stored, _ := store.Get(context.Background())
	if stored.ClusterKeyID == nil || *stored.ClusterKeyID != key {
		t.Fatalf("persisted cluster key = %v, want %s", stored.ClusterKeyID, key)
	}
	// A second token on the same primary reuses the key.
	if _, err := svc.WithAtomicEnrollment(context.Background(), activate); err != nil || calls != 1 {
		t.Fatalf("second activation err=%v provisioner calls=%d", err, calls)
	}
}

func TestServiceWithAtomicEnrollmentProvisionFailureAbortsActivation(t *testing.T) {
	store := newFakeStore()
	svc := NewService(store)
	if _, _, err := svc.EnsureBootstrapped(context.Background()); err != nil {
		t.Fatalf("EnsureBootstrapped: %v", err)
	}
	cause := errors.New("key store unavailable")
	svc.SetClusterKeyProvisioner(func(context.Context) (uuid.UUID, error) { return uuid.Nil, cause })
	activated := false
	_, err := svc.WithAtomicEnrollment(context.Background(), func(context.Context) (domain.TopologyRole, error) {
		activated = true
		return domain.TopologyRolePrimary, nil
	})
	if !errors.Is(err, cause) || activated {
		t.Fatalf("err=%v activated=%t, want provisioning error before activation", err, activated)
	}
	if svc.Current().Role != domain.TopologyRoleStandalone || svc.Current().ClusterKeyID != nil {
		t.Fatalf("cache changed after failed provisioning: %#v", svc.Current())
	}
}
