package httpexport

import (
	"context"
	"testing"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/identity"
)

func TestIdentitySourceStringifiesCurrentIdentity(t *testing.T) {
	store := &identityTestStore{}
	service := identity.NewService(store)
	current, _, err := service.EnsureBootstrapped(context.Background())
	if err != nil {
		t.Fatalf("EnsureBootstrapped: %v", err)
	}

	source := NewIdentitySource(service)
	if got := source.InstallationIDString(); got != current.InstallationID.String() {
		t.Fatalf("InstallationIDString() = %q, want %q", got, current.InstallationID.String())
	}
	if got := source.NodeIDString(); got != current.NodeID.String() {
		t.Fatalf("NodeIDString() = %q, want %q", got, current.NodeID.String())
	}
	if got := source.RoleString(); got != current.Role.String() {
		t.Fatalf("RoleString() = %q, want %q", got, current.Role.String())
	}
}

type identityTestStore struct {
	identity.Store
}

func (s *identityTestStore) Get(context.Context) (identity.Identity, error) {
	return identity.Identity{}, identity.ErrNotFound
}

func (s *identityTestStore) Ensure(_ context.Context, installationID domain.InstallationID, nodeID domain.NodeID) (identity.Identity, bool, error) {
	return identity.Identity{
		InstallationID:        installationID,
		NodeID:                nodeID,
		Role:                  domain.TopologyRoleStandalone,
		LeadershipGeneration:  1,
		LatestKnownGeneration: 1,
	}, true, nil
}
