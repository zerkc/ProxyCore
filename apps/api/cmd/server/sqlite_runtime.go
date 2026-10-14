package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/auth"
	"github.com/zerkc/ProxyCore/apps/api/internal/backupcore/dbimport"
	"github.com/zerkc/ProxyCore/apps/api/internal/backupcore/httpexport"
	"github.com/zerkc/ProxyCore/apps/api/internal/cluster"
	"github.com/zerkc/ProxyCore/apps/api/internal/config"
	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/httpserver"
	"github.com/zerkc/ProxyCore/apps/api/internal/identity"
	"github.com/zerkc/ProxyCore/apps/api/internal/secrets"
	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
	"github.com/zerkc/ProxyCore/apps/api/internal/version"
)

// This file is the SQLite composition root. It is deliberately separate from
// main.go so it stays independent of any other persistence backend.

// errSQLitePathRequired is returned when PROXYCORE_SQLITE_PATH is unset: SQLite
// is the only supported backend, so there is no store-less boot.
var errSQLitePathRequired = errors.New("PROXYCORE_SQLITE_PATH is required: SQLite is the only supported backend")

var (
	_ enrollmentTLSConfigurationStore = (*configuration.SQLiteConfigurationBackend)(nil)
	_ configuration.RenewalStore      = (*configuration.SQLiteConfigurationBackend)(nil)
	_ httpserver.ConfigurationBackend = (*configuration.SQLiteConfigurationBackend)(nil)
)

// sqliteStack owns one database, one writer and every SQLite store built on
// them. The secrets store is built once; it creates its own writer internally
// (secrets.NewSQLiteStore takes no writer), and BEGIN IMMEDIATE plus the
// 5-second busy timeout serialize it against the shared one.
type sqliteStack struct {
	db             *sql.DB
	writer         *sqlitedb.Writer
	keys           *cluster.SQLiteKeyStore
	config         *configuration.SQLiteConfigurationBackend
	phase2         *configuration.SQLitePhase2Store
	authStore      auth.Store
	identity       *identity.Service
	identityResult identity.Identity
	tokenAuthority httpserver.EnrollmentTokenAuthority
	localKEKLoader *cluster.LocalKEKLoader
}

func openSQLiteStack(ctx context.Context, cfg config.Config, logger *log.Logger) (stack *sqliteStack, err error) {
	if ctx == nil {
		return nil, errors.New("process context is required")
	}
	if strings.TrimSpace(cfg.SQLitePath) == "" {
		return nil, errSQLitePathRequired
	}
	if err := os.MkdirAll(filepath.Dir(cfg.SQLitePath), 0o700); err != nil {
		return nil, fmt.Errorf("sqlite directory: %w", err)
	}
	db, err := sqlitedb.Open(cfg.SQLitePath)
	if err != nil {
		return nil, fmt.Errorf("sqlite open: %w", err)
	}
	defer func() {
		if err != nil {
			_ = db.Close()
		}
	}()
	migrateCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	err = sqlitedb.Migrate(migrateCtx, db)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("sqlite migrate: %w", err)
	}

	writer := sqlitedb.NewWriter(db, nil)
	secretStore := secrets.NewSQLiteStore(db, cfg.MasterKeyBase64)
	keys := cluster.NewSQLiteKeyStore(db, writer, cfg.MasterKeyBase64)
	ingress := domain.Ingress{IPv4: cfg.ProxyIngressIPv4, IPv6: cfg.ProxyIngressIPv6}
	stack = &sqliteStack{
		db:        db,
		writer:    writer,
		keys:      keys,
		config:    configuration.NewSQLiteConfigurationBackend(db, writer, ingress, secretStore),
		phase2:    configuration.NewSQLitePhase2Store(db, writer, keys, cfg.MasterKeyBase64),
		authStore: auth.NewSQLiteStore(db, writer),
	}

	identityCtx, identityCancel := context.WithTimeout(ctx, 10*time.Second)
	stack.identity, stack.identityResult, _, err = bootstrapIdentity(identityCtx, identity.NewSQLiteStore(db, writer))
	identityCancel()
	if err != nil {
		return nil, fmt.Errorf("identity bootstrap: %w", err)
	}
	if logger != nil {
		logger.Printf(
			"identity ready installation=%s node=%s role=%s generation=%d",
			stack.identityResult.InstallationID,
			stack.identityResult.NodeID,
			stack.identityResult.Role,
			stack.identityResult.LeadershipGeneration,
		)
	}
	// A PRIMARY must own a cluster KEK before it can publish a snapshot or
	// answer snapshot-by-token; mint it when enrollment is first activated.
	stack.identity.SetClusterKeyProvisioner(func(provisionCtx context.Context) (uuid.UUID, error) {
		material, err := keys.LoadOrCreate(provisionCtx, nil)
		if err != nil {
			return uuid.Nil, err
		}
		defer material.Destroy()
		return material.ID, nil
	})
	stack.localKEKLoader = cluster.NewLocalKEKLoader(cluster.LocalKEKLoaderOptions{Keys: keys, Identity: stack.identity})
	stack.tokenAuthority = httpserver.NewEnrollmentTokenAuthority(stack.phase2, stack.identity, httpserver.EnrollmentTokenAuthorityOptions{
		List: sqliteEnrollmentTokenLister(stack.phase2),
	})
	return stack, nil
}

// options are the HTTP options for the SQLite path. Backup export/import is
// wired separately by backupOptions, and only when a master key is configured;
// without it the backup handlers treat a nil exporter/importer as unavailable.
func (s *sqliteStack) options(cfg config.Config) []httpserver.Option {
	return []httpserver.Option{
		httpserver.WithAuthService(auth.NewService(s.authStore, auth.ServiceOptions{SessionTTL: cfg.SessionTTL})),
		httpserver.WithConfigurationStore(s.config),
		httpserver.WithDefaultIngress(domain.Ingress{IPv4: cfg.ProxyIngressIPv4, IPv6: cfg.ProxyIngressIPv6}),
		httpserver.WithIdentityService(s.identity),
		httpserver.WithEnrollmentTokenAuthority(s.tokenAuthority),
	}
}

func (s *sqliteStack) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

type enrollmentTokenListSource interface {
	ListEnrollmentTokens(ctx context.Context, ownerID string) ([]configuration.EnrollmentTokenListRecord, error)
}

// sqliteEnrollmentTokenLister adapts the configuration list record to the HTTP
// record (httpserver imports configuration, so the store cannot return it).
func sqliteEnrollmentTokenLister(source enrollmentTokenListSource) httpserver.EnrollmentTokenLister {
	return func(ctx context.Context, ownerID string) ([]httpserver.EnrollmentTokenRecord, error) {
		if isTypedNil(source) || ctx == nil || strings.TrimSpace(ownerID) == "" {
			return nil, errors.New("enrollment token list unavailable")
		}
		records, err := source.ListEnrollmentTokens(ctx, ownerID)
		if err != nil {
			return nil, err
		}
		result := make([]httpserver.EnrollmentTokenRecord, 0, len(records))
		for _, record := range records {
			result = append(result, httpserver.EnrollmentTokenRecord(record))
		}
		return result, nil
	}
}

// backupOptions wires the public backup export/import over the stack's
// database. Like the PostgreSQL wiring it replaced, it needs the master key:
// the bundle carries a marker row encrypted with it. Without a key the backup
// handlers keep reporting the service as unavailable.
func (s *sqliteStack) backupOptions(cfg config.Config) []httpserver.Option {
	if cfg.MasterKeyBase64 == "" {
		return nil
	}
	exporter := &httpexport.SQLiteBackupExporter{
		DB:              s.db,
		Identity:        httpexport.NewIdentitySource(s.identity),
		Version:         version.Version,
		Now:             time.Now,
		MasterKeyBase64: cfg.MasterKeyBase64,
	}
	importer := &httpexport.SQLiteBackupImporter{
		DB:              s.db,
		Writer:          s.writer,
		MasterKeyBase64: cfg.MasterKeyBase64,
		EnvMode:         "0600",
		Apply:           httpexport.NewApplyTrigger(s.config),
		Audit:           &dbimport.SQLiteAuditEmitter{DB: s.db, Writer: s.writer},
		Now:             time.Now,
		BodyLimit:       4 << 30,
	}
	return []httpserver.Option{httpserver.WithBackup(exporter, importer)}
}
