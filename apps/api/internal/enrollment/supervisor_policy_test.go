package enrollment_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/enrollment"
)

type supervisorFakeClock struct {
	mu       sync.Mutex
	pending  []supervisorFakeTimer
	requests chan time.Duration
}

type supervisorFakeTimer struct {
	delay time.Duration
	ch    chan time.Time
}

func newSupervisorFakeClock() *supervisorFakeClock {
	return &supervisorFakeClock{requests: make(chan time.Duration, 16)}
}

func (c *supervisorFakeClock) After(delay time.Duration) <-chan time.Time {
	timer := supervisorFakeTimer{delay: delay, ch: make(chan time.Time, 1)}
	c.mu.Lock()
	c.pending = append(c.pending, timer)
	c.mu.Unlock()
	c.requests <- delay
	return timer.ch
}

func (c *supervisorFakeClock) fire(t *testing.T, want time.Duration) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, timer := range c.pending {
		if timer.delay == want {
			c.pending = append(c.pending[:i], c.pending[i+1:]...)
			timer.ch <- time.Time{}
			return
		}
	}
	t.Fatalf("no pending fake timer for %s", want)
}

func awaitSupervisor[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for deterministic supervisor event")
		var zero T
		return zero
	}
}

func TestEnrollmentTLSSupervisorStatusSeparatesRoleAndRunnableEligibility(t *testing.T) {
	fixture := newProviderFixture(t)
	id := &supervisorIdentity{value: supervisorIdentityValue(domain.TopologyRolePrimary), loaded: true}
	cfg := &supervisorConfiguration{hostnames: configuration.EnrollmentHostnameConfig{Configured: false}}
	supervisor, err := enrollment.NewEnrollmentTLSSupervisor(supervisorOptions(id, cfg, new(supervisorFactory), new(supervisorPublisher)))
	if err != nil {
		t.Fatal(err)
	}
	if err := supervisor.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	status := supervisor.Status()
	if !status.RoleEligible || status.Eligible || status.Configured || status.MaterialReady || status.Running {
		t.Fatalf("unconfigured primary status = %#v", status)
	}

	cfg.mu.Lock()
	cfg.hostnames = configuration.EnrollmentHostnameConfig{Configured: true, Hostnames: []string{"primary.example"}}
	cfg.materials = []configuration.EnrollmentTLSMaterial{supervisorMaterial(fixture.first, fixture.ca.CertificatePEM)}
	cfg.mu.Unlock()
	if err := supervisor.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	status = supervisor.Status()
	if !status.RoleEligible || !status.Eligible || !status.Configured || !status.MaterialReady || !status.Running {
		t.Fatalf("configured primary status = %#v", status)
	}
	if err := supervisor.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestEnrollmentTLSSupervisorUnexpectedServeErrorIsSafeAndRetryable(t *testing.T) {
	fixture := newProviderFixture(t)
	id := &supervisorIdentity{value: supervisorIdentityValue(domain.TopologyRolePrimary), loaded: true}
	cfg := &supervisorConfiguration{
		hostnames: configuration.EnrollmentHostnameConfig{Configured: true, Hostnames: []string{"primary.example"}},
		materials: []configuration.EnrollmentTLSMaterial{
			supervisorMaterial(fixture.first, fixture.ca.CertificatePEM),
			supervisorMaterial(fixture.first, fixture.ca.CertificatePEM),
		},
	}
	factory := &supervisorFactory{created: make(chan *supervisorListener, 2)}
	supervisor, err := enrollment.NewEnrollmentTLSSupervisor(supervisorOptions(id, cfg, factory, new(supervisorPublisher)))
	if err != nil {
		t.Fatal(err)
	}
	if err := supervisor.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := awaitSupervisor(t, factory.created)
	secret := "serve-private-detail"
	first.finish(errors.New(secret))
	awaitSupervisor(t, first.done)
	if err := supervisor.Reconcile(context.Background()); !errors.Is(err, enrollment.ErrEnrollmentTLSServe) {
		t.Fatalf("serve error = %v", err)
	}
	status := supervisor.Status()
	if status.State != enrollment.EnrollmentTLSStateDegraded || status.Running || !status.Eligible || strings.Contains(status.LastError, secret) {
		t.Fatalf("unexpected serve status = %#v", status)
	}
	if err := supervisor.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := awaitSupervisor(t, factory.created); got == first {
		t.Fatal("retry reused the failed listener")
	}
	if err := supervisor.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestEnrollmentTLSSupervisorRunUsesDeterministicBoundedRetry(t *testing.T) {
	fixture := newProviderFixture(t)
	id := &supervisorIdentity{value: supervisorIdentityValue(domain.TopologyRolePrimary), loaded: true}
	cfg := &supervisorConfiguration{
		hostnames: configuration.EnrollmentHostnameConfig{Configured: true, Hostnames: []string{"primary.example"}},
		materials: []configuration.EnrollmentTLSMaterial{
			supervisorMaterial(fixture.first, fixture.ca.CertificatePEM),
			supervisorMaterial(fixture.first, fixture.ca.CertificatePEM),
		},
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
	if got := awaitSupervisor(t, clock.requests); got != opts.ReconcileInterval {
		t.Fatalf("initial interval = %s, want %s", got, opts.ReconcileInterval)
	}
	first.finish(errors.New("unexpected serve detail"))
	awaitSupervisor(t, first.done)
	retry := awaitSupervisor(t, clock.requests)
	if retry != opts.RetryMin {
		t.Fatalf("retry interval = %s, want bounded minimum %s", retry, opts.RetryMin)
	}
	clock.fire(t, retry)
	awaitSupervisor(t, factory.created)
	cancel()
	if err := awaitSupervisor(t, runDone); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestEnrollmentTLSSupervisorFactoryGetsImmutableAddressAndProvider(t *testing.T) {
	fixture := newProviderFixture(t)
	id := &supervisorIdentity{value: supervisorIdentityValue(domain.TopologyRolePrimary), loaded: true}
	cfg := &supervisorConfiguration{
		hostnames: configuration.EnrollmentHostnameConfig{Configured: true, Hostnames: []string{"primary.example"}},
		materials: []configuration.EnrollmentTLSMaterial{supervisorMaterial(fixture.first, fixture.ca.CertificatePEM), supervisorMaterial(fixture.first, fixture.ca.CertificatePEM)},
	}
	factory := &supervisorFactory{created: make(chan *supervisorListener, 2)}
	publisher := new(supervisorPublisher)
	opts := supervisorOptions(id, cfg, factory, publisher)
	opts.Address = "  127.0.0.1:3555  "
	supervisor, err := enrollment.NewEnrollmentTLSSupervisor(opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := supervisor.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	awaitSupervisor(t, factory.created)
	factory.mu.Lock()
	address := factory.addresses[0]
	provider := factory.providers[0]
	factory.mu.Unlock()
	if address != "127.0.0.1:3555" || provider != publisher.provider {
		t.Fatalf("factory inputs = address %q provider=%p, want immutable address and published provider %p", address, provider, publisher.provider)
	}
	if status := supervisor.Status(); status.Address != "127.0.0.1:3555" {
		t.Fatalf("status address = %q", status.Address)
	}
	if err := supervisor.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if factory.calls() != 1 {
		t.Fatalf("same address/material rebound listener: %d calls", factory.calls())
	}
	if err := supervisor.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestEnrollmentTLSSupervisorStatusSupportsConcurrentReaders(t *testing.T) {
	id := &supervisorIdentity{value: supervisorIdentityValue(domain.TopologyRoleStandalone), loaded: true}
	cfg := &supervisorConfiguration{}
	supervisor, err := enrollment.NewEnrollmentTLSSupervisor(supervisorOptions(id, cfg, new(supervisorFactory), new(supervisorPublisher)))
	if err != nil {
		t.Fatal(err)
	}
	const readers = 16
	ready := make(chan struct{}, readers)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ready <- struct{}{}
			for {
				select {
				case <-stop:
					return
				default:
					supervisor.Status()
				}
			}
		}()
	}
	for i := 0; i < readers; i++ {
		awaitSupervisor(t, ready)
	}
	for i := 0; i < 64; i++ {
		id.mu.Lock()
		if i%2 == 0 {
			id.value.Role = domain.TopologyRolePrimary
		} else {
			id.value.Role = domain.TopologyRoleStandalone
		}
		id.mu.Unlock()
		if err := supervisor.Reconcile(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()
}

func TestEnrollmentTLSSupervisorRetryBackoffIsBoundedByFakeClock(t *testing.T) {
	fixture := newProviderFixture(t)
	id := &supervisorIdentity{value: supervisorIdentityValue(domain.TopologyRolePrimary), loaded: true}
	material := supervisorMaterial(fixture.first, fixture.ca.CertificatePEM)
	cfg := &supervisorConfiguration{
		hostnames: configuration.EnrollmentHostnameConfig{Configured: true, Hostnames: []string{"primary.example"}},
		materials: []configuration.EnrollmentTLSMaterial{material, material, material},
	}
	factory := &supervisorFactory{
		errs:    []error{errors.New("first bind failure"), errors.New("second bind failure")},
		created: make(chan *supervisorListener, 1),
	}
	clock := newSupervisorFakeClock()
	opts := supervisorOptions(id, cfg, factory, new(supervisorPublisher))
	opts.Clock, opts.RetryMax = clock, 7*time.Millisecond
	supervisor, err := enrollment.NewEnrollmentTLSSupervisor(opts)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- supervisor.Run(ctx) }()
	first := awaitSupervisor(t, clock.requests)
	if first != opts.RetryMin {
		t.Fatalf("first retry interval = %s, want %s", first, opts.RetryMin)
	}
	clock.fire(t, first)
	second := awaitSupervisor(t, clock.requests)
	if second != opts.RetryMax {
		t.Fatalf("second retry interval = %s, want bounded %s", second, opts.RetryMax)
	}
	clock.fire(t, second)
	listener := awaitSupervisor(t, factory.created)
	awaitSupervisor(t, listener.started)
	cancel()
	if err := awaitSupervisor(t, runDone); err != nil {
		t.Fatalf("Run: %v", err)
	}
	awaitSupervisor(t, listener.done)
}
