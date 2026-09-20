package httpexport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zerkc/ProxyCore/apps/api/internal/backupcore/dbimport"
	"github.com/zerkc/ProxyCore/apps/api/internal/backupcore/hostread"
	"github.com/zerkc/ProxyCore/apps/api/internal/backupcore/zipextract"
	"github.com/zerkc/ProxyCore/apps/api/internal/httpserver"
)

// BackupImporter adapts the HTTP upload boundary to the backup import engine.
type BackupImporter struct {
	Pool            *pgxpool.Pool
	MasterKeyBase64 string
	EnvRestorePath  string
	CandidateRoot   string
	EnvMode         string
	Apply           dbimport.ApplyTrigger
	Audit           dbimport.AuditEmitter
	ActorID         string
	NewRevisionID   string
	Now             func() time.Time
	BodyLimit       int64
}

var _ httpserver.BackupImporter = (*BackupImporter)(nil)

// Import opens, validates, and restores one uploaded backup bundle.
func (i *BackupImporter) Import(ctx context.Context, body io.Reader, size int64, passphrase []byte, dryRun bool) (httpserver.ImportReport, error) {
	if i == nil {
		return httpserver.ImportReport{}, errors.New("httpexport: nil importer")
	}
	return i.importWithActorID(ctx, body, size, passphrase, dryRun, i.ActorID)
}

// ImportWithActorID is the request-scoped variant used by the HTTP handler.
// The struct's ActorID remains the fallback for direct callers and background
// composition, while authenticated requests always pass their current user ID.
func (i *BackupImporter) ImportWithActorID(ctx context.Context, body io.Reader, size int64, passphrase []byte, dryRun bool, actorID string) (httpserver.ImportReport, error) {
	if i == nil {
		return httpserver.ImportReport{}, errors.New("httpexport: nil importer")
	}
	return i.importWithActorID(ctx, body, size, passphrase, dryRun, actorID)
}

func (i *BackupImporter) importWithActorID(ctx context.Context, body io.Reader, size int64, passphrase []byte, dryRun bool, actorID string) (httpserver.ImportReport, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if i.BodyLimit > 0 && size > i.BodyLimit {
		return httpserver.ImportReport{}, ErrBodyTooLarge
	}
	if body == nil {
		return httpserver.ImportReport{}, zipextract.ErrCorruptArchive
	}

	limited := body
	var counted *countingReader
	if i.BodyLimit > 0 {
		counted = &countingReader{Reader: io.LimitReader(body, i.BodyLimit+1)}
		limited = counted
	}
	archive, err := zipextract.Open(ctx, limited, passphrase)
	if counted != nil && counted.count > i.BodyLimit {
		if archive != nil {
			_ = archive.Close()
		}
		return httpserver.ImportReport{}, ErrBodyTooLarge
	}
	if err != nil {
		return httpserver.ImportReport{}, err
	}

	now := i.Now
	if now == nil {
		now = time.Now
	}
	hostConfig := hostread.Config{EnvPath: i.EnvRestorePath, CandidateRoot: i.CandidateRoot}
	hostConfig.ApplyDefaults(nil)
	envMode := i.EnvMode
	if envMode == "" {
		envMode = "0600"
	}
	engine, err := dbimport.New(dbimport.Options{
		Pool:                    i.Pool,
		MasterKeyBase64:         i.MasterKeyBase64,
		EnvRestorePath:          hostConfig.EnvPath,
		CandidateRoot:           hostConfig.CandidateRoot,
		NewRevisionID:           i.NewRevisionID,
		Now:                     now,
		Apply:                   i.Apply,
		Audit:                   i.Audit,
		AuditActorID:            actorID,
		EnvMode:                 envMode,
		ApplyDoesNotMarkApplied: true,
	})
	if err != nil {
		_ = archive.Close()
		return httpserver.ImportReport{}, fmt.Errorf("create backup import engine: %w", err)
	}
	engineContext := ctx
	if actorID != "" {
		engineContext = dbimport.WithActorID(ctx, actorID)
	}
	return engine.Import(engineContext, archive, passphrase, dryRun)
}

type countingReader struct {
	io.Reader
	count int64
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.count += int64(n)
	return n, err
}
