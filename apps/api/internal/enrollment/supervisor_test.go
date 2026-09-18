package enrollment_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/acme"
	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/enrollment"
	"github.com/zerkc/ProxyCore/apps/api/internal/identity"
)

type supervisorIdentity struct {
	mu     sync.Mutex
	value  identity.Identity
	loaded bool
	err    error
}

func (f *supervisorIdentity) Current(context.Context) (identity.Identity, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.value, f.loaded, f.err
}

type supervisorConfiguration struct {
	mu          sync.Mutex
	hostnames   configuration.EnrollmentHostnameConfig
	materials   []configuration.EnrollmentTLSMaterial
	ensureErrs  []error
	getErr      error
	ensureCalls int
}

func (f *supervisorConfiguration) GetEnrollmentHostnames(context.Context) (configuration.EnrollmentHostnameConfig, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return configuration.EnrollmentHostnameConfig{}, f.getErr
	}
	return f.hostnames, nil
}

func (f *supervisorConfiguration) EnsureEnrollmentTLSMaterial(context.Context) (configuration.EnrollmentTLSMaterial, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ensureCalls++
	if len(f.ensureErrs) != 0 {
		err := f.ensureErrs[0]
		f.ensureErrs = f.ensureErrs[1:]
		return configuration.EnrollmentTLSMaterial{}, err
	}
	if len(f.materials) == 0 {
		return configuration.EnrollmentTLSMaterial{}, errors.New("no test material")
	}
	material := f.materials[0]
	f.materials = f.materials[1:]
	return material, nil
}

type supervisorListener struct {
	started chan struct{}
	result  chan error
	done    chan struct{}
	once    sync.Once
	mu      sync.Mutex
	stops   int
}

func newSupervisorListener() *supervisorListener {
	return &supervisorListener{
		started: make(chan struct{}),
		result:  make(chan error, 1),
		done:    make(chan struct{}),
	}
}

func (l *supervisorListener) Serve() error {
	close(l.started)
	err := <-l.result
	close(l.done)
	return err
}

func (l *supervisorListener) finish(err error) {
	l.once.Do(func() { l.result <- err })
}

func (l *supervisorListener) Shutdown(context.Context) error {
	l.mu.Lock()
	l.stops++
	l.mu.Unlock()
	l.finish(http.ErrServerClosed)
	return nil
}

func (l *supervisorListener) stopCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.stops
}

type supervisorFactory struct {
	mu        sync.Mutex
	listeners []*supervisorListener
	addresses []string
	providers []*enrollment.TLSCertificateProvider
	created   chan *supervisorListener
	errs      []error
}

func (f *supervisorFactory) Listen(_ context.Context, address string, provider *enrollment.TLSCertificateProvider) (enrollment.EnrollmentTLSListener, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.errs) != 0 {
		err := f.errs[0]
		f.errs = f.errs[1:]
		return nil, err
	}
	listener := newSupervisorListener()
	f.listeners = append(f.listeners, listener)
	f.addresses = append(f.addresses, address)
	f.providers = append(f.providers, provider)
	if f.created != nil {
		f.created <- listener
	}
	return listener, nil
}

func (f *supervisorFactory) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.listeners)
}

type supervisorPublisher struct {
	mu       sync.Mutex
	provider *enrollment.TLSCertificateProvider
	calls    int
	err      error
}

func (p *supervisorPublisher) PublishEnrollmentTLSProvider(provider *enrollment.TLSCertificateProvider) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if p.err != nil {
		return p.err
	}
	p.provider = provider
	return nil
}

func supervisorOptions(id *supervisorIdentity, cfg *supervisorConfiguration, factory *supervisorFactory, publisher *supervisorPublisher) enrollment.EnrollmentTLSSupervisorOptions {
	return enrollment.EnrollmentTLSSupervisorOptions{
		Identity:          id,
		Configuration:     cfg,
		ListenerFactory:   factory,
		ProviderPublisher: publisher,
		Address:           "127.0.0.1:3443",
		ReconcileInterval: 20 * time.Millisecond,
		RetryMin:          5 * time.Millisecond,
		RetryMax:          20 * time.Millisecond,
		ShutdownTimeout:   time.Second,
	}
}

func supervisorIdentityValue(role domain.TopologyRole) identity.Identity {
	return identity.Identity{Role: role, LeadershipGeneration: 1, LatestKnownGeneration: 1}
}

func supervisorMaterial(leaf acme.Material, ca string) configuration.EnrollmentTLSMaterial {
	return configuration.EnrollmentTLSMaterial{
		CertificatePEM:   leaf.CertificatePEM,
		PrivateKeyPEM:    leaf.PrivateKeyPEM,
		CACertificatePEM: ca,
	}
}

func TestEnrollmentTLSSupervisorEligibleRolesStartAndIneligibleRolesDoNot(t *testing.T) {
	fixture := newProviderFixture(t)
	roles := []domain.TopologyRole{
		domain.TopologyRoleStandalone,
		domain.TopologyRoleNode,
		domain.TopologyRoleStalePrimary,
	}
	for _, role := range roles {
		t.Run(string(role), func(t *testing.T) {
			id := &supervisorIdentity{value: supervisorIdentityValue(role), loaded: true}
			cfg := &supervisorConfiguration{hostnames: configuration.EnrollmentHostnameConfig{Configured: true, Hostnames: []string{"primary.example"}}, materials: []configuration.EnrollmentTLSMaterial{supervisorMaterial(fixture.first, fixture.ca.CertificatePEM)}}
			factory := new(supervisorFactory)
			supervisor, err := enrollment.NewEnrollmentTLSSupervisor(supervisorOptions(id, cfg, factory, new(supervisorPublisher)))
			if err != nil {
				t.Fatal(err)
			}
			if err := supervisor.Reconcile(context.Background()); err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			if got := factory.calls(); got != 0 {
				t.Fatalf("listener calls = %d, want 0", got)
			}
			if got := cfg.ensureCalls; got != 0 {
				t.Fatalf("material ensure calls = %d, want 0", got)
			}
			if status := supervisor.Status(); status.Running || status.State != enrollment.EnrollmentTLSStateIneligible {
				t.Fatalf("status = %#v", status)
			}
		})
	}

	t.Run("unloaded", func(t *testing.T) {
		id := &supervisorIdentity{value: supervisorIdentityValue(domain.TopologyRolePrimary)}
		cfg := &supervisorConfiguration{hostnames: configuration.EnrollmentHostnameConfig{Configured: true, Hostnames: []string{"primary.example"}}}
		factory := new(supervisorFactory)
		supervisor, err := enrollment.NewEnrollmentTLSSupervisor(supervisorOptions(id, cfg, factory, new(supervisorPublisher)))
		if err != nil {
			t.Fatal(err)
		}
		if err := supervisor.Reconcile(context.Background()); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		if factory.calls() != 0 || supervisor.Status().Running {
			t.Fatalf("unloaded identity started listener: status=%#v calls=%d", supervisor.Status(), factory.calls())
		}
	})
}

func TestEnrollmentTLSSupervisorStartsOnlyAfterMaterialAndBindSucceed(t *testing.T) {
	fixture := newProviderFixture(t)
	secret := "private-material-marker"
	invalid := configuration.EnrollmentTLSMaterial{CertificatePEM: fixture.first.CertificatePEM, PrivateKeyPEM: secret, CACertificatePEM: fixture.ca.CertificatePEM}
	id := &supervisorIdentity{value: supervisorIdentityValue(domain.TopologyRolePrimary), loaded: true}
	cfg := &supervisorConfiguration{
		hostnames: configuration.EnrollmentHostnameConfig{Configured: true, Hostnames: []string{"primary.example"}},
		materials: []configuration.EnrollmentTLSMaterial{
			invalid,
			supervisorMaterial(fixture.first, fixture.ca.CertificatePEM),
			supervisorMaterial(fixture.first, fixture.ca.CertificatePEM),
		},
	}
	factory := &supervisorFactory{errs: []error{errors.New("bind failed")}}
	publisher := new(supervisorPublisher)
	supervisor, err := enrollment.NewEnrollmentTLSSupervisor(supervisorOptions(id, cfg, factory, publisher))
	if err != nil {
		t.Fatal(err)
	}
	if err := supervisor.Reconcile(context.Background()); !errors.Is(err, enrollment.ErrEnrollmentTLSMaterialUnavailable) {
		t.Fatalf("invalid material error = %v", err)
	}
	if factory.calls() != 0 || supervisor.Status().Running || strings.Contains(supervisor.Status().LastError, secret) {
		t.Fatalf("invalid material status/listener = %#v calls=%d", supervisor.Status(), factory.calls())
	}
	if err := supervisor.Reconcile(context.Background()); !errors.Is(err, enrollment.ErrEnrollmentTLSBind) {
		t.Fatalf("bind error = %v", err)
	}
	if factory.calls() != 0 || supervisor.Status().Running {
		t.Fatalf("failed bind left running listener: status=%#v calls=%d", supervisor.Status(), factory.calls())
	}
	if err := supervisor.Reconcile(context.Background()); err != nil {
		t.Fatalf("retry Reconcile: %v", err)
	}
	if factory.calls() != 1 || publisher.calls != 2 || !supervisor.Status().Running {
		t.Fatalf("retry status/publish/listen = %#v/%d/%d", supervisor.Status(), publisher.calls, factory.calls())
	}
	if err := supervisor.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestEnrollmentTLSSupervisorRefreshReplacesProviderWithoutRebinding(t *testing.T) {
	fixture := newProviderFixture(t)
	next, err := acme.IssueSignedByCA([]string{"next.example"}, 30, fixture.ca.CertificatePEM, fixture.ca.PrivateKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	id := &supervisorIdentity{value: supervisorIdentityValue(domain.TopologyRolePrimary), loaded: true}
	cfg := &supervisorConfiguration{hostnames: configuration.EnrollmentHostnameConfig{Configured: true, Hostnames: []string{"primary.example"}}, materials: []configuration.EnrollmentTLSMaterial{supervisorMaterial(fixture.first, fixture.ca.CertificatePEM), supervisorMaterial(next, fixture.ca.CertificatePEM)}}
	factory := new(supervisorFactory)
	publisher := new(supervisorPublisher)
	supervisor, err := enrollment.NewEnrollmentTLSSupervisor(supervisorOptions(id, cfg, factory, publisher))
	if err != nil {
		t.Fatal(err)
	}
	if err := supervisor.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	cfg.mu.Lock()
	cfg.hostnames = configuration.EnrollmentHostnameConfig{Configured: true, Hostnames: []string{"next.example"}}
	cfg.mu.Unlock()
	if err := supervisor.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if factory.calls() != 1 || publisher.calls != 1 {
		t.Fatalf("refresh rebound or republished: listens=%d publishes=%d", factory.calls(), publisher.calls)
	}
	if got := providerSerial(t, publisher.provider); got != parseCert(t, next.CertificatePEM).SerialNumber.String() {
		t.Fatalf("provider serial = %s, want renewed %s", got, parseCert(t, next.CertificatePEM).SerialNumber)
	}
	if err := supervisor.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestEnrollmentTLSSupervisorInvalidRefreshPreservesRunningProvider(t *testing.T) {
	fixture := newProviderFixture(t)
	secret := "invalid-refresh-private-material"
	invalid := configuration.EnrollmentTLSMaterial{CertificatePEM: fixture.second.CertificatePEM, PrivateKeyPEM: secret, CACertificatePEM: fixture.ca.CertificatePEM}
	id := &supervisorIdentity{value: supervisorIdentityValue(domain.TopologyRolePrimary), loaded: true}
	cfg := &supervisorConfiguration{hostnames: configuration.EnrollmentHostnameConfig{Configured: true, Hostnames: []string{"primary.example"}}, materials: []configuration.EnrollmentTLSMaterial{supervisorMaterial(fixture.first, fixture.ca.CertificatePEM), invalid}}
	factory := new(supervisorFactory)
	publisher := new(supervisorPublisher)
	supervisor, err := enrollment.NewEnrollmentTLSSupervisor(supervisorOptions(id, cfg, factory, publisher))
	if err != nil {
		t.Fatal(err)
	}
	if err := supervisor.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := providerSerial(t, publisher.provider)
	if err := supervisor.Reconcile(context.Background()); !errors.Is(err, enrollment.ErrEnrollmentTLSMaterialUnavailable) {
		t.Fatalf("invalid refresh error = %v", err)
	}
	status := supervisor.Status()
	if !status.Running || factory.calls() != 1 || providerSerial(t, publisher.provider) != before || strings.Contains(status.LastError, secret) {
		t.Fatalf("invalid refresh changed state: status=%#v calls=%d serial=%s", status, factory.calls(), providerSerial(t, publisher.provider))
	}
	if err := supervisor.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}
