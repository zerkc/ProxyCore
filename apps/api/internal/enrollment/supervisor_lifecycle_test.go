package enrollment_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/enrollment"
)

func TestEnrollmentTLSSupervisorRunRetriesBindAndStopsOnCancellation(t *testing.T) {
	fixture := newProviderFixture(t)
	id := &supervisorIdentity{value: supervisorIdentityValue(domain.TopologyRolePrimary), loaded: true}
	cfg := &supervisorConfiguration{
		hostnames: configuration.EnrollmentHostnameConfig{Configured: true, Hostnames: []string{"primary.example"}},
		materials: []configuration.EnrollmentTLSMaterial{
			supervisorMaterial(fixture.first, fixture.ca.CertificatePEM),
			supervisorMaterial(fixture.first, fixture.ca.CertificatePEM),
		},
	}
	factory := &supervisorFactory{errs: []error{errors.New("temporary bind failure")}, created: make(chan *supervisorListener, 1)}
	clock := newSupervisorFakeClock()
	opts := supervisorOptions(id, cfg, factory, new(supervisorPublisher))
	opts.Clock = clock
	supervisor, err := enrollment.NewEnrollmentTLSSupervisor(opts)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- supervisor.Run(ctx) }()
	retry := awaitSupervisor(t, clock.requests)
	if retry != opts.RetryMin {
		t.Fatalf("retry interval = %s, want %s", retry, opts.RetryMin)
	}
	clock.fire(t, retry)
	listener := awaitSupervisor(t, factory.created)
	awaitSupervisor(t, listener.started)
	interval := awaitSupervisor(t, clock.requests)
	if interval != opts.ReconcileInterval {
		t.Fatalf("steady interval = %s, want %s", interval, opts.ReconcileInterval)
	}
	cancel()
	if err := awaitSupervisor(t, runDone); err != nil {
		t.Fatalf("Run after cancellation: %v", err)
	}
	awaitSupervisor(t, listener.done)
	if listener.stopCount() != 1 || supervisor.Status().Running || supervisor.Status().State != enrollment.EnrollmentTLSStateStopped {
		t.Fatalf("shutdown state/listener = %#v/%d", supervisor.Status(), listener.stopCount())
	}
}

func TestEnrollmentTLSSupervisorRoleTransitionsStopAndRestartExactlyOnce(t *testing.T) {
	fixture := newProviderFixture(t)
	id := &supervisorIdentity{value: supervisorIdentityValue(domain.TopologyRolePrimary), loaded: true}
	cfg := &supervisorConfiguration{
		hostnames: configuration.EnrollmentHostnameConfig{Configured: true, Hostnames: []string{"primary.example"}},
		materials: []configuration.EnrollmentTLSMaterial{supervisorMaterial(fixture.first, fixture.ca.CertificatePEM)},
	}
	factory := &supervisorFactory{created: make(chan *supervisorListener, 2)}
	clock := newSupervisorFakeClock()
	opts := supervisorOptions(id, cfg, factory, new(supervisorPublisher))
	opts.Clock = clock
	supervisor, err := enrollment.NewEnrollmentTLSSupervisor(opts)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- supervisor.Run(ctx) }()
	first := awaitSupervisor(t, factory.created)
	awaitSupervisor(t, first.started)
	awaitSupervisor(t, clock.requests)

	id.mu.Lock()
	id.value.Role = domain.TopologyRoleNode
	id.mu.Unlock()
	supervisor.Trigger()
	awaitSupervisor(t, first.done)
	awaitSupervisor(t, clock.requests)
	status := supervisor.Status()
	if status.State != enrollment.EnrollmentTLSStateIneligible || status.Running {
		t.Fatalf("role transition status = %#v", status)
	}
	if first.stopCount() != 1 {
		t.Fatalf("role transition shutdown count = %d", first.stopCount())
	}

	cfg.mu.Lock()
	cfg.materials = append(cfg.materials, supervisorMaterial(fixture.second, fixture.ca.CertificatePEM))
	cfg.mu.Unlock()
	id.mu.Lock()
	id.value.Role = domain.TopologyRolePrimaryWithNodes
	id.mu.Unlock()
	supervisor.Trigger()
	second := awaitSupervisor(t, factory.created)
	awaitSupervisor(t, second.started)
	if first.stopCount() != 1 {
		t.Fatalf("old listener was stopped more than once: %d", first.stopCount())
	}
	cancel()
	if err := awaitSupervisor(t, runDone); err != nil {
		t.Fatalf("Run: %v", err)
	}
	awaitSupervisor(t, second.done)
}

func TestEnrollmentTLSSupervisorUnconfiguredStateCanBecomeEligible(t *testing.T) {
	fixture := newProviderFixture(t)
	id := &supervisorIdentity{value: supervisorIdentityValue(domain.TopologyRolePrimary), loaded: true}
	cfg := &supervisorConfiguration{hostnames: configuration.EnrollmentHostnameConfig{Configured: false}}
	factory := &supervisorFactory{created: make(chan *supervisorListener, 1)}
	supervisor, err := enrollment.NewEnrollmentTLSSupervisor(supervisorOptions(id, cfg, factory, new(supervisorPublisher)))
	if err != nil {
		t.Fatal(err)
	}
	if err := supervisor.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if status := supervisor.Status(); status.RoleEligible != true || status.Eligible || status.Configured || status.MaterialReady || status.Running {
		t.Fatalf("unconfigured status = %#v", status)
	}
	cfg.mu.Lock()
	cfg.hostnames = configuration.EnrollmentHostnameConfig{Configured: true, Hostnames: []string{"primary.example"}}
	cfg.materials = []configuration.EnrollmentTLSMaterial{supervisorMaterial(fixture.first, fixture.ca.CertificatePEM)}
	cfg.mu.Unlock()
	if err := supervisor.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	listener := awaitSupervisor(t, factory.created)
	awaitSupervisor(t, listener.started)
	if err := supervisor.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	awaitSupervisor(t, listener.done)
}

type overlapConfiguration struct {
	mu       sync.Mutex
	entered  chan struct{}
	release  chan struct{}
	active   int
	max      int
	hostname configuration.EnrollmentHostnameConfig
	material configuration.EnrollmentTLSMaterial
}

func (f *overlapConfiguration) GetEnrollmentHostnames(context.Context) (configuration.EnrollmentHostnameConfig, error) {
	return f.hostname, nil
}

func (f *overlapConfiguration) EnsureEnrollmentTLSMaterial(context.Context) (configuration.EnrollmentTLSMaterial, error) {
	f.mu.Lock()
	f.active++
	if f.active > f.max {
		f.max = f.active
	}
	select {
	case <-f.entered:
	default:
		close(f.entered)
	}
	f.mu.Unlock()
	<-f.release
	f.mu.Lock()
	f.active--
	f.mu.Unlock()
	return f.material, nil
}

func TestEnrollmentTLSSupervisorSerializesConcurrentReconciles(t *testing.T) {
	fixture := newProviderFixture(t)
	cfg := &overlapConfiguration{
		entered:  make(chan struct{}),
		release:  make(chan struct{}),
		hostname: configuration.EnrollmentHostnameConfig{Configured: true, Hostnames: []string{"primary.example"}},
		material: supervisorMaterial(fixture.first, fixture.ca.CertificatePEM),
	}
	id := &supervisorIdentity{value: supervisorIdentityValue(domain.TopologyRolePrimary), loaded: true}
	factory := new(supervisorFactory)
	supervisor, err := enrollment.NewEnrollmentTLSSupervisor(enrollment.EnrollmentTLSSupervisorOptions{
		Identity:          id,
		Configuration:     cfg,
		ListenerFactory:   factory,
		ProviderPublisher: new(supervisorPublisher),
		Address:           "127.0.0.1:3443",
		ReconcileInterval: 20 * time.Millisecond,
		RetryMin:          5 * time.Millisecond,
		RetryMax:          20 * time.Millisecond,
		ShutdownTimeout:   time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 2)
	go func() { results <- supervisor.Reconcile(context.Background()) }()
	awaitSupervisor(t, cfg.entered)
	go func() { results <- supervisor.Reconcile(context.Background()) }()
	close(cfg.release)
	for range 2 {
		if err := awaitSupervisor(t, results); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
	}
	cfg.mu.Lock()
	max := cfg.max
	cfg.mu.Unlock()
	if max != 1 {
		t.Fatalf("maximum concurrent ensure calls = %d", max)
	}
	if factory.calls() != 1 || !supervisor.Status().Running {
		t.Fatalf("serialized reconcile started unexpected listeners: calls=%d status=%#v", factory.calls(), supervisor.Status())
	}
	if err := supervisor.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}
