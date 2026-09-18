package enrollment

import (
	"context"
	"errors"
	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/identity"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const defaultEnrollmentTLSReconcileInterval, defaultEnrollmentTLSRetryMin, defaultEnrollmentTLSRetryMax, defaultEnrollmentTLSShutdownTimeout = 30 * time.Second, time.Second, 30 * time.Second, 5 * time.Second

var (
	ErrEnrollmentTLSSupervisorDependencyRequired, ErrEnrollmentTLSInvalidSupervisorOptions                             = errors.New("enrollment TLS supervisor dependency is required"), errors.New("invalid enrollment TLS supervisor options")
	ErrEnrollmentTLSSupervisorContextRequired, ErrEnrollmentTLSSupervisorAlreadyRunning                                = errors.New("enrollment TLS supervisor context is required"), errors.New("enrollment TLS supervisor is already running")
	ErrEnrollmentTLSIdentityUnavailable, ErrEnrollmentTLSConfigurationUnavailable, ErrEnrollmentTLSMaterialUnavailable = errors.New("enrollment TLS identity is unavailable"), errors.New("enrollment TLS configuration is unavailable"), errors.New("enrollment TLS material is unavailable")
	ErrEnrollmentTLSProviderPublication, ErrEnrollmentTLSBind, ErrEnrollmentTLSServe, ErrEnrollmentTLSShutdown         = errors.New("enrollment TLS provider publication failed"), errors.New("enrollment TLS listener bind failed"), errors.New("enrollment TLS listener stopped unexpectedly"), errors.New("enrollment TLS listener shutdown failed")
)

type EnrollmentTLSState string

const EnrollmentTLSStateStopped, EnrollmentTLSStateIneligible, EnrollmentTLSStateRunning, EnrollmentTLSStateDegraded EnrollmentTLSState = "stopped", "ineligible", "running", "degraded"

type EnrollmentTLSIdentityCurrent interface {
	Current(context.Context) (identity.Identity, bool, error)
}
type EnrollmentTLSConfiguration interface {
	GetEnrollmentHostnames(context.Context) (configuration.EnrollmentHostnameConfig, error)
	EnsureEnrollmentTLSMaterial(context.Context) (configuration.EnrollmentTLSMaterial, error)
}
type EnrollmentTLSListener interface {
	Serve() error
	Shutdown(context.Context) error
}

// EnrollmentTLSListenerFactory is a trusted construction seam. PNE-2E-D
// production wiring must bind the address and construct NewEnrollmentTLSServer;
// injected factories alone do not prove proof-only TLS security.
type EnrollmentTLSListenerFactory interface {
	Listen(context.Context, string, *TLSCertificateProvider) (EnrollmentTLSListener, error)
}
type EnrollmentTLSProviderPublisher interface {
	PublishEnrollmentTLSProvider(*TLSCertificateProvider) error
}
type EnrollmentTLSClock interface {
	After(time.Duration) <-chan time.Time
}
type wallEnrollmentTLSClock struct{}

func (wallEnrollmentTLSClock) After(delay time.Duration) <-chan time.Time { return time.After(delay) }

type EnrollmentTLSSupervisorOptions struct {
	Identity          EnrollmentTLSIdentityCurrent
	Configuration     EnrollmentTLSConfiguration
	ListenerFactory   EnrollmentTLSListenerFactory
	ProviderPublisher EnrollmentTLSProviderPublisher
	Clock             EnrollmentTLSClock
	Address           string
	ReconcileInterval time.Duration
	RetryMin          time.Duration
	RetryMax          time.Duration
	ShutdownTimeout   time.Duration
}
type EnrollmentTLSSupervisorStatus struct {
	State   EnrollmentTLSState
	Running bool
	Address string
	Role    domain.TopologyRole
	Loaded  bool
	// Eligible is full current eligibility; the other flags expose each prerequisite.
	RoleEligible, Configured, MaterialReady, Eligible bool
	LastError                                         string
	ConsecutiveFailures                               int
}
type EnrollmentTLSSupervisor struct {
	identity                                      EnrollmentTLSIdentityCurrent
	configuration                                 EnrollmentTLSConfiguration
	listenerFactory                               EnrollmentTLSListenerFactory
	providerPublisher                             EnrollmentTLSProviderPublisher
	address                                       string
	interval, retryMin, retryMax, shutdownTimeout time.Duration
	clock                                         EnrollmentTLSClock
	mu                                            sync.RWMutex
	status                                        EnrollmentTLSSupervisorStatus
	running                                       *runningEnrollmentTLS
	loopRunning                                   bool
	wake                                          chan struct{}
	reconcileMu                                   sync.Mutex
}
type runningEnrollmentTLS struct {
	listener EnrollmentTLSListener
	provider *TLSCertificateProvider
	material TLSCertificateMaterial
	done     chan error
	stopping bool
}

func NewEnrollmentTLSSupervisor(opts EnrollmentTLSSupervisorOptions) (*EnrollmentTLSSupervisor, error) {
	if opts.Identity == nil || opts.Configuration == nil || opts.ListenerFactory == nil {
		return nil, ErrEnrollmentTLSSupervisorDependencyRequired
	}
	address := strings.TrimSpace(opts.Address)
	if address == "" {
		return nil, ErrEnrollmentTLSInvalidSupervisorOptions
	}
	opts.ReconcileInterval = enrollmentTLSDurationDefault(opts.ReconcileInterval, defaultEnrollmentTLSReconcileInterval)
	opts.RetryMin = enrollmentTLSDurationDefault(opts.RetryMin, defaultEnrollmentTLSRetryMin)
	opts.RetryMax = enrollmentTLSDurationDefault(opts.RetryMax, defaultEnrollmentTLSRetryMax)
	opts.ShutdownTimeout = enrollmentTLSDurationDefault(opts.ShutdownTimeout, defaultEnrollmentTLSShutdownTimeout)
	if opts.ReconcileInterval <= 0 || opts.RetryMin <= 0 || opts.RetryMax < opts.RetryMin || opts.ShutdownTimeout <= 0 {
		return nil, ErrEnrollmentTLSInvalidSupervisorOptions
	}
	clock := opts.Clock
	if clock == nil {
		clock = wallEnrollmentTLSClock{}
	}
	return &EnrollmentTLSSupervisor{
		identity: opts.Identity, configuration: opts.Configuration, listenerFactory: opts.ListenerFactory,
		providerPublisher: opts.ProviderPublisher, address: address, interval: opts.ReconcileInterval,
		retryMin: opts.RetryMin, retryMax: opts.RetryMax, shutdownTimeout: opts.ShutdownTimeout,
		clock: clock, wake: make(chan struct{}, 1),
		status: EnrollmentTLSSupervisorStatus{State: EnrollmentTLSStateStopped, Address: address},
	}, nil
}
func (s *EnrollmentTLSSupervisor) Status() EnrollmentTLSSupervisorStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.status
}

func (s *EnrollmentTLSSupervisor) Trigger() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}
func (s *EnrollmentTLSSupervisor) Run(ctx context.Context) error {
	if s == nil || ctx == nil {
		return ErrEnrollmentTLSSupervisorContextRequired
	}
	s.mu.Lock()
	if s.loopRunning {
		s.mu.Unlock()
		return ErrEnrollmentTLSSupervisorAlreadyRunning
	}
	s.loopRunning = true
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.loopRunning = false; s.mu.Unlock() }()
	var delay time.Duration
	for {
		if delay > 0 && !s.wait(ctx, delay) {
			break
		}
		if ctx.Err() != nil {
			break
		}
		err := s.Reconcile(ctx)
		if ctx.Err() != nil {
			break
		}
		if err != nil {
			delay = s.retryDelay()
		} else {
			delay = s.interval
		}
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), s.shutdownTimeout)
	defer cancel()
	return s.Shutdown(shutdownCtx)
}
func (s *EnrollmentTLSSupervisor) wait(ctx context.Context, delay time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-s.wake:
		return true
	case <-s.clock.After(delay):
		return true
	}
}
func (s *EnrollmentTLSSupervisor) retryDelay() time.Duration {
	delay := s.retryMin
	for i, failures := 1, s.Status().ConsecutiveFailures; i < failures && delay < s.retryMax; i++ {
		if delay > s.retryMax/2 {
			return s.retryMax
		}
		delay *= 2
	}
	if delay > s.retryMax {
		return s.retryMax
	}
	return delay
}
func (s *EnrollmentTLSSupervisor) Reconcile(ctx context.Context) error {
	if s == nil || ctx == nil {
		return ErrEnrollmentTLSSupervisorContextRequired
	}
	s.reconcileMu.Lock()
	defer s.reconcileMu.Unlock()
	return s.reconcile(ctx)
}
func (s *EnrollmentTLSSupervisor) reconcile(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.observeServeResult(); err != nil {
		return err
	}
	current, loaded, err := s.identity.Current(ctx)
	if err != nil {
		s.setIdentityObservation(identity.Identity{}, false, false)
		return s.fail(ErrEnrollmentTLSIdentityUnavailable)
	}
	s.setIdentityObservation(current, loaded, false)
	if !loaded || !(current.Role == domain.TopologyRolePrimary || current.Role == domain.TopologyRolePrimaryWithNodes) || current.IsStalePrimary() {
		return s.disable(ctx)
	}
	hostnames, err := s.configuration.GetEnrollmentHostnames(ctx)
	if err != nil {
		return s.fail(ErrEnrollmentTLSConfigurationUnavailable)
	}
	configured := hostnames.Configured && len(hostnames.Hostnames) != 0
	s.mu.Lock()
	s.status.Configured, s.status.MaterialReady, s.status.Eligible = configured, false, false
	s.mu.Unlock()
	if !configured {
		return s.disable(ctx)
	}
	configuredMaterial, err := s.configuration.EnsureEnrollmentTLSMaterial(ctx)
	if err != nil {
		return s.fail(ErrEnrollmentTLSMaterialUnavailable)
	}
	next := TLSCertificateMaterial{CertificatePEM: configuredMaterial.CertificatePEM, PrivateKeyPEM: configuredMaterial.PrivateKeyPEM, CACertificatePEM: configuredMaterial.CACertificatePEM}
	s.mu.RLock()
	running := s.running
	s.mu.RUnlock()
	if running != nil {
		if sameTLSCertificateMaterial(running.material, next) {
			s.markMaterialReady()
			s.setState(EnrollmentTLSStateRunning, "")
			return nil
		}
		if err := running.provider.Replace(next); err != nil {
			return s.fail(ErrEnrollmentTLSMaterialUnavailable)
		}
		s.mu.Lock()
		if s.running == running {
			running.material = next
		}
		s.mu.Unlock()
		s.markMaterialReady()
		s.setState(EnrollmentTLSStateRunning, "")
		return nil
	}
	provider, err := NewTLSCertificateProvider(next)
	if err != nil {
		return s.fail(ErrEnrollmentTLSMaterialUnavailable)
	}
	s.markMaterialReady()
	return s.startRunning(ctx, provider, next)
}
func (s *EnrollmentTLSSupervisor) disable(ctx context.Context) error {
	if err := s.stopRunning(ctx); err != nil {
		return err
	}
	s.setState(EnrollmentTLSStateIneligible, "")
	return nil
}
func (s *EnrollmentTLSSupervisor) publish(provider *TLSCertificateProvider) error {
	if s.providerPublisher == nil {
		return nil
	}
	return s.providerPublisher.PublishEnrollmentTLSProvider(provider)
}
func (s *EnrollmentTLSSupervisor) startRunning(ctx context.Context, provider *TLSCertificateProvider, material TLSCertificateMaterial) error {
	if err := s.publish(provider); err != nil {
		return s.fail(ErrEnrollmentTLSProviderPublication)
	}
	listener, err := s.listenerFactory.Listen(ctx, s.address, provider)
	if err != nil || listener == nil {
		return s.fail(ErrEnrollmentTLSBind)
	}
	running := &runningEnrollmentTLS{listener: listener, provider: provider, material: material, done: make(chan error, 1)}
	s.mu.Lock()
	s.running = running
	s.status.State, s.status.Running = EnrollmentTLSStateRunning, true
	s.mu.Unlock()
	go s.serve(running)
	s.setState(EnrollmentTLSStateRunning, "")
	return nil
}
func (s *EnrollmentTLSSupervisor) serve(running *runningEnrollmentTLS) {
	running.done <- running.listener.Serve()
	s.Trigger()
}
func (s *EnrollmentTLSSupervisor) observeServeResult() error {
	s.mu.Lock()
	running := s.running
	if running == nil {
		s.mu.Unlock()
		return nil
	}
	select {
	case <-running.done:
		s.running, s.status.Running = nil, false
		if running.stopping {
			s.mu.Unlock()
			return nil
		}
		s.status.State, s.status.LastError = EnrollmentTLSStateDegraded, ErrEnrollmentTLSServe.Error()
		s.status.ConsecutiveFailures++
		s.mu.Unlock()
		return ErrEnrollmentTLSServe
	default:
		s.mu.Unlock()
		return nil
	}
}
func (s *EnrollmentTLSSupervisor) stopRunning(ctx context.Context) error {
	s.mu.Lock()
	running := s.running
	if running == nil {
		s.mu.Unlock()
		return nil
	}
	running.stopping = true
	s.status.State, s.status.Running = EnrollmentTLSStateRunning, true
	s.mu.Unlock()
	shutdownCtx, cancel := context.WithTimeout(ctx, s.shutdownTimeout)
	defer cancel()
	shutdownErr := running.listener.Shutdown(shutdownCtx)
	var serveErr error
	select {
	case serveErr = <-running.done:
	case <-shutdownCtx.Done():
		s.mu.Lock()
		running.stopping = false
		s.mu.Unlock()
		return s.fail(ErrEnrollmentTLSShutdown)
	}
	s.mu.Lock()
	if s.running == running {
		s.running, s.status.Running = nil, false
	}
	s.mu.Unlock()
	if shutdownErr != nil || !(serveErr == nil || errors.Is(serveErr, http.ErrServerClosed) || errors.Is(serveErr, net.ErrClosed)) {
		return s.fail(ErrEnrollmentTLSShutdown)
	}
	return nil
}
func (s *EnrollmentTLSSupervisor) Shutdown(ctx context.Context) error {
	if s == nil || ctx == nil {
		return ErrEnrollmentTLSSupervisorContextRequired
	}
	s.reconcileMu.Lock()
	defer s.reconcileMu.Unlock()
	if err := s.stopRunning(ctx); err != nil {
		return err
	}
	s.setState(EnrollmentTLSStateStopped, "")
	return nil
}
func (s *EnrollmentTLSSupervisor) setIdentityObservation(current identity.Identity, loaded, configured bool) {
	s.mu.Lock()
	s.status.Role, s.status.Loaded, s.status.Configured = current.Role, loaded, configured
	s.status.RoleEligible = loaded && (current.Role == domain.TopologyRolePrimary || current.Role == domain.TopologyRolePrimaryWithNodes) && !current.IsStalePrimary()
	s.status.MaterialReady, s.status.Eligible = false, false
	s.mu.Unlock()
}
func (s *EnrollmentTLSSupervisor) markMaterialReady() {
	s.mu.Lock()
	s.status.MaterialReady = true
	s.status.Eligible = s.status.RoleEligible && s.status.Configured
	s.mu.Unlock()
}
func (s *EnrollmentTLSSupervisor) setState(state EnrollmentTLSState, lastError string) {
	s.mu.Lock()
	s.status.State, s.status.Running, s.status.LastError, s.status.ConsecutiveFailures = state, s.running != nil, lastError, 0
	s.mu.Unlock()
}
func (s *EnrollmentTLSSupervisor) fail(kind error) error {
	s.mu.Lock()
	s.status.State, s.status.Running, s.status.LastError, s.status.ConsecutiveFailures = EnrollmentTLSStateDegraded, s.running != nil, kind.Error(), s.status.ConsecutiveFailures+1
	s.mu.Unlock()
	return kind
}
func enrollmentTLSDurationDefault(value, fallback time.Duration) time.Duration {
	if value == 0 {
		return fallback
	}
	return value
}
func sameTLSCertificateMaterial(left, right TLSCertificateMaterial) bool {
	return left.CertificatePEM == right.CertificatePEM && left.PrivateKeyPEM == right.PrivateKeyPEM && left.CACertificatePEM == right.CACertificatePEM
}
