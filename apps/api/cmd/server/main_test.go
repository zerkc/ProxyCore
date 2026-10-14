package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/cluster"
	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/enrollment"
	"github.com/zerkc/ProxyCore/apps/api/internal/httpserver"
	"github.com/zerkc/ProxyCore/apps/api/internal/identity"
	"github.com/zerkc/ProxyCore/apps/api/internal/secrets"
	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
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

func TestNodeConverterFlagIsOnByDefaultWithExplicitOptOut(t *testing.T) {
	if !nodeConverterEnabled(nil) {
		t.Fatal("node converter should be enabled by default")
	}
	if !nodeConverterEnabled([]string{"--other"}) {
		t.Fatal("unrelated flag disabled node converter")
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
		t.Fatalf("token request method=%q path=%q authLen=%d bodyLen=%d bodyHash=%x", observed[0].method, observed[0].path, len(observed[0].auth), len(observed[0].body), sha256.Sum256(observed[0].body))
	}
	if observed[1].method != http.MethodGet || observed[1].path != "/api/topology/sync/snapshot" ||
		observed[1].auth != "Bearer "+credential.BearerCopy() {
		t.Fatalf("node request method=%q path=%q authLen=%d", observed[1].method, observed[1].path, len(observed[1].auth))
	}
}

type snapshotRoundTripperMain func(*http.Request) (*http.Response, error)

func (f snapshotRoundTripperMain) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type snapshotByTokenStoreReaderFunc func(context.Context, string, configuration.SnapshotPublicationIdentityRecord) ([]byte, error)

func (f snapshotByTokenStoreReaderFunc) ReadSnapshotPublicationByToken(ctx context.Context, selector string, identity configuration.SnapshotPublicationIdentityRecord) ([]byte, error) {
	return f(ctx, selector, identity)
}

type snapshotByTokenMainIdentity struct {
	current identity.Identity
}

func (s snapshotByTokenMainIdentity) Current() (identity.Identity, bool) {
	return s.current, true
}

type snapshotByTokenMainMaterial struct{}

func (snapshotByTokenMainMaterial) CopyBytes() []byte { return []byte{1} }
func (snapshotByTokenMainMaterial) Destroy()          {}

func TestProductionSnapshotByTokenStoreIsNonNil(t *testing.T) {
	store := productionSnapshotByTokenStore(&configuration.SQLitePhase2Store{})
	if store == nil {
		t.Fatal("production snapshot-by-token store is nil")
	}
}

func TestProductionSnapshotByTokenStoreMapsConfigurationSentinels(t *testing.T) {
	cases := []struct {
		name string
		from error
		to   error
	}{
		{name: "unauthenticated", from: configuration.ErrSnapshotByTokenUnauthenticated, to: httpserver.ErrSnapshotByTokenUnauthenticated},
		{name: "gone", from: configuration.ErrSnapshotByTokenGone, to: httpserver.ErrSnapshotByTokenGone},
		{name: "denied", from: configuration.ErrSnapshotByTokenDenied, to: httpserver.ErrSnapshotByTokenDenied},
		{name: "unavailable", from: configuration.ErrSnapshotByTokenUnavailable, to: httpserver.ErrSnapshotByTokenUnavailable},
		{name: "not ready", from: configuration.ErrNoPublishableSnapshot, to: httpserver.ErrNoPublishableSnapshot},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			adapter := snapshotByTokenStoreAdapter{store: snapshotByTokenStoreReaderFunc(func(context.Context, string, configuration.SnapshotPublicationIdentityRecord) ([]byte, error) {
				return nil, test.from
			})}
			_, err := adapter.ReadSnapshotPublicationByToken(context.Background(), "selector", configuration.SnapshotPublicationIdentityRecord{})
			if !errors.Is(err, test.to) {
				t.Fatalf("error=%v want %v", err, test.to)
			}
		})
	}
}

func TestProductionSnapshotByTokenStorePassesExactBytes(t *testing.T) {
	want := []byte(`{"published":"canonical"}`)
	adapter := snapshotByTokenStoreAdapter{store: snapshotByTokenStoreReaderFunc(func(context.Context, string, configuration.SnapshotPublicationIdentityRecord) ([]byte, error) {
		return append([]byte(nil), want...), nil
	})}
	got, err := adapter.ReadSnapshotPublicationByToken(context.Background(), "selector", configuration.SnapshotPublicationIdentityRecord{})
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("body=%q err=%v want=%q", got, err, want)
	}
}

func TestProductionSnapshotByTokenTransportReturnsExactBytes(t *testing.T) {
	selector := strings.Repeat("a", 32)
	token := "pcenr1_" + selector + "_" + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x42}, 32))
	want := []byte(`{"published":"canonical"}`)
	adapter := snapshotByTokenStoreAdapter{store: snapshotByTokenStoreReaderFunc(func(_ context.Context, gotSelector string, _ configuration.SnapshotPublicationIdentityRecord) ([]byte, error) {
		if gotSelector != selector {
			t.Fatalf("selector=%q want %q", gotSelector, selector)
		}
		return append([]byte(nil), want...), nil
	})}
	current := identity.Identity{
		InstallationID: domain.NewInstallationID(), NodeID: domain.NewNodeID(), Role: domain.TopologyRolePrimary,
		LeadershipGeneration: 1, LatestKnownGeneration: 1, ClusterKeyID: func() *uuid.UUID { id := uuid.New(); return &id }(),
	}
	handler := httpserver.NewEnrollmentSnapshotByTokenHandler(httpserver.EnrollmentSnapshotByTokenHandlerOptions{
		Store: adapter, Identity: snapshotByTokenMainIdentity{current: current},
		Certificate: func(context.Context) (*x509.Certificate, error) {
			return &x509.Certificate{DNSNames: []string{"primary.example"}}, nil
		},
		ParseToken:  httpserver.ParseHTTPEnrollmentToken,
		VerifyToken: func(context.Context, string) error { return nil },
		ClusterKeyLoader: func(context.Context) (httpserver.SnapshotByTokenKeyMaterial, error) {
			return snapshotByTokenMainMaterial{}, nil
		},
	})
	request := httptest.NewRequest(http.MethodPost, httpserver.EnrollmentSnapshotByTokenPath, bytes.NewBufferString(`{"token":"`+token+`"}`))
	request.Host = "primary.example:3443"
	request.TLS = &tls.ConnectionState{Version: tls.VersionTLS13, HandshakeComplete: true, ServerName: "primary.example"}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !bytes.Equal(response.Body.Bytes(), want) {
		t.Fatalf("status=%d body=%q want status=%d body=%q", response.Code, response.Body.Bytes(), http.StatusOK, want)
	}
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

func TestEnrollmentWorkflowTLSListenerFactoryMountsAcknowledgementRoute(t *testing.T) {
	workflow := httpserver.EnrollmentWorkflowHandlerOptions{Cache: enrollment.NewDraftCache(enrollment.DraftCacheOptions{})}
	factory := newEnrollmentWorkflowTLSListenerFactory(runtimeTestIdentity{}, workflow, log.New(io.Discard, "", 0))
	factory.acknowledgement = &httpserver.EnrollmentSnapshotAcknowledgementHandlerOptions{}
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
	gotHandler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, httpserver.EnrollmentSnapshotAcknowledgementPath, nil))
	if response.Code != http.StatusMethodNotAllowed || response.Header().Get("Allow") != http.MethodPost {
		t.Fatalf("acknowledgement route status=%d allow=%q", response.Code, response.Header().Get("Allow"))
	}
}

func TestBuildNodeConverterRequiresFlagAndDependenciesOnly(t *testing.T) {
	for _, test := range []struct {
		name    string
		role    domain.TopologyRole
		enabled bool
		want    bool
	}{
		{name: "disabled standalone", role: domain.TopologyRoleStandalone, enabled: false, want: false},
		{name: "enabled standalone", role: domain.TopologyRoleStandalone, enabled: true, want: true},
		{name: "enabled primary", role: domain.TopologyRolePrimary, enabled: true, want: true},
		// Role eligibility is a per-request confirm check, so a demoted
		// installation can enroll without a restart; the converter itself is
		// built regardless of the boot-time role when the flag is on.
		{name: "node", role: domain.TopologyRoleNode, enabled: true, want: true},
		{name: "primary with nodes", role: domain.TopologyRolePrimaryWithNodes, enabled: true, want: true},
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
			got := buildNodeConverter(test.enabled, &configuration.SQLitePhase2Store{}, service, nil)
			if (got != nil) != test.want {
				t.Fatalf("converter present=%t, want %t", got != nil, test.want)
			}
		})
	}
}

type snapshotKeyIdentityFake struct {
	current identity.Identity
	loaded  bool
}

func (f snapshotKeyIdentityFake) Current() (identity.Identity, bool) { return f.current, f.loaded }

type snapshotKeyLoaderFake struct {
	calls      int
	gotID      uuid.UUID
	materialID uuid.UUID
	key        []byte
	err        error
}

func (f *snapshotKeyLoaderFake) LoadActive(_ context.Context, id uuid.UUID) (cluster.KeyMaterial, error) {
	f.calls++
	f.gotID = id
	if f.err != nil {
		return cluster.KeyMaterial{}, f.err
	}
	materialID := f.materialID
	if materialID == uuid.Nil {
		materialID = id
	}
	return cluster.NewKeyMaterial(materialID, f.key)
}

func snapshotKeyTestIdentity(role domain.TopologyRole, keyID *uuid.UUID) snapshotKeyIdentityFake {
	return snapshotKeyIdentityFake{loaded: true, current: identity.Identity{Role: role, ClusterKeyID: keyID}}
}

func TestSnapshotByTokenClusterKeyLoaderMatrix(t *testing.T) {
	keyID := uuid.New()
	good := bytes.Repeat([]byte{9}, cluster.KEKLength)
	cases := []struct {
		name      string
		identity  snapshotKeyIdentityFake
		fake      snapshotKeyLoaderFake
		wantErr   bool
		wantCalls int
	}{
		{name: "primary loads", identity: snapshotKeyTestIdentity(domain.TopologyRolePrimary, &keyID), fake: snapshotKeyLoaderFake{key: good}, wantCalls: 1},
		{name: "primary with nodes loads", identity: snapshotKeyTestIdentity(domain.TopologyRolePrimaryWithNodes, &keyID), fake: snapshotKeyLoaderFake{key: good}, wantCalls: 1},
		{name: "node rejected", identity: snapshotKeyTestIdentity(domain.TopologyRoleNode, &keyID), fake: snapshotKeyLoaderFake{key: good}, wantErr: true},
		{name: "standalone rejected", identity: snapshotKeyTestIdentity(domain.TopologyRoleStandalone, &keyID), fake: snapshotKeyLoaderFake{key: good}, wantErr: true},
		{name: "missing cluster key id", identity: snapshotKeyTestIdentity(domain.TopologyRolePrimary, nil), fake: snapshotKeyLoaderFake{key: good}, wantErr: true},
		{name: "identity not loaded", identity: snapshotKeyIdentityFake{}, fake: snapshotKeyLoaderFake{key: good}, wantErr: true},
		{name: "loaded id differs", identity: snapshotKeyTestIdentity(domain.TopologyRolePrimary, &keyID), fake: snapshotKeyLoaderFake{key: good, materialID: uuid.New()}, wantErr: true, wantCalls: 1},
		{name: "store failure", identity: snapshotKeyTestIdentity(domain.TopologyRolePrimary, &keyID), fake: snapshotKeyLoaderFake{err: errors.New("boom")}, wantErr: true, wantCalls: 1},
		{name: "key denied", identity: snapshotKeyTestIdentity(domain.TopologyRolePrimary, &keyID), fake: snapshotKeyLoaderFake{err: cluster.ErrClusterKeyDenied}, wantErr: true, wantCalls: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := tc.fake
			loader := &snapshotByTokenClusterKeyLoader{keys: &fake, identity: tc.identity}
			material, err := loader.Load(context.Background())
			if fake.calls != tc.wantCalls {
				t.Fatalf("key loader calls=%d, want %d", fake.calls, tc.wantCalls)
			}
			if tc.wantErr {
				if !errors.Is(err, httpserver.ErrSnapshotByTokenUnavailable) || material != nil {
					t.Fatalf("error=%v material=%v, want ErrSnapshotByTokenUnavailable", err, material != nil)
				}
				return
			}
			if err != nil || material == nil {
				t.Fatalf("Load: err=%v material=%v", err, material != nil)
			}
			defer material.Destroy()
			if fake.gotID != keyID || !bytes.Equal(material.CopyBytes(), good) {
				t.Fatal("loader requested or returned the wrong key")
			}
		})
	}
}

func TestSnapshotByTokenClusterKeyLoaderUnconfigured(t *testing.T) {
	keyID := uuid.New()
	fake := &snapshotKeyLoaderFake{key: bytes.Repeat([]byte{9}, cluster.KEKLength)}
	for name, loader := range map[string]*snapshotByTokenClusterKeyLoader{
		"nil":         nil,
		"no keys":     {identity: snapshotKeyTestIdentity(domain.TopologyRolePrimary, &keyID)},
		"no identity": {keys: fake},
	} {
		if material, err := loader.Load(context.Background()); !errors.Is(err, httpserver.ErrSnapshotByTokenUnavailable) || material != nil {
			t.Fatalf("%s: err=%v material=%v", name, err, material != nil)
		}
	}
	//nolint:staticcheck // a nil context is the case under test.
	if _, err := (&snapshotByTokenClusterKeyLoader{keys: fake, identity: snapshotKeyTestIdentity(domain.TopologyRolePrimary, &keyID)}).Load(nil); !errors.Is(err, httpserver.ErrSnapshotByTokenUnavailable) {
		t.Fatalf("nil ctx err=%v", err)
	}
}

func TestSnapshotByTokenClusterKeyLoaderWorksWithSQLiteKeyStore(t *testing.T) {
	db, err := sqlitedb.Open(filepath.Join(t.TempDir(), "keys.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := sqlitedb.Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	master := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x19}, 32))
	key := bytes.Repeat([]byte{0x33}, cluster.KEKLength)
	wrapped, err := secrets.EncryptSecret(string(key), master)
	if err != nil {
		t.Fatal(err)
	}
	keyID := uuid.New()
	created, err := sqlitedb.EncodeTimestamp(sqlitedb.FiniteTimestamp{Seconds: 1_700_000_000, Nanos: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`insert into cluster_keys (id, purpose, wrapped_kek, wrapping_key_version, created_at) values (?, ?, ?, ?, ?)`,
		keyID.String(), cluster.ClusterKeyPurpose, wrapped, cluster.ClusterKeyWrappingVersion, created); err != nil {
		t.Fatal(err)
	}
	store := cluster.NewSQLiteKeyStore(db, nil, master)
	load := func(role domain.TopologyRole, id *uuid.UUID) (httpserver.SnapshotByTokenKeyMaterial, error) {
		return (&snapshotByTokenClusterKeyLoader{keys: store, identity: snapshotKeyTestIdentity(role, id)}).Load(context.Background())
	}
	material, err := load(domain.TopologyRolePrimaryWithNodes, &keyID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !bytes.Equal(material.CopyBytes(), key) {
		t.Fatal("loaded key differs from persisted key")
	}
	material.Destroy()
	unknown := uuid.New()
	if m, err := load(domain.TopologyRolePrimaryWithNodes, &unknown); !errors.Is(err, httpserver.ErrSnapshotByTokenUnavailable) || m != nil {
		t.Fatalf("unknown id: err=%v", err)
	}
}

type fakePrimaryTrustReader struct {
	anchors map[string]configuration.PrimaryTrustAnchor
	err     error
}

func (f fakePrimaryTrustReader) Get(_ context.Context, origin string) (configuration.PrimaryTrustAnchor, bool, error) {
	anchor, ok := f.anchors[origin]
	return anchor, ok, f.err
}

func TestPrimaryTrustResolverAdaptsTheStoreForTheTransport(t *testing.T) {
	reader := fakePrimaryTrustReader{anchors: map[string]configuration.PrimaryTrustAnchor{
		"https://primary.example:3443": {Origin: "https://primary.example:3443", CAPEM: "PEM"},
	}}
	resolve := primaryTrustResolver(reader)
	if pem, ok, err := resolve(context.Background(), "https://primary.example:3443"); pem != "PEM" || !ok || err != nil {
		t.Fatalf("known origin = %q %v %v", pem, ok, err)
	}
	if _, ok, err := resolve(context.Background(), "https://other.example:3443"); ok || err != nil {
		t.Fatalf("unknown origin ok=%v err=%v, want not found without error", ok, err)
	}
	failing := primaryTrustResolver(fakePrimaryTrustReader{err: errors.New("db closed")})
	if _, _, err := failing(context.Background(), "https://primary.example:3443"); err == nil {
		t.Fatal("a store failure must reach the transport so it fails closed")
	}
	if _, ok, err := primaryTrustResolver(nil)(context.Background(), "https://primary.example:3443"); ok || err != nil {
		t.Fatalf("nil reader ok=%v err=%v, want no trust", ok, err)
	}
}
