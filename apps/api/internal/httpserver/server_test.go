package httpserver_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/zerkc/ProxyCore/apps/api/internal/config"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/httpserver"
	"github.com/zerkc/ProxyCore/apps/api/internal/identity"
)

func TestHealth(t *testing.T) {
	srv := httpserver.New(config.Config{UIDist: t.TempDir()}, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["ok"] != true {
		t.Fatalf("body=%v", body)
	}
}

type readyIdentityStore struct {
	current identity.Identity
}

func (s *readyIdentityStore) Get(context.Context) (identity.Identity, error) {
	return s.current, nil
}

func (s *readyIdentityStore) Ensure(context.Context, domain.InstallationID, domain.NodeID) (identity.Identity, bool, error) {
	return s.current, false, nil
}

func (s *readyIdentityStore) UpdateRole(_ context.Context, role domain.TopologyRole) error {
	s.current.Role = role
	return nil
}

func (s *readyIdentityStore) UpdateLeadershipGeneration(_ context.Context, generation domain.LeadershipGeneration) error {
	s.current.LeadershipGeneration = generation
	if generation > s.current.LatestKnownGeneration {
		s.current.LatestKnownGeneration = generation
	}
	return nil
}

func (s *readyIdentityStore) UpdateLatestKnownGeneration(_ context.Context, generation domain.LeadershipGeneration) error {
	if generation > s.current.LatestKnownGeneration {
		s.current.LatestKnownGeneration = generation
	}
	return nil
}

func (s *readyIdentityStore) UpdateClusterKeyID(_ context.Context, keyID *uuid.UUID) error {
	s.current.ClusterKeyID = keyID
	return nil
}

func TestReadyIdentityUsesRedactedTopologyFields(t *testing.T) {
	clusterKeyID := uuid.MustParse("aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee")
	store := &readyIdentityStore{current: identity.Identity{
		InstallationID:        domain.NewInstallationID(),
		NodeID:                domain.NewNodeID(),
		Role:                  domain.TopologyRoleStalePrimary,
		LeadershipGeneration:  3,
		LatestKnownGeneration: 7,
		ClusterKeyID:          &clusterKeyID,
	}}
	identityService := identity.NewService(store)
	if _, err := identityService.Load(context.Background()); err != nil {
		t.Fatalf("load identity: %v", err)
	}

	srv := httpserver.New(config.Config{UIDist: t.TempDir()}, nil, httpserver.WithIdentityService(identityService))
	req := httptest.NewRequest(http.MethodGet, "/api/ready", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Identity map[string]any `json:"identity"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{
		"installationId",
		"nodeId",
		"role",
		"leadershipGeneration",
		"latestKnownGeneration",
		"stalePrimary",
		"writable",
	} {
		if _, ok := body.Identity[field]; !ok {
			t.Fatalf("identity missing %q: %v", field, body.Identity)
		}
	}
	if body.Identity["role"] != string(domain.TopologyRoleStalePrimary) || body.Identity["writable"] != false {
		t.Fatalf("unexpected identity=%v", body.Identity)
	}
	if body.Identity["clusterKeyId"] != clusterKeyID.String() {
		t.Fatalf("identity cluster key id=%v want %q", body.Identity["clusterKeyId"], clusterKeyID)
	}
}

func TestSPAFallback(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "index.html"), []byte("<html>ok</html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := httpserver.New(config.Config{UIDist: root}, nil)
	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != "<html>ok</html>" {
		t.Fatalf("body=%q", got)
	}
}
