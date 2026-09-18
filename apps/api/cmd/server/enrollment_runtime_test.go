package main

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
	"github.com/zerkc/ProxyCore/apps/api/internal/enrollment"
	"github.com/zerkc/ProxyCore/apps/api/internal/identity"
)

type runtimeTestIdentity struct{}

func (runtimeTestIdentity) Current(context.Context) (identity.Identity, bool, error) {
	return identity.Identity{}, true, nil
}

type runtimeTestListener struct{}

func (runtimeTestListener) Serve() error { return nil }

func (runtimeTestListener) Shutdown(context.Context) error { return nil }

type runtimeTestHTTPServer struct {
	started   chan struct{}
	finished  chan struct{}
	shutdown  chan struct{}
	available bool
	mu        sync.Mutex
}

func newRuntimeTestHTTPServer() *runtimeTestHTTPServer {
	return &runtimeTestHTTPServer{
		started:  make(chan struct{}),
		finished: make(chan struct{}),
		shutdown: make(chan struct{}),
	}
}

func (s *runtimeTestHTTPServer) ListenAndServe() error {
	s.mu.Lock()
	s.available = true
	s.mu.Unlock()
	close(s.started)
	<-s.shutdown
	close(s.finished)
	return http.ErrServerClosed
}

func (s *runtimeTestHTTPServer) Shutdown(context.Context) error {
	s.mu.Lock()
	s.available = false
	s.mu.Unlock()
	select {
	case <-s.shutdown:
	default:
		close(s.shutdown)
	}
	return nil
}

func (s *runtimeTestHTTPServer) isAvailable() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.available
}

func (s *runtimeTestHTTPServer) wasShutdown() bool {
	select {
	case <-s.shutdown:
		return true
	default:
		return false
	}
}

type runtimeTestSupervisor struct {
	runErr   error
	finished chan struct{}
	canceled chan struct{}
	status   enrollment.EnrollmentTLSSupervisorStatus
}

func (s *runtimeTestSupervisor) Run(ctx context.Context) error {
	defer close(s.finished)
	if s.runErr != nil {
		return s.runErr
	}
	<-ctx.Done()
	close(s.canceled)
	return nil
}

func (s *runtimeTestSupervisor) Status() enrollment.EnrollmentTLSSupervisorStatus {
	return s.status
}

type runtimeTestConfiguration struct {
	hostnames     configuration.EnrollmentHostnameConfig
	material      configuration.EnrollmentTLSMaterial
	hostnameError error
	materialError error
}

func (s runtimeTestConfiguration) GetEnrollmentHostnames(context.Context) (configuration.EnrollmentHostnameConfig, error) {
	return s.hostnames, s.hostnameError
}

func (s runtimeTestConfiguration) EnsureEnrollmentTLSMaterial(context.Context) (configuration.EnrollmentTLSMaterial, error) {
	return s.material, s.materialError
}

type runtimeTestNetListener struct{}

func (runtimeTestNetListener) Accept() (net.Conn, error) { return nil, net.ErrClosed }

func (runtimeTestNetListener) Close() error { return nil }

func (runtimeTestNetListener) Addr() net.Addr { return runtimeTestAddr("runtime-test") }

type runtimeTestAddr string

func (a runtimeTestAddr) Network() string { return "tcp" }

func (a runtimeTestAddr) String() string { return string(a) }

func TestWatchEnrollmentTLSStatusRedactsFailures(t *testing.T) {
	cases := []struct {
		name   string
		status enrollment.EnrollmentTLSSupervisorStatus
		want   string
	}{
		{
			name: "ineligible role",
			status: enrollment.EnrollmentTLSSupervisorStatus{
				State:        enrollment.EnrollmentTLSStateIneligible,
				RoleEligible: false,
			},
			want: "ineligible identity",
		},
		{
			name: "unconfigured hostnames",
			status: enrollment.EnrollmentTLSSupervisorStatus{
				State:        enrollment.EnrollmentTLSStateIneligible,
				RoleEligible: true,
				Configured:   false,
			},
			want: "hostnames are not configured",
		},
		{
			name: "material failure",
			status: enrollment.EnrollmentTLSSupervisorStatus{
				State:               enrollment.EnrollmentTLSStateDegraded,
				LastError:           "BEGIN PRIVATE KEY leaked",
				MaterialReady:       false,
				ConsecutiveFailures: 1,
			},
			want: "unavailable",
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			var logs runtimeTestLog
			supervisor := &runtimeTestSupervisor{status: tt.status}
			done := make(chan struct{})
			go func() {
				watchEnrollmentTLSStatus(ctx, supervisor, log.New(&logs, "", 0), time.Millisecond)
				close(done)
			}()
			if !logs.WaitFor(tt.want, time.Second) {
				cancel()
				<-done
				t.Fatalf("logs=%q, want %q", logs.String(), tt.want)
			}
			cancel()
			<-done
			if strings.Contains(logs.String(), "BEGIN PRIVATE KEY") {
				t.Fatalf("logs leaked private material: %q", logs.String())
			}
		})
	}
}

func TestEnrollmentTLSAdaptersRedactDependencyFailures(t *testing.T) {
	secret := "BEGIN PRIVATE KEY: do-not-log"
	var logs runtimeTestLog
	adapter := &enrollmentTLSConfigurationAdapter{
		store: runtimeTestConfiguration{
			hostnameError: errors.New(secret),
			materialError: errors.New(secret),
		},
		logger: log.New(&logs, "", 0),
	}
	if _, err := adapter.GetEnrollmentHostnames(context.Background()); strings.Contains(err.Error(), secret) {
		t.Fatalf("hostname error leaked private material: %v", err)
	}
	if _, err := adapter.EnsureEnrollmentTLSMaterial(context.Background()); strings.Contains(err.Error(), secret) {
		t.Fatalf("material error leaked private material: %v", err)
	}
	if strings.Contains(logs.String(), secret) {
		t.Fatalf("adapter logs leaked private material: %q", logs.String())
	}
}

func TestEnrollmentTLSListenerFactoryRedactsBindFailure(t *testing.T) {
	secret := "PRIVATE KEY PEM"
	var logs runtimeTestLog
	factory := newEnrollmentTLSListenerFactory(runtimeTestIdentity{}, log.New(&logs, "", 0))
	factory.listen = func(string, string) (net.Listener, error) {
		return nil, errors.New(secret)
	}
	_, err := factory.Listen(context.Background(), "127.0.0.1:0", &enrollment.TLSCertificateProvider{})
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("bind error leaked private material: %v", err)
	}
	if strings.Contains(logs.String(), secret) {
		t.Fatalf("bind log leaked private material: %q", logs.String())
	}
}

func TestRunServerRuntimeKeepsOrdinaryHTTPAfterEnrollmentFailure(t *testing.T) {
	processCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ordinary := newRuntimeTestHTTPServer()
	supervisor := &runtimeTestSupervisor{
		runErr:   errors.New("BEGIN PRIVATE KEY leaked"),
		finished: make(chan struct{}),
	}
	var logs runtimeTestLog

	result := make(chan error, 1)
	go func() {
		result <- runServerRuntime(processCtx, log.New(&logs, "", 0), serverRuntimeOptions{
			ordinary:                 ordinary,
			enrollment:               supervisor,
			enrollmentStatusInterval: time.Millisecond,
		})
	}()

	select {
	case <-ordinary.started:
	case <-time.After(time.Second):
		t.Fatal("ordinary HTTP server did not start")
	}
	select {
	case <-supervisor.finished:
	case <-time.After(time.Second):
		t.Fatal("enrollment supervisor did not finish")
	}
	if !ordinary.isAvailable() {
		t.Fatal("ordinary HTTP server was not left available after enrollment failure")
	}
	if ordinary.wasShutdown() {
		t.Fatal("ordinary HTTP server was shut down by enrollment failure")
	}
	if strings.Contains(logs.String(), "BEGIN PRIVATE KEY") {
		t.Fatalf("enrollment failure log leaked private material: %q", logs.String())
	}

	cancel()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("runServerRuntime: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("runServerRuntime did not wait for cancellation")
	}
	if !ordinary.wasShutdown() {
		t.Fatal("ordinary HTTP server was not shut down")
	}
}

func TestRunServerRuntimeCancelsAndWaitsForOwnedGoroutines(t *testing.T) {
	processCtx, cancel := context.WithCancel(context.Background())
	ordinary := newRuntimeTestHTTPServer()
	supervisor := &runtimeTestSupervisor{
		finished: make(chan struct{}),
		canceled: make(chan struct{}),
	}
	workerStarted := make(chan struct{})
	workerFinished := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		result <- runServerRuntime(processCtx, log.New(io.Discard, "", 0), serverRuntimeOptions{
			ordinary:   ordinary,
			enrollment: supervisor,
			workers: []func(context.Context){func(context.Context) {
				close(workerStarted)
				<-processCtx.Done()
				close(workerFinished)
			}},
		})
	}()

	select {
	case <-ordinary.started:
	case <-time.After(time.Second):
		t.Fatal("ordinary HTTP server did not start")
	}
	select {
	case <-workerStarted:
	case <-time.After(time.Second):
		t.Fatal("runtime worker did not start")
	}
	cancel()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("runServerRuntime: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("runServerRuntime did not wait for owned goroutines")
	}
	select {
	case <-supervisor.canceled:
	case <-time.After(time.Second):
		t.Fatal("enrollment supervisor did not observe cancellation")
	}
	select {
	case <-workerFinished:
	default:
		t.Fatal("runtime worker was not joined")
	}
	if !ordinary.wasShutdown() {
		t.Fatal("ordinary HTTP server was not shut down")
	}
}

func TestEnrollmentTLSListenerFactoryWiresAddressProviderAndProofOnlyHandler(t *testing.T) {
	factory := newEnrollmentTLSListenerFactory(runtimeTestIdentity{}, log.New(io.Discard, "", 0))
	var gotNetwork, gotAddress string
	var gotProvider *enrollment.TLSCertificateProvider
	var gotHandler http.Handler
	factory.listen = func(network, address string) (net.Listener, error) {
		gotNetwork, gotAddress = network, address
		return runtimeTestNetListener{}, nil
	}
	factory.newServer = func(listener net.Listener, handler http.Handler, provider *enrollment.TLSCertificateProvider) (enrollment.EnrollmentTLSListener, error) {
		gotProvider = provider
		gotHandler = handler
		return runtimeTestListener{}, nil
	}

	provider := &enrollment.TLSCertificateProvider{}
	listener, err := factory.Listen(context.Background(), "127.0.0.1:0", provider)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	if listener == nil {
		t.Fatal("Listen returned a nil listener")
	}
	if gotNetwork != "tcp" || gotAddress != "127.0.0.1:0" {
		t.Fatalf("listen args=(%q, %q), want (tcp, 127.0.0.1:0)", gotNetwork, gotAddress)
	}
	if gotProvider != provider {
		t.Fatal("factory did not forward the exact certificate provider")
	}
	if gotHandler == nil {
		t.Fatal("factory did not construct a proof-only handler")
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	gotHandler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("ordinary route status=%d, want %d", recorder.Code, http.StatusNotFound)
	}
}
