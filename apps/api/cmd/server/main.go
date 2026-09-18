package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/zerkc/ProxyCore/apps/api/internal/auth"
	"github.com/zerkc/ProxyCore/apps/api/internal/config"
	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/enrollment"
	"github.com/zerkc/ProxyCore/apps/api/internal/httpserver"
	"github.com/zerkc/ProxyCore/apps/api/internal/identity"
	"github.com/zerkc/ProxyCore/apps/api/internal/snapshot"
	"github.com/zerkc/ProxyCore/apps/api/internal/update"
	"github.com/zerkc/ProxyCore/apps/api/internal/version"
)

func bootstrapIdentity(ctx context.Context, store identity.Store) (*identity.Service, identity.Identity, bool, error) {
	service := identity.NewService(store)
	current, created, err := service.EnsureBootstrapped(ctx)
	if err != nil {
		return nil, identity.Identity{}, false, err
	}
	return service, current, created, nil
}

func renewalIdentityProvider(service *identity.Service) configuration.RenewalIdentityProvider {
	return func(ctx context.Context) (current identity.Identity, loaded bool, err error) {
		if service == nil || ctx == nil {
			return identity.Identity{}, false, context.Canceled
		}
		if err := ctx.Err(); err != nil {
			return identity.Identity{}, false, err
		}
		// Service.Current intentionally panics before startup loading. The
		// renewal worker must fail closed rather than let that invariant escape.
		defer func() {
			if recover() != nil {
				current, loaded, err = identity.Identity{}, false, nil
			}
		}()
		return service.Current(), true, nil
	}
}

func updaterClientFromConfig(cfg config.Config) httpserver.Option {
	baseURL := strings.TrimRight(strings.TrimSpace(cfg.UpdaterURL), "/")
	if baseURL == "" {
		return nil
	}
	return httpserver.WithUpdaterClient(&httpserver.UpdaterHTTPClient{
		URL:       baseURL + "/internal/apply",
		StatusURL: baseURL + "/internal/status",
		Client:    &http.Client{Timeout: 10 * time.Second},
	})
}

func main() {
	logger := log.New(os.Stdout, "", log.LstdFlags|log.Lmsgprefix)
	cfg, err := config.Load()
	if err != nil {
		logger.Fatalf("config: %v", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := runServer(ctx, cfg, logger); err != nil {
		logger.Fatalf("server: %v", err)
	}
}

func runServer(ctx context.Context, cfg config.Config, logger *log.Logger) error {
	if ctx == nil {
		return errors.New("process context is required")
	}
	if logger == nil {
		logger = log.Default()
	}

	var pool *pgxpool.Pool
	var configStore *configuration.Store
	var identitySvc *identity.Service
	var identityResult identity.Identity
	options := []httpserver.Option{
		httpserver.WithUpdateChecker(update.NewChecker(update.CheckerOptions{
			CurrentVersion: version.Version,
			Enabled:        cfg.UpdateCheckEnabled,
			TTL:            cfg.UpdateCheckInterval,
			Timeout:        cfg.UpdateCheckTimeout,
		})),
	}
	if opt := updaterClientFromConfig(cfg); opt != nil {
		options = append(options, opt)
	}
	if cfg.DatabaseURL != "" {
		connectCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		var err error
		pool, err = pgxpool.New(connectCtx, cfg.DatabaseURL)
		cancel()
		if err != nil {
			return fmt.Errorf("database: %w", err)
		}
		defer pool.Close()

		if err := pool.Ping(context.Background()); err != nil {
			return fmt.Errorf("database ping: %w", err)
		}
		// Drizzle migrate remains the primary path; EnsureSchema is idempotent and
		// covers additive tables (e.g. internal_ca) if migrate was not re-run yet.
		if err := configuration.EnsureSchema(context.Background(), pool); err != nil {
			return fmt.Errorf("configuration schema: %w", err)
		}
		store := auth.NewPostgresStore(pool)
		options = append(options, httpserver.WithAuthService(auth.NewService(store, auth.ServiceOptions{
			SessionTTL: cfg.SessionTTL,
		})))

		defaultIngress := domain.Ingress{IPv4: cfg.ProxyIngressIPv4, IPv6: cfg.ProxyIngressIPv6}
		configStore = configuration.New(pool, cfg.MasterKeyBase64, defaultIngress)
		options = append(options,
			httpserver.WithConfigurationStore(configStore),
			httpserver.WithDefaultIngress(defaultIngress),
		)

		// Load the durable PRIMARY/NODE identity, bootstrapping only when the
		// singleton row is absent. Persisted valid roles must survive restart;
		// the HTTP server enforces their writable/read-only boundary.
		identityStore := identity.NewPgStore(pool)
		identityCtx, identityCancel := context.WithTimeout(context.Background(), 10*time.Second)
		identitySvc, identityResult, _, err = bootstrapIdentity(identityCtx, identityStore)
		identityCancel()
		if err != nil {
			return fmt.Errorf("identity bootstrap: %w", err)
		}
		options = append(options, httpserver.WithIdentityService(identitySvc))
		logger.Printf(
			"identity ready installation=%s node=%s role=%s generation=%d",
			identityResult.InstallationID,
			identityResult.NodeID,
			identityResult.Role,
			identityResult.LeadershipGeneration,
		)
	}

	server := &http.Server{
		Addr:              cfg.Addr,
		Handler:           httpserver.New(cfg, logger, options...).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	var supervisor *enrollment.EnrollmentTLSSupervisor
	if configStore != nil && identitySvc != nil {
		var err error
		supervisor, err = buildEnrollmentTLSSupervisor(cfg, identitySvc, configStore, logger)
		if err != nil {
			logEnrollmentTLSFailure(logger, "supervisor unavailable")
			supervisor = nil
		}
	}

	workers := make([]func(context.Context), 0, 2)
	if configStore != nil {
		workers = append(workers, func(workerCtx context.Context) {
			// Ordinary renewal is gated by the live identity on every cycle and
			// work item. Enrollment TLS material remains supervisor-owned and is
			// refreshed atomically by the supervisor above.
			configuration.RunRenewalLoop(workerCtx, configStore, configuration.RenewalOptions{
				StagingDirectoryURL:    cfg.ACMEDirectoryURL,
				ProductionDirectoryURL: cfg.ACMEProductionDirectoryURL,
				Email:                  cfg.AcmeEmail,
				Log:                    logger,
				Identity:               renewalIdentityProvider(identitySvc),
				IdentityLease:          identitySvc,
			}, cfg.CertRenewalInterval)
		})
	}
	// Start the standalone-archive retention worker. Phase 1 introduces
	// the worker and the ArchiveStore seam; the actual ArchiveStore
	// implementation that reads from PostgreSQL is added by the same
	// change that wires enrollment in Phase 2. Until then, the worker
	// runs with a no-op archive store and is a no-op itself.
	workers = append(workers, func(workerCtx context.Context) {
		snapshot.NewRetentionWorker(snapshot.NoopArchiveStore{}, time.Hour, logger).Run(workerCtx)
	})

	logger.Printf("proxycore-api listening on %s (ui=%s)", cfg.Addr, cfg.UIDist)
	return runServerRuntime(ctx, logger, serverRuntimeOptions{
		ordinary:   server,
		enrollment: supervisor,
		workers:    workers,
	})
}

type serverRuntimeResult struct {
	kind string
	err  error
}

const (
	serverRuntimeHTTPResult         = "ordinary-http"
	serverRuntimeEnrollmentResult   = "enrollment-tls"
	serverRuntimeStatusResult       = "enrollment-tls-status"
	defaultEnrollmentStatusInterval = time.Second
)

func runServerRuntime(ctx context.Context, logger *log.Logger, opts serverRuntimeOptions) error {
	if ctx == nil {
		return errEnrollmentTLSRuntimeUnavailable
	}
	if opts.ordinary == nil {
		return errors.New("ordinary HTTP server is required")
	}
	if logger == nil {
		logger = log.Default()
	}
	childCtx, rawCancel := context.WithCancel(ctx)
	cancel := newProcessRuntimeCancel(rawCancel, opts.cancelObserved)
	defer cancel()
	results := make(chan serverRuntimeResult, 3)
	var workers sync.WaitGroup
	for _, worker := range opts.workers {
		if worker == nil {
			continue
		}
		workers.Add(1)
		go func(run func(context.Context)) {
			defer workers.Done()
			run(childCtx)
		}(worker)
	}
	workersDone := make(chan struct{})
	go func() {
		workers.Wait()
		close(workersDone)
	}()

	go func() {
		results <- serverRuntimeResult{kind: serverRuntimeHTTPResult, err: opts.ordinary.ListenAndServe()}
	}()
	remaining := 1

	var statusCancel context.CancelFunc
	if opts.enrollment != nil {
		go func() {
			results <- serverRuntimeResult{kind: serverRuntimeEnrollmentResult, err: opts.enrollment.Run(childCtx)}
		}()
		remaining++
		statusCtx, cancelStatus := context.WithCancel(childCtx)
		statusCancel = cancelStatus
		interval := opts.enrollmentStatusInterval
		if interval <= 0 {
			interval = defaultEnrollmentStatusInterval
		}
		go func() {
			watchEnrollmentTLSStatus(statusCtx, opts.enrollment, logger, interval)
			results <- serverRuntimeResult{kind: serverRuntimeStatusResult}
		}()
		remaining++
	}
	if statusCancel == nil {
		statusCancel = func() {}
	}

	var firstErr error
	var shutdownErr error
	shutdownStarted := false
	ctxDone := (<-chan struct{})(ctx.Done())
	shutdown := func() {
		if shutdownStarted {
			return
		}
		shutdownStarted = true
		shutdownErr = shutdownProcessRuntime(cancel, opts.ordinary, workersDone, processRuntimeShutdownOptions{
			Timeout:     opts.shutdownTimeout,
			WithTimeout: opts.shutdownWithTimeout,
		})
		if shutdownErr != nil {
			if firstErr == nil {
				firstErr = shutdownErr
			}
			logger.Printf("process shutdown: %s", redactedProcessRuntimeError(shutdownErr))
		}
	}

	for remaining > 0 {
		select {
		case <-ctxDone:
			shutdown()
			if shutdownErr != nil {
				return firstErr
			}
			ctxDone = nil
		case result := <-results:
			remaining--
			switch result.kind {
			case serverRuntimeHTTPResult:
				if !isNormalRuntimeClose(result.err) {
					if firstErr == nil {
						firstErr = errProcessRuntimeHTTPServeFailed
					}
					logger.Printf("ordinary HTTP server stopped: %s", redactedProcessRuntimeError(errProcessRuntimeHTTPServeFailed))
				}
				if !shutdownStarted {
					shutdown()
					if shutdownErr != nil {
						return firstErr
					}
				}
			case serverRuntimeEnrollmentResult:
				statusCancel()
				if result.err != nil && !isNormalRuntimeClose(result.err) {
					logger.Printf("enrollment TLS supervisor stopped: %s", redactedEnrollmentTLSError(result.err))
					if shutdownStarted && firstErr == nil {
						firstErr = errEnrollmentTLSRuntimeUnavailable
					}
				}
			case serverRuntimeStatusResult:
			}
		}
	}
	return firstErr
}

func watchEnrollmentTLSStatus(ctx context.Context, supervisor enrollmentSupervisorRunner, logger *log.Logger, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var previous enrollmentTLSStatusObservation
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			status := supervisor.Status()
			observation := enrollmentTLSStatusObservation{
				state:               status.State,
				roleEligible:        status.RoleEligible,
				configured:          status.Configured,
				materialReady:       status.MaterialReady,
				lastError:           status.LastError,
				consecutiveFailures: status.ConsecutiveFailures,
			}
			if observation == previous {
				continue
			}
			previous = observation
			if status.State == enrollment.EnrollmentTLSStateIneligible {
				switch {
				case !status.RoleEligible:
					logEnrollmentTLSFailure(logger, "ineligible identity")
				case !status.Configured:
					logEnrollmentTLSFailure(logger, "hostnames are not configured")
				case !status.MaterialReady:
					logEnrollmentTLSFailure(logger, "TLS material unavailable")
				}
			}
			if status.LastError != "" {
				logger.Printf("enrollment TLS degraded: %s", redactedEnrollmentTLSError(errors.New(status.LastError)))
			}
		}
	}
}

type enrollmentTLSStatusObservation struct {
	state               enrollment.EnrollmentTLSState
	roleEligible        bool
	configured          bool
	materialReady       bool
	lastError           string
	consecutiveFailures int
}

func redactedEnrollmentTLSError(err error) string {
	switch {
	case err == nil:
		return "none"
	case errors.Is(err, enrollment.ErrEnrollmentTLSIdentityUnavailable):
		return "identity unavailable"
	case errors.Is(err, enrollment.ErrEnrollmentTLSConfigurationUnavailable):
		return "configuration unavailable"
	case errors.Is(err, enrollment.ErrEnrollmentTLSMaterialUnavailable):
		return "TLS material unavailable"
	case errors.Is(err, enrollment.ErrEnrollmentTLSBind):
		return "listener bind unavailable"
	case errors.Is(err, enrollment.ErrEnrollmentTLSServe):
		return "listener stopped unexpectedly"
	case errors.Is(err, enrollment.ErrEnrollmentTLSShutdown):
		return "listener shutdown unavailable"
	default:
		return "unavailable"
	}
}
