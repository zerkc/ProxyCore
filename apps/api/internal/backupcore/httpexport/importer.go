package httpexport

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/backupcore/dbimport"
	"github.com/zerkc/ProxyCore/apps/api/internal/backupcore/hostread"
	"github.com/zerkc/ProxyCore/apps/api/internal/backupcore/zipextract"
	"github.com/zerkc/ProxyCore/apps/api/internal/httpserver"
)

// bundleImport is the database-independent configuration of one backup import.
type bundleImport struct {
	Tables          dbimport.TableImporter
	MasterKeyBase64 string
	EnvRestorePath  string
	CandidateRoot   string
	EnvMode         string
	Apply           dbimport.ApplyTrigger
	Audit           dbimport.AuditEmitter
	NewRevisionID   string
	Now             func() time.Time
	BodyLimit       int64
}

func importBundle(ctx context.Context, body io.Reader, size int64, passphrase []byte, dryRun bool, actorID string, i bundleImport) (httpserver.ImportReport, error) {
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
		Tables:                  i.Tables,
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
