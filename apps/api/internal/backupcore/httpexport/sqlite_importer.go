package httpexport

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/backupcore/dbimport"
	"github.com/zerkc/ProxyCore/apps/api/internal/httpserver"
	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

// SQLiteBackupImporter restores an uploaded backup bundle into a SQLite
// configuration database. Validation, master-key verification, file restore
// and the apply trigger are shared through importBundle; only the table replace
// differs: one BEGIN IMMEDIATE transaction deletes the 16 bundle tables and
// every table that depends on them (all sessions included, so everyone is
// logged out) and reinserts the bundle.
type SQLiteBackupImporter struct {
	DB *sql.DB
	// Writer is optional; it defaults to a writer on DB.
	Writer          *sqlitedb.Writer
	MasterKeyBase64 string
	EnvRestorePath  string
	CandidateRoot   string
	EnvMode         string
	// Apply runs after a successful restore; use NewApplyTrigger with the
	// SQLite configuration backend.
	Apply dbimport.ApplyTrigger
	// Audit is typically a *dbimport.SQLiteAuditEmitter on the same database.
	Audit         dbimport.AuditEmitter
	ActorID       string
	NewRevisionID string
	Now           func() time.Time
	BodyLimit     int64
}

var _ httpserver.BackupImporter = (*SQLiteBackupImporter)(nil)

// Import opens, validates, and restores one uploaded backup bundle.
func (i *SQLiteBackupImporter) Import(ctx context.Context, body io.Reader, size int64, passphrase []byte, dryRun bool) (httpserver.ImportReport, error) {
	if i == nil {
		return httpserver.ImportReport{}, errors.New("httpexport: nil importer")
	}
	return i.ImportWithActorID(ctx, body, size, passphrase, dryRun, i.ActorID)
}

// ImportWithActorID is the request-scoped variant used by the HTTP handler.
func (i *SQLiteBackupImporter) ImportWithActorID(ctx context.Context, body io.Reader, size int64, passphrase []byte, dryRun bool, actorID string) (httpserver.ImportReport, error) {
	if i == nil {
		return httpserver.ImportReport{}, errors.New("httpexport: nil importer")
	}
	if i.DB == nil {
		return httpserver.ImportReport{}, errors.New("httpexport: nil sqlite database")
	}
	var apply dbimport.ApplyTrigger
	if i.Apply != nil {
		apply = &survivingActorApply{db: i.DB, inner: i.Apply}
	}
	return importBundle(ctx, body, size, passphrase, dryRun, actorID, bundleImport{
		Tables:          dbimport.NewSQLiteTableImporter(i.DB, i.Writer),
		MasterKeyBase64: i.MasterKeyBase64,
		EnvRestorePath:  i.EnvRestorePath,
		CandidateRoot:   i.CandidateRoot,
		EnvMode:         i.EnvMode,
		Apply:           apply,
		Audit:           i.Audit,
		NewRevisionID:   i.NewRevisionID,
		Now:             i.Now,
		BodyLimit:       i.BodyLimit,
	})
}

// survivingActorApply drops an actor that the replaced users table no longer
// contains, so the apply job is created without a dangling foreign key.
type survivingActorApply struct {
	db    *sql.DB
	inner dbimport.ApplyTrigger
}

func (a *survivingActorApply) Trigger(ctx context.Context, actorID string) error {
	if actorID != "" {
		var found string
		err := a.db.QueryRowContext(ctx, `select id from users where id = ?`, strings.ToLower(actorID)).Scan(&found)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			actorID = ""
		case err != nil:
			return err
		}
	}
	return a.inner.Trigger(ctx, actorID)
}
