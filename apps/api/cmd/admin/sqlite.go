package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/auth"
	"github.com/zerkc/ProxyCore/apps/api/internal/config"
	"github.com/zerkc/ProxyCore/apps/api/internal/identity"
	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

var errSQLitePathRequired = errors.New("PROXYCORE_SQLITE_PATH is required: SQLite is the only supported backend")

// adminRuntime owns the SQLite handle behind the admin subcommands.
type adminRuntime struct {
	db       *sql.DB
	authSvc  *auth.Service
	identity *identity.Service
}

func openAdminRuntime(ctx context.Context, cfg config.Config) (*adminRuntime, error) {
	path := strings.TrimSpace(cfg.SQLitePath)
	if path == "" {
		return nil, errSQLitePathRequired
	}
	db, err := sqlitedb.Open(path)
	if err != nil {
		return nil, fmt.Errorf("sqlite open: %w", err)
	}
	migrateCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	err = sqlitedb.Migrate(migrateCtx, db)
	cancel()
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlite migrate: %w", err)
	}
	writer := sqlitedb.NewWriter(db, nil)
	return &adminRuntime{
		db:       db,
		authSvc:  auth.NewService(auth.NewSQLiteStore(db, writer), auth.ServiceOptions{}),
		identity: identity.NewService(identity.NewSQLiteStore(db, writer)),
	}, nil
}

func (r *adminRuntime) reset(ctx context.Context, username string) (string, error) {
	return r.authSvc.ResetPassword(ctx, username)
}

func (r *adminRuntime) status(ctx context.Context) (identity.Identity, error) {
	return r.identity.Load(ctx)
}

func (r *adminRuntime) Close() error { return r.db.Close() }
