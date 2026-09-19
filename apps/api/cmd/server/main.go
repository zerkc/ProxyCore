package main

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/zerkc/ProxyCore/apps/api/internal/auth"
	"github.com/zerkc/ProxyCore/apps/api/internal/cluster"
	"github.com/zerkc/ProxyCore/apps/api/internal/config"
	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/enrollment"
	"github.com/zerkc/ProxyCore/apps/api/internal/httpserver"
	"github.com/zerkc/ProxyCore/apps/api/internal/identity"
	"github.com/zerkc/ProxyCore/apps/api/internal/snapshot"
	syncpublication "github.com/zerkc/ProxyCore/apps/api/internal/sync"
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
	if err := runServerWithNodeConverter(ctx, cfg, logger, nodeConverterEnabled(os.Args[1:])); err != nil {
		logger.Fatalf("server: %v", err)
	}
}

// runServer preserves the default-safe boot path for callers and tests. NODE
// conversion is opt-in through the explicit command-line flag only.
func runServer(ctx context.Context, cfg config.Config, logger *log.Logger) error {
	return runServerWithNodeConverter(ctx, cfg, logger, false)
}

func runServerWithNodeConverter(ctx context.Context, cfg config.Config, logger *log.Logger, enableNodeConverter bool) error {
	if ctx == nil {
		return errors.New("process context is required")
	}
	if logger == nil {
		logger = log.Default()
	}

	var pool *pgxpool.Pool
	var phase2Store *configuration.PgPhase2Store
	var configStore *configuration.Store
	var identitySvc *identity.Service
	var identityResult identity.Identity
	var nodeConverter *enrollment.NodeConverter
	var tokenAuthority httpserver.EnrollmentTokenAuthority
	var snapshotClient *syncpublication.SnapshotClient
	var localKEKLoader *cluster.LocalKEKLoader
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
		phase2Store = configuration.NewPhase2StoreWithMasterKey(pool, cfg.MasterKeyBase64)
		snapshotClient = syncpublication.NewSnapshotClient(syncpublication.SnapshotClientOptions{
			CanonicalizeURL: enrollment.CanonicalizePrimaryURL,
		})
		localKEKLoader = cluster.NewLocalKEKLoader(cluster.LocalKEKLoaderOptions{
			Pool: pool, Store: cluster.NewStore(cfg.MasterKeyBase64), Identity: identitySvc,
		})
		tokenAuthority = httpserver.NewEnrollmentTokenAuthority(phase2Store, identitySvc, httpserver.EnrollmentTokenAuthorityOptions{
			List: func(ctx context.Context, ownerID string) ([]httpserver.EnrollmentTokenRecord, error) {
				return listEnrollmentTokens(ctx, pool, ownerID)
			},
		})
		options = append(options, httpserver.WithEnrollmentTokenAuthority(tokenAuthority))
		nodeConverter = buildNodeConverter(enableNodeConverter, configStore, identitySvc, logger)
		if nodeConverter != nil {
			logger.Printf("node converter ready for role=%s", identityResult.Role)
		}
	}

	server := &http.Server{
		Addr:              cfg.Addr,
		Handler:           httpserver.New(cfg, logger, options...).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	draftCache := enrollment.NewDraftCache(enrollment.DraftCacheOptions{})
	var supervisor *enrollment.EnrollmentTLSSupervisor
	if configStore != nil && identitySvc != nil {
		workflow := httpserver.EnrollmentWorkflowHandlerOptions{
			Cache:     draftCache,
			Converter: nodeConverter,
			Identity:  localEnrollmentIdentityProvider(identitySvc),
			LocalIngress: func(context.Context) (domain.Ingress, error) {
				return domain.Ingress{IPv4: cfg.ProxyIngressIPv4, IPv6: cfg.ProxyIngressIPv6}, nil
			},
		}
		if tokenAuthority != nil {
			workflow.VerifyToken = tokenAuthority.Verify
		}
		wireEnrollmentWorkflowDependencies(&workflow, snapshotClient, localKEKLoader)
		snapshotByTokenKeyLoader := &snapshotByTokenClusterKeyLoader{pool: pool, store: cluster.NewStore(cfg.MasterKeyBase64), identity: identitySvc}
		snapshotByTokenOptions := httpserver.EnrollmentSnapshotByTokenHandlerOptions{
			Store:            productionSnapshotByTokenStore(phase2Store),
			Identity:         snapshotByTokenIdentitySource{service: identitySvc},
			ParseToken:       httpserver.ParseHTTPEnrollmentToken,
			VerifyToken:      workflow.VerifyToken,
			ClusterKeyLoader: snapshotByTokenKeyLoader.Load,
			Now:              time.Now,
		}
		var err error
		publicationOptions := httpserver.EnrollmentSnapshotPublicationHandlerOptions{
			Store: phase2Store, Identity: identitySvc,
		}
		supervisor, err = newEnrollmentWorkflowTLSSupervisorWithHandlers(cfg, identitySvc, configStore, workflow, logger, &publicationOptions, &snapshotByTokenOptions)
		if err != nil {
			logEnrollmentTLSFailure(logger, "supervisor unavailable")
			supervisor = nil
		}
	}

	workers := make([]func(context.Context), 0, 3)
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
		publicationProducer := syncpublication.NewCanonicalSnapshotProducer(phase2Store, syncpublication.CanonicalSnapshotProducerOptions{
			OnError: func(error) {
				logger.Printf("canonical snapshot publication reconciliation unavailable")
			},
		})
		workers = append(workers, func(workerCtx context.Context) {
			publicationProducer.Run(workerCtx)
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

func wireEnrollmentWorkflowDependencies(workflow *httpserver.EnrollmentWorkflowHandlerOptions, client *syncpublication.SnapshotClient, loader *cluster.LocalKEKLoader) {
	if workflow == nil {
		return
	}
	if client != nil {
		workflow.FetchSnapshot = func(ctx context.Context, primaryURL, credential string) (*snapshot.Envelope, error) {
			var body []byte
			var err error
			_, _, isEnrollmentToken := httpserver.ParseHTTPEnrollmentToken(credential)
			if isEnrollmentToken && strings.HasPrefix(credential, "pcenr1_") {
				body, err = client.FetchSnapshotByToken(ctx, primaryURL, credential)
			} else {
				body, err = client.FetchSnapshot(ctx, primaryURL, []byte(credential))
			}
			if err != nil {
				return nil, err
			}
			envelope, err := snapshot.Unmarshal(body)
			if err != nil {
				return nil, syncpublication.ErrSnapshotClientInvalid
			}
			return &envelope, nil
		}
	}
	if loader != nil {
		workflow.Validator = httpserver.NewLocalKEKSnapshotValidator(loader)
	}
}

func nodeConverterEnabled(args []string) bool {
	for _, arg := range args {
		if arg == "--enable-node-converter" || arg == "--enable-node-converter=true" {
			return true
		}
	}
	return false
}

func buildNodeConverter(enabled bool, configStore *configuration.Store, identitySvc *identity.Service, logger *log.Logger) *enrollment.NodeConverter {
	if !enabled || configStore == nil || identitySvc == nil || !nodeConverterRoleEligible(identitySvc) {
		return nil
	}
	logf := func(format string, args ...any) {}
	if logger != nil {
		logf = logger.Printf
	}
	return enrollment.NewNodeConverter(enrollment.NodeConverterOptions{
		Importer:      snapshot.NewImporter(configStore, configStore, time.Now),
		Archive:       configStore,
		Identity:      identitySvc,
		ApplyWaiter:   configStore,
		ApplyEnqueuer: configStore,
		Logger:        logf,
		Now:           time.Now,
	})
}

func nodeConverterRoleEligible(service *identity.Service) (eligible bool) {
	defer func() {
		if recover() != nil {
			eligible = false
		}
	}()
	current := service.Current()
	return current.Role == domain.TopologyRoleStandalone || current.Role == domain.TopologyRolePrimary
}

func localEnrollmentIdentityProvider(service *identity.Service) func(context.Context) (httpserver.EnrollmentLocalIdentity, error) {
	return func(ctx context.Context) (local httpserver.EnrollmentLocalIdentity, err error) {
		if service == nil || ctx == nil {
			return httpserver.EnrollmentLocalIdentity{}, httpserver.ErrEnrollmentWorkflowUnavailable
		}
		if err := ctx.Err(); err != nil {
			return httpserver.EnrollmentLocalIdentity{}, err
		}
		defer func() {
			if recover() != nil {
				local, err = httpserver.EnrollmentLocalIdentity{}, httpserver.ErrEnrollmentWorkflowUnavailable
			}
		}()
		current := service.Current()
		local = httpserver.EnrollmentLocalIdentity{
			InstallationID:       current.InstallationID,
			NodeID:               current.NodeID,
			Role:                 current.Role,
			LeadershipGeneration: current.LeadershipGeneration,
		}
		if current.ClusterKeyID != nil {
			local.ClusterKeyID = current.ClusterKeyID.String()
		}
		return local, nil
	}
}

type snapshotByTokenIdentitySource struct {
	service *identity.Service
}

func (s snapshotByTokenIdentitySource) Current() (current identity.Identity, loaded bool) {
	if s.service == nil {
		return identity.Identity{}, false
	}
	defer func() {
		if recover() != nil {
			current, loaded = identity.Identity{}, false
		}
	}()
	return s.service.Current(), true
}

type snapshotByTokenStoreReader interface {
	ReadSnapshotPublicationByToken(context.Context, string, configuration.SnapshotPublicationIdentityRecord) ([]byte, error)
}

type snapshotByTokenStoreAdapter struct {
	store snapshotByTokenStoreReader
}

func (s snapshotByTokenStoreAdapter) ReadSnapshotPublicationByToken(ctx context.Context, selector string, current configuration.SnapshotPublicationIdentityRecord) ([]byte, error) {
	if s.store == nil {
		return nil, httpserver.ErrSnapshotByTokenUnavailable
	}
	body, err := s.store.ReadSnapshotPublicationByToken(ctx, selector, current)
	switch {
	case errors.Is(err, configuration.ErrNoPublishableSnapshot):
		return nil, httpserver.ErrNoPublishableSnapshot
	case errors.Is(err, configuration.ErrSnapshotByTokenUnauthenticated):
		return nil, httpserver.ErrSnapshotByTokenUnauthenticated
	case errors.Is(err, configuration.ErrSnapshotByTokenGone):
		return nil, httpserver.ErrSnapshotByTokenGone
	case errors.Is(err, configuration.ErrSnapshotByTokenDenied):
		return nil, httpserver.ErrSnapshotByTokenDenied
	case errors.Is(err, configuration.ErrSnapshotByTokenUnavailable):
		return nil, httpserver.ErrSnapshotByTokenUnavailable
	case err != nil:
		return nil, httpserver.ErrSnapshotByTokenUnavailable
	default:
		return body, nil
	}
}

func productionSnapshotByTokenStore(store *configuration.PgPhase2Store) httpserver.SnapshotByTokenStore {
	if store == nil {
		return nil
	}
	return snapshotByTokenStoreAdapter{store: store}
}

type snapshotByTokenClusterKeyLoader struct {
	pool     *pgxpool.Pool
	store    *cluster.Store
	identity *identity.Service
}

func (l *snapshotByTokenClusterKeyLoader) Load(ctx context.Context) (httpserver.SnapshotByTokenKeyMaterial, error) {
	if l == nil || ctx == nil || l.pool == nil || l.store == nil || l.identity == nil {
		return nil, httpserver.ErrSnapshotByTokenUnavailable
	}
	current, loaded := (snapshotByTokenIdentitySource{service: l.identity}).Current()
	if !loaded || current.ClusterKeyID == nil ||
		(current.Role != domain.TopologyRolePrimary && current.Role != domain.TopologyRolePrimaryWithNodes) {
		return nil, httpserver.ErrSnapshotByTokenUnavailable
	}
	tx, err := l.pool.Begin(ctx)
	if err != nil {
		return nil, httpserver.ErrSnapshotByTokenUnavailable
	}
	defer tx.Rollback(ctx)
	material, err := l.store.LoadOrCreate(ctx, tx, current.ClusterKeyID)
	keyBytes := material.CopyBytes()
	keyUsable := len(keyBytes) > 0
	for index := range keyBytes {
		keyBytes[index] = 0
	}
	if err != nil || material.ID != *current.ClusterKeyID || !keyUsable {
		material.Destroy()
		return nil, httpserver.ErrSnapshotByTokenUnavailable
	}
	if err := tx.Commit(ctx); err != nil {
		material.Destroy()
		return nil, httpserver.ErrSnapshotByTokenUnavailable
	}
	return &material, nil
}

type enrollmentWorkflowTLSListenerFactory struct {
	identity        enrollment.EnrollmentTLSIdentityCurrent
	workflow        httpserver.EnrollmentWorkflowHandlerOptions
	logger          *log.Logger
	listen          func(string, string) (net.Listener, error)
	newServer       func(net.Listener, http.Handler, *enrollment.TLSCertificateProvider) (enrollment.EnrollmentTLSListener, error)
	now             func() time.Time
	publication     *httpserver.EnrollmentSnapshotPublicationHandlerOptions
	snapshotByToken *httpserver.EnrollmentSnapshotByTokenHandlerOptions
}

func newEnrollmentWorkflowTLSListenerFactory(identityCurrent enrollment.EnrollmentTLSIdentityCurrent, workflow httpserver.EnrollmentWorkflowHandlerOptions, logger *log.Logger) *enrollmentWorkflowTLSListenerFactory {
	return &enrollmentWorkflowTLSListenerFactory{
		identity: identityCurrent,
		workflow: workflow,
		logger:   logger,
		listen:   net.Listen,
		now:      time.Now,
		newServer: func(listener net.Listener, handler http.Handler, provider *enrollment.TLSCertificateProvider) (enrollment.EnrollmentTLSListener, error) {
			return enrollment.NewEnrollmentTLSServer(listener, handler, provider)
		},
	}
}

func (f *enrollmentWorkflowTLSListenerFactory) Listen(ctx context.Context, address string, provider *enrollment.TLSCertificateProvider) (enrollment.EnrollmentTLSListener, error) {
	if f == nil || f.identity == nil || f.listen == nil || f.newServer == nil || provider == nil || strings.TrimSpace(address) == "" {
		return nil, enrollment.ErrEnrollmentTLSBind
	}
	if ctx == nil {
		return nil, errEnrollmentTLSRuntimeUnavailable
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	listener, err := f.listen("tcp", address)
	if err != nil {
		logEnrollmentTLSFailure(f.logger, "listener bind failed")
		return nil, enrollment.ErrEnrollmentTLSBind
	}
	if err := ctx.Err(); err != nil {
		_ = listener.Close()
		return nil, err
	}
	now := f.now
	if now == nil {
		now = time.Now
	}
	signer, err := enrollment.NewIdentityProofSigner(enrollment.IdentityProofSignerOptions{
		Identity: f.identity.Current,
		Material: provider.Material,
		Now:      now,
	})
	if err != nil {
		_ = listener.Close()
		logEnrollmentTLSFailure(f.logger, "identity proof signer unavailable")
		return nil, enrollment.ErrEnrollmentTLSBind
	}
	certificate := func(ctx context.Context) (*x509.Certificate, error) {
		return provider.Certificate(ctx)
	}
	handler := httpserver.NewEnrollmentIdentityMux(httpserver.EnrollmentIdentityHandlerOptions{
		Signer: signer, Certificate: certificate, Workflow: &f.workflow,
	})
	if f.publication != nil {
		publicationOptions := *f.publication
		publicationOptions.Certificate = certificate
		publication := httpserver.NewEnrollmentSnapshotPublicationHandler(publicationOptions)
		handler = httpserver.NewEnrollmentSnapshotPublicationMux(publication, handler)
	}
	if f.snapshotByToken != nil {
		snapshotByTokenOptions := *f.snapshotByToken
		snapshotByTokenOptions.Certificate = certificate
		snapshotByToken := httpserver.NewEnrollmentSnapshotByTokenHandler(snapshotByTokenOptions)
		handler = httpserver.NewEnrollmentSnapshotByTokenMux(snapshotByToken, handler)
	}
	server, err := f.newServer(listener, handler, provider)
	if err != nil || server == nil {
		_ = listener.Close()
		logEnrollmentTLSFailure(f.logger, "TLS server construction failed")
		return nil, enrollment.ErrEnrollmentTLSBind
	}
	return server, nil
}

func newEnrollmentWorkflowTLSSupervisor(cfg config.Config, identityService *identity.Service, configStore *configuration.Store, workflow httpserver.EnrollmentWorkflowHandlerOptions, logger *log.Logger, publication ...httpserver.EnrollmentSnapshotPublicationHandlerOptions) (*enrollment.EnrollmentTLSSupervisor, error) {
	var publicationOptions *httpserver.EnrollmentSnapshotPublicationHandlerOptions
	if len(publication) > 0 {
		copy := publication[0]
		publicationOptions = &copy
	}
	return newEnrollmentWorkflowTLSSupervisorWithHandlers(cfg, identityService, configStore, workflow, logger, publicationOptions, nil)
}

func newEnrollmentWorkflowTLSSupervisorWithHandlers(cfg config.Config, identityService *identity.Service, configStore *configuration.Store, workflow httpserver.EnrollmentWorkflowHandlerOptions, logger *log.Logger, publication *httpserver.EnrollmentSnapshotPublicationHandlerOptions, snapshotByToken *httpserver.EnrollmentSnapshotByTokenHandlerOptions) (*enrollment.EnrollmentTLSSupervisor, error) {
	identityCurrent := &enrollmentTLSIdentityAdapter{service: identityService, logger: logger}
	configurationStore := &enrollmentTLSConfigurationAdapter{store: configStore, logger: logger}
	factory := newEnrollmentWorkflowTLSListenerFactory(identityCurrent, workflow, logger)
	factory.publication = publication
	factory.snapshotByToken = snapshotByToken
	return enrollment.NewEnrollmentTLSSupervisor(enrollment.EnrollmentTLSSupervisorOptions{
		Identity:        identityCurrent,
		Configuration:   configurationStore,
		ListenerFactory: factory,
		Address:         cfg.EnrollmentTLSAddr,
	})
}

func listEnrollmentTokens(ctx context.Context, pool *pgxpool.Pool, ownerID string) ([]httpserver.EnrollmentTokenRecord, error) {
	if pool == nil || ctx == nil || strings.TrimSpace(ownerID) == "" {
		return nil, errors.New("enrollment token list unavailable")
	}
	rows, err := pool.Query(ctx, `
		select t.id::text, t.token_selector, t.created_at, t.expires_at,
			t.consumed_at, t.revoked_at, count(g.attempt_id), max(g.created_at)
		from enrollment_tokens t
		left join enrollment_grants g on g.token_id = t.id
		where t.created_by_user_id = $1
		group by t.id, t.token_selector, t.created_at, t.expires_at, t.consumed_at, t.revoked_at
		order by t.created_at desc, t.id desc
	`, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]httpserver.EnrollmentTokenRecord, 0)
	for rows.Next() {
		var record httpserver.EnrollmentTokenRecord
		var attempts int64
		if err := rows.Scan(&record.ID, &record.Selector, &record.CreatedAt, &record.ExpiresAt, &record.ConsumedAt, &record.RevokedAt, &attempts, &record.LastAttemptAt); err != nil {
			return nil, err
		}
		if attempts > int64(^uint(0)>>1) {
			return nil, errors.New("enrollment token attempt count overflow")
		}
		record.AttemptCount = int(attempts)
		result = append(result, record)
	}
	return result, rows.Err()
}
