package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/identity"
)

type startupIdentityStore struct {
	current identity.Identity
	present bool
}

func (s *startupIdentityStore) Get(context.Context) (identity.Identity, error) {
	if !s.present {
		return identity.Identity{}, identity.ErrNotFound
	}
	return s.current, nil
}

func (s *startupIdentityStore) Ensure(_ context.Context, installationID domain.InstallationID, nodeID domain.NodeID) (identity.Identity, bool, error) {
	if s.present {
		return s.current, false, nil
	}
	s.current = identity.Identity{
		InstallationID:        installationID,
		NodeID:                nodeID,
		Role:                  domain.TopologyRoleStandalone,
		LeadershipGeneration:  1,
		LatestKnownGeneration: 1,
	}
	s.present = true
	return s.current, true, nil
}

func (s *startupIdentityStore) UpdateRole(_ context.Context, role domain.TopologyRole) error {
	if !s.present {
		return identity.ErrNotFound
	}
	s.current.Role = role
	return nil
}

func (s *startupIdentityStore) UpdateLeadershipGeneration(_ context.Context, generation domain.LeadershipGeneration) error {
	if !s.present {
		return identity.ErrNotFound
	}
	s.current.LeadershipGeneration = generation
	if generation > s.current.LatestKnownGeneration {
		s.current.LatestKnownGeneration = generation
	}
	return nil
}

func (s *startupIdentityStore) UpdateLatestKnownGeneration(_ context.Context, generation domain.LeadershipGeneration) error {
	if !s.present {
		return identity.ErrNotFound
	}
	if generation > s.current.LatestKnownGeneration {
		s.current.LatestKnownGeneration = generation
	}
	return nil
}

func (s *startupIdentityStore) UpdateClusterKeyID(_ context.Context, keyID *uuid.UUID) error {
	if !s.present {
		return identity.ErrNotFound
	}
	s.current.ClusterKeyID = keyID
	return nil
}

func TestIsNormalRuntimeCloseClassifiesExpectedStops(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", want: true},
		{name: "context canceled", err: context.Canceled, want: true},
		{name: "http closed", err: http.ErrServerClosed, want: true},
		{name: "network closed", err: net.ErrClosed, want: true},
		{name: "unexpected", err: errors.New("unexpected server failure"), want: false},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := isNormalRuntimeClose(tt.err); got != tt.want {
				t.Fatalf("isNormalRuntimeClose(%v)=%t, want %t", tt.err, got, tt.want)
			}
		})
	}
}

func TestBootstrapIdentityAcceptsPersistedNonStandaloneIdentity(t *testing.T) {
	persisted := identity.Identity{
		InstallationID:        domain.NewInstallationID(),
		NodeID:                domain.NewNodeID(),
		Role:                  domain.TopologyRoleNode,
		LeadershipGeneration:  4,
		LatestKnownGeneration: 4,
	}

	service, loaded, created, err := bootstrapIdentity(context.Background(), &startupIdentityStore{
		current: persisted,
		present: true,
	})
	if err != nil {
		t.Fatalf("bootstrapIdentity: %v", err)
	}
	if created {
		t.Fatal("expected an existing identity to be loaded, not bootstrapped")
	}
	if loaded != persisted {
		t.Fatalf("loaded identity=%+v want %+v", loaded, persisted)
	}
	if service.Current().Role != domain.TopologyRoleNode {
		t.Fatalf("service role=%s want %s", service.Current().Role, domain.TopologyRoleNode)
	}
}

func TestBootstrapIdentityCreatesMissingIdentity(t *testing.T) {
	service, bootstrapped, created, err := bootstrapIdentity(context.Background(), &startupIdentityStore{})
	if err != nil {
		t.Fatalf("bootstrapIdentity: %v", err)
	}
	if !created {
		t.Fatal("expected a missing identity to be bootstrapped")
	}
	if !bootstrapped.InstallationID.IsValid() || !bootstrapped.NodeID.IsValid() {
		t.Fatalf("bootstrapped identity has invalid ids: %+v", bootstrapped)
	}
	if bootstrapped.Role != domain.TopologyRoleStandalone {
		t.Fatalf("bootstrapped role=%s want %s", bootstrapped.Role, domain.TopologyRoleStandalone)
	}
	if service.Current() != bootstrapped {
		t.Fatalf("service identity=%+v want %+v", service.Current(), bootstrapped)
	}
}

func TestRenewalIdentityProviderFailsClosedWhenServiceIsUnloaded(t *testing.T) {
	provider := renewalIdentityProvider(identity.NewService(&startupIdentityStore{}))
	current, loaded, err := provider(context.Background())
	if err != nil || loaded || current != (identity.Identity{}) {
		t.Fatalf("unloaded provider current=%+v loaded=%t err=%v", current, loaded, err)
	}
}

func TestRenewalIdentityProviderTracksLiveServiceRole(t *testing.T) {
	service, current, _, err := bootstrapIdentity(context.Background(), &startupIdentityStore{})
	if err != nil {
		t.Fatalf("bootstrapIdentity: %v", err)
	}
	provider := renewalIdentityProvider(service)
	loaded, ok, err := provider(context.Background())
	if err != nil || !ok || loaded != current {
		t.Fatalf("initial provider current=%+v loaded=%t err=%v", loaded, ok, err)
	}
	if status := configuration.CheckRenewalPolicy(context.Background(), provider); !status.Allowed {
		t.Fatalf("standalone renewal policy status=%+v", status)
	}
	if _, err := service.TransitionTo(context.Background(), domain.TopologyRoleNode); err != nil {
		t.Fatalf("TransitionTo node: %v", err)
	}
	loaded, ok, err = provider(context.Background())
	if err != nil || !ok || loaded.Role != domain.TopologyRoleNode {
		t.Fatalf("live provider current=%+v loaded=%t err=%v", loaded, ok, err)
	}
	if status := configuration.CheckRenewalPolicy(context.Background(), provider); status.Allowed {
		t.Fatalf("node renewal policy unexpectedly allowed: %+v", status)
	}
}
