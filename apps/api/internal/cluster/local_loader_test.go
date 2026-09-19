package cluster

import (
	"context"
	"errors"
	"testing"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/identity"
)

func TestLocalKEKLoaderRejectsUnloadedIdentity(t *testing.T) {
	loader := NewLocalKEKLoader(LocalKEKLoaderOptions{Identity: localLoaderIdentitySource{}})
	_, _, err := loader.LoadLocalKEK(context.Background())
	if !errors.Is(err, ErrLocalKEKLoaderNotReady) {
		t.Fatalf("LoadLocalKEK error=%v, want ErrLocalKEKLoaderNotReady", err)
	}
}

func TestLocalKEKLoaderRejectsIneligibleRole(t *testing.T) {
	loader := NewLocalKEKLoader(LocalKEKLoaderOptions{Identity: localLoaderIdentitySource{
		current: identity.Identity{
			InstallationID: domain.NewInstallationID(), NodeID: domain.NewNodeID(),
			Role: domain.TopologyRoleStandalone, LeadershipGeneration: 1, LatestKnownGeneration: 1,
		},
	}})
	_, _, err := loader.LoadLocalKEK(context.Background())
	if !errors.Is(err, ErrLocalKEKLoaderDenied) {
		t.Fatalf("LoadLocalKEK error=%v, want ErrLocalKEKLoaderDenied", err)
	}
}

func TestLocalKEKLoaderRejectsMissingClusterKey(t *testing.T) {
	loader := NewLocalKEKLoader(LocalKEKLoaderOptions{Identity: localLoaderIdentitySource{
		current: identity.Identity{
			InstallationID: domain.NewInstallationID(), NodeID: domain.NewNodeID(),
			Role: domain.TopologyRoleNode, LeadershipGeneration: 1, LatestKnownGeneration: 1,
		},
	}})
	_, _, err := loader.LoadLocalKEK(context.Background())
	if !errors.Is(err, ErrLocalKEKLoaderNotReady) {
		t.Fatalf("LoadLocalKEK error=%v, want ErrLocalKEKLoaderNotReady", err)
	}
}

type localLoaderIdentitySource struct {
	current identity.Identity
}

func (s localLoaderIdentitySource) Current() identity.Identity { return s.current }

func (s localLoaderIdentitySource) Load(context.Context) (identity.Identity, error) {
	if s.current.InstallationID == "" {
		return identity.Identity{}, identity.ErrIdentityNotLoaded
	}
	return s.current, nil
}
