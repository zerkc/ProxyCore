package httpexport

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/backupcore/dbexport"
	"github.com/zerkc/ProxyCore/apps/api/internal/backupcore/hostread"
	"github.com/zerkc/ProxyCore/apps/api/internal/httpserver"
)

// SQLiteBackupExporter produces the same HTTP backup bundle as BackupExporter
// from a SQLite configuration database. The database tables are read in one
// read transaction, so the bundle holds a consistent snapshot.
type SQLiteBackupExporter struct {
	DB              *sql.DB
	Identity        IdentitySource
	Version         string
	Now             func() time.Time
	MasterKeyBase64 string
	// HostConfig overrides the host paths; the zero value resolves them from
	// PROXYCORE_HOST_HOME exactly like BackupExporter.
	HostConfig hostread.Config
}

var _ httpserver.BackupExporter = (*SQLiteBackupExporter)(nil)

// Export streams the host and SQLite database entries into an optional
// encrypted ZIP and returns the SHA-256 digest of the canonical manifest.
func (e *SQLiteBackupExporter) Export(ctx context.Context, w io.Writer, passphrase []byte) (string, error) {
	if e == nil {
		return "", errors.New("httpexport: nil exporter")
	}
	if e.DB == nil {
		return "", errors.New("httpexport: nil sqlite database")
	}
	return exportBundle(ctx, w, passphrase, bundleConfig{
		Identity:        e.Identity,
		Version:         e.Version,
		Now:             e.Now,
		MasterKeyBase64: e.MasterKeyBase64,
		Host:            hostread.NewSQLite(e.DB, e.HostConfig),
		ExportDB: func(ctx context.Context, entries dbexport.ZipWriter, now func() time.Time, masterKeyBase64 string) (dbexport.ExportReport, error) {
			return dbexport.NewSQLite(e.DB, dbexport.ExporterOptions{
				Now:             now,
				MasterKeyBase64: masterKeyBase64,
			}).Export(ctx, entries)
		},
	})
}
