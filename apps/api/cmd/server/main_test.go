package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/cluster"
	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/enrollment"
	"github.com/zerkc/ProxyCore/apps/api/internal/httpserver"
	"github.com/zerkc/ProxyCore/apps/api/internal/identity"
	syncpublication "github.com/zerkc/ProxyCore/apps/api/internal/sync"
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

func TestNodeConverterFlagIsExplicitAndOffByDefault(t *testing.T) {
	if nodeConverterEnabled(nil) {
		t.Fatal("node converter should be disabled without an explicit flag")
	}
	if nodeConverterEnabled([]string{"--other"}) {
		t.Fatal("unrelated flag enabled node converter")
	}
	if !nodeConverterEnabled([]string{"--enable-node-converter"}) || !nodeConverterEnabled([]string{"--enable-node-converter=true"}) {
		t.Fatal("explicit node converter flag was not recognized")
	}
	if nodeConverterEnabled([]string{"--enable-node-converter=false"}) {
		t.Fatal("false node converter flag enabled conversion")
	}
}

func TestEnrollmentWorkflowTLSListenerFactoryWiresWorkflowMux(t *testing.T) {
	workflow := httpserver.EnrollmentWorkflowHandlerOptions{Cache: enrollment.NewDraftCache(enrollment.DraftCacheOptions{})}
	factory := newEnrollmentWorkflowTLSListenerFactory(runtimeTestIdentity{}, workflow, log.New(io.Discard, "", 0))
	var gotHandler http.Handler
	factory.listen = func(string, string) (net.Listener, error) {
		return runtimeTestNetListener{}, nil
	}
	factory.newServer = func(_ net.Listener, handler http.Handler, _ *enrollment.TLSCertificateProvider) (enrollment.EnrollmentTLSListener, error) {
		gotHandler = handler
		return runtimeTestListener{}, nil
	}
	if _, err := factory.Listen(context.Background(), "127.0.0.1:0", &enrollment.TLSCertificateProvider{}); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	if gotHandler == nil {
		t.Fatal("workflow factory did not construct a handler")
	}
	recorder := httptest.NewRecorder()
	gotHandler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("ordinary route status=%d, want %d", recorder.Code, http.StatusNotFound)
	}
}

func TestWireEnrollmentWorkflowDependenciesProvidesTransportAndValidator(t *testing.T) {
	workflow := httpserver.EnrollmentWorkflowHandlerOptions{}
	wireEnrollmentWorkflowDependencies(&workflow, syncpublication.NewSnapshotClient(syncpublication.SnapshotClientOptions{}), cluster.NewLocalKEKLoader(cluster.LocalKEKLoaderOptions{}))
	if workflow.FetchSnapshot == nil || workflow.Validator == nil {
		t.Fatalf("workflow dependencies fetcher=%v validator=%v, want both wired", workflow.FetchSnapshot != nil, workflow.Validator != nil)
	}
}

func TestWireEnrollmentWorkflowDependenciesPreservesLegacyNilPath(t *testing.T) {
	workflow := httpserver.EnrollmentWorkflowHandlerOptions{}
	wireEnrollmentWorkflowDependencies(&workflow, nil, nil)
	if workflow.FetchSnapshot != nil || workflow.Validator != nil {
		t.Fatalf("legacy workflow dependencies fetcher=%v validator=%v, want both nil", workflow.FetchSnapshot != nil, workflow.Validator != nil)
	}
}

func TestWireEnrollmentWorkflowDependenciesSelectsTokenTransport(t *testing.T) {
	token := "pcenr1_" + strings.Repeat("a", 32) + "_" + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x42}, 32))
	credential, err := syncpublication.NewNodeCredential("11112222-3333-4444-8999-aabbccddeeff", bytes.Repeat([]byte{0x19}, syncpublication.NodeCredentialSecretBytes))
	if err != nil {
		t.Fatal(err)
	}
	defer credential.Destroy()
	type observedRequest struct {
		method string
		path   string
		auth   string
		body   []byte
	}
	var observed []observedRequest
	client := syncpublication.NewSnapshotClient(syncpublication.SnapshotClientOptions{
		CanonicalizeURL: func(string) (string, error) { return "https://primary.example:3443/", nil },
		HTTPClient: &http.Client{Transport: snapshotRoundTripperMain(func(request *http.Request) (*http.Response, error) {
			var body []byte
			if request.Body != nil {
				body, _ = io.ReadAll(request.Body)
			}
			observed = append(observed, observedRequest{method: request.Method, path: request.URL.Path, auth: request.Header.Get("Authorization"), body: body})
			return &http.Response{StatusCode: http.StatusOK, ProtoMajor: 2, Body: io.NopCloser(bytes.NewReader([]byte("not-an-envelope"))), Header: make(http.Header)}, nil
		})},
	})
	workflow := httpserver.EnrollmentWorkflowHandlerOptions{}
	wireEnrollmentWorkflowDependencies(&workflow, client, nil)
	if workflow.FetchSnapshot == nil {
		t.Fatal("workflow fetcher was not wired")
	}
	_, _ = workflow.FetchSnapshot(context.Background(), "https://primary.example:3443/", token)
	_, _ = workflow.FetchSnapshot(context.Background(), "https://primary.example:3443/", credential.BearerCopy())
	if len(observed) != 2 {
		t.Fatalf("observed %d requests, want 2", len(observed))
	}
	if observed[0].method != http.MethodPost || observed[0].path != "/api/topology/sync/snapshot-by-token" ||
		string(observed[0].body) != `{"token":"`+token+`"}` || observed[0].auth != "" {
		t.Fatalf("token request=%+v", observed[0])
	}
	if observed[1].method != http.MethodGet || observed[1].path != "/api/topology/sync/snapshot" ||
		observed[1].auth != "Bearer "+credential.BearerCopy() {
		t.Fatalf("node request=%+v", observed[1])
	}
}

type snapshotRoundTripperMain func(*http.Request) (*http.Response, error)

func (f snapshotRoundTripperMain) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestEnrollmentWorkflowTLSListenerFactoryMountsSnapshotByTokenRoute(t *testing.T) {
	workflow := httpserver.EnrollmentWorkflowHandlerOptions{Cache: enrollment.NewDraftCache(enrollment.DraftCacheOptions{})}
	factory := newEnrollmentWorkflowTLSListenerFactory(runtimeTestIdentity{}, workflow, log.New(io.Discard, "", 0))
	factory.snapshotByToken = &httpserver.EnrollmentSnapshotByTokenHandlerOptions{}
	var gotHandler http.Handler
	factory.listen = func(string, string) (net.Listener, error) { return runtimeTestNetListener{}, nil }
	factory.newServer = func(_ net.Listener, handler http.Handler, _ *enrollment.TLSCertificateProvider) (enrollment.EnrollmentTLSListener, error) {
		gotHandler = handler
		return runtimeTestListener{}, nil
	}
	if _, err := factory.Listen(context.Background(), "127.0.0.1:0", &enrollment.TLSCertificateProvider{}); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	response := httptest.NewRecorder()
	gotHandler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, httpserver.EnrollmentSnapshotByTokenPath, nil))
	if response.Code != http.StatusMethodNotAllowed || response.Header().Get("Allow") != http.MethodPost {
		t.Fatalf("snapshot-by-token route status=%d allow=%q", response.Code, response.Header().Get("Allow"))
	}
}

func TestBuildNodeConverterRequiresFlagAndEligibleIdentity(t *testing.T) {
	for _, test := range []struct {
		name    string
		role    domain.TopologyRole
		enabled bool
		want    bool
	}{
		{name: "disabled standalone", role: domain.TopologyRoleStandalone, enabled: false, want: false},
		{name: "enabled standalone", role: domain.TopologyRoleStandalone, enabled: true, want: true},
		{name: "enabled primary", role: domain.TopologyRolePrimary, enabled: true, want: true},
		{name: "node is not eligible", role: domain.TopologyRoleNode, enabled: true, want: false},
		{name: "primary with nodes is not eligible", role: domain.TopologyRolePrimaryWithNodes, enabled: true, want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, _, _, err := bootstrapIdentity(context.Background(), &startupIdentityStore{
				current: identity.Identity{
					InstallationID:        domain.NewInstallationID(),
					NodeID:                domain.NewNodeID(),
					Role:                  test.role,
					LeadershipGeneration:  1,
					LatestKnownGeneration: 1,
				},
				present: true,
			})
			if err != nil {
				t.Fatalf("bootstrapIdentity: %v", err)
			}
			got := buildNodeConverter(test.enabled, &configuration.Store{}, service, nil)
			if (got != nil) != test.want {
				t.Fatalf("converter present=%t, want %t", got != nil, test.want)
			}
		})
	}
}
