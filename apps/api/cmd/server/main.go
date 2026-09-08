package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/zerkc/ProxyCore/apps/api/internal/auth"
	"github.com/zerkc/ProxyCore/apps/api/internal/config"
	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/httpserver"
	"github.com/zerkc/ProxyCore/apps/api/internal/update"
	"github.com/zerkc/ProxyCore/apps/api/internal/version"
)

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

	var pool *pgxpool.Pool
	var configStore *configuration.Store
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
			logger.Fatalf("database: %v", err)
		}
		defer pool.Close()

		if err := pool.Ping(context.Background()); err != nil {
			logger.Fatalf("database ping: %v", err)
		}
		// Drizzle migrate remains the primary path; EnsureSchema is idempotent and
		// covers additive tables (e.g. internal_ca) if migrate was not re-run yet.
		if err := configuration.EnsureSchema(context.Background(), pool); err != nil {
			logger.Fatalf("configuration schema: %v", err)
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
	}

	server := &http.Server{
		Addr:              cfg.Addr,
		Handler:           httpserver.New(cfg, logger, options...).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if configStore != nil {
		go configuration.RunRenewalLoop(ctx, configStore, configuration.RenewalOptions{
			StagingDirectoryURL:    cfg.ACMEDirectoryURL,
			ProductionDirectoryURL: cfg.ACMEProductionDirectoryURL,
			Email:                  cfg.AcmeEmail,
			Log:                    logger,
		}, cfg.CertRenewalInterval)
	}

	go func() {
		logger.Printf("proxycore-api listening on %s (ui=%s)", cfg.Addr, cfg.UIDist)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Fatalf("listen: %v", err)
		}
	}()

	<-ctx.Done()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Printf("shutdown: %v", err)
	}
}
