package dbimport

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/backupcore/zipextract"
)

type ApplyTrigger interface {
	Trigger(ctx context.Context, actorID string) error
}

type AuditEmitter interface {
	EmitBackupImport(ctx context.Context, actorID string, bundleSHA256 string, dryRun bool, success bool) error
}

// TableImporter atomically replaces the configuration tables with the decoded
// bundle payloads. It owns the transaction and the single-flight guard, so the
// engine stays independent of the database. A failure must leave the database
// unchanged; an in-flight second import must return ErrImportAlreadyInProgress.
type TableImporter interface {
	ReplaceTables(ctx context.Context, payloads []tablePayload) ([]TablePreview, error)
}

type Options struct {
	Tables                  TableImporter
	MasterKeyBase64         string
	EnvRestorePath          string
	CandidateRoot           string
	NewRevisionID           string
	Now                     func() time.Time
	Apply                   ApplyTrigger
	Audit                   AuditEmitter
	AuditActorID            string
	EnvMode                 string
	ApplyDoesNotMarkApplied bool
}

type Engine struct {
	tables                  TableImporter
	masterKeyBase64         string
	envRestorePath          string
	candidateRoot           string
	revisionID              string
	now                     func() time.Time
	apply                   ApplyTrigger
	audit                   AuditEmitter
	auditActorID            string
	envMode                 os.FileMode
	applyDoesNotMarkApplied bool
}

// actorIDContextKey lets the HTTP composition layer carry the authenticated
// owner ID without widening the import method's archive-facing signature.
type actorIDContextKey struct{}

func WithActorID(ctx context.Context, actorID string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, actorIDContextKey{}, actorID)
}

func New(opts Options) (*Engine, error) {
	var envMode os.FileMode
	switch opts.EnvMode {
	case "0600":
		envMode = 0600
	case "0644":
		envMode = 0644
	default:
		return nil, ErrInvalidEnvMode
	}

	revisionID := strings.TrimSpace(opts.NewRevisionID)
	if revisionID == "" {
		revisionID = uuid.NewString()
	} else if !validRevisionID(revisionID) {
		return nil, fmt.Errorf("invalid backup revision ID %q", revisionID)
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Engine{
		tables:                  opts.Tables,
		masterKeyBase64:         opts.MasterKeyBase64,
		envRestorePath:          opts.EnvRestorePath,
		candidateRoot:           opts.CandidateRoot,
		revisionID:              revisionID,
		now:                     now,
		apply:                   opts.Apply,
		audit:                   opts.Audit,
		auditActorID:            opts.AuditActorID,
		envMode:                 envMode,
		applyDoesNotMarkApplied: opts.ApplyDoesNotMarkApplied,
	}, nil
}

func validRevisionID(value string) bool {
	if value == "." || value == ".." || value == "" || strings.ContainsAny(value, `/\\`) {
		return false
	}
	return true
}

func (e *Engine) Import(ctx context.Context, archive zipextract.Archive, passphrase []byte, dryRun bool) (ImportReport, error) {
	var report ImportReport
	if ctx == nil {
		ctx = context.Background()
	}
	var audit AuditEmitter
	actorID := ""
	if e != nil {
		audit = e.audit
		actorID = e.auditActorID
		if actorID == "" {
			actorID = actorIDFromContext(ctx)
		}
	}
	bundleSHA256 := ""
	success := false
	defer func() {
		finalizeAudit(audit, actorID, bundleSHA256, dryRun, success)
	}()

	if e == nil || archive == nil {
		return report, zipextract.ErrCorruptArchive
	}
	_ = passphrase // The archive is already opened/decrypted by zipextract.

	files, err := archive.Files(ctx)
	if err != nil {
		return report, err
	}
	defer func() { _ = archive.Close() }()
	indexed, err := indexArchiveFiles(files)
	if err != nil {
		return report, zipextract.ErrCorruptArchive
	}

	manifest, manifestBytes, err := readManifest(ctx, indexed)
	if err != nil {
		return report, err
	}
	report = ImportReport{
		DryRun:            dryRun,
		BundleSHA256:      manifestDigest(manifestBytes),
		FormatVersion:     manifest.FormatVersion,
		ExporterVersion:   manifest.ExporterVersion,
		CreatedAt:         manifest.CreatedAt,
		InstallationID:    manifest.InstallationID,
		NodeID:            manifest.NodeID,
		EnvWouldChange:    manifest.EnvPresent,
		CertsToRestore:    certificatePaths(indexed, manifest.Certs),
		AppliedPostImport: false,
	}
	bundleSHA256 = report.BundleSHA256

	if err := verifyArchiveChecksums(ctx, indexed, manifest); err != nil {
		return report, err
	}
	if err := verifyMasterKey(ctx, indexed, e.masterKeyBase64); err != nil {
		return report, err
	}
	payloads, err := readTablePayloads(ctx, indexed, manifest.DBTables)
	if err != nil {
		return report, fmt.Errorf("read import tables: %w", err)
	}
	previews := previewTables(payloads)
	if dryRun {
		report.Tables = previews
		success = true
		return report, nil
	}
	if e.tables == nil {
		return report, errors.New("backup import: nil database pool")
	}
	previews, err = e.tables.ReplaceTables(ctx, payloads)
	if err != nil {
		return report, err
	}
	report.Tables = previews

	if err := restoreFiles(ctx, indexed, manifest, e.envRestorePath, e.candidateRoot, e.revisionID, e.envMode); err != nil {
		return report, err
	}
	if e.apply != nil {
		if err := e.apply.Trigger(ctx, actorID); err != nil {
			return report, err
		}
		if !e.applyDoesNotMarkApplied {
			report.AppliedPostImport = true
		}
	}
	success = true
	return report, nil
}

func indexArchiveFiles(files []zipextract.File) (map[string]zipextract.File, error) {
	indexed := make(map[string]zipextract.File, len(files))
	for _, file := range files {
		if file.Path == "" || file.Open == nil {
			return nil, errors.New("archive contains an invalid file")
		}
		if _, exists := indexed[file.Path]; exists {
			return nil, errors.New("archive contains a duplicate file")
		}
		indexed[file.Path] = file
	}
	return indexed, nil
}

func previewTables(payloads []tablePayload) []TablePreview {
	previews := make([]TablePreview, 0, len(payloads))
	for _, payload := range payloads {
		previews = append(previews, TablePreview{Name: payload.name, RowCount: len(payload.rows), Action: "truncate+reinsert"})
	}
	return previews
}

func finalizeAudit(audit AuditEmitter, actor string, sha string, dryRun, success bool) {
	if audit != nil {
		_ = audit.EmitBackupImport(context.Background(), actor, sha, dryRun, success)
	}
}

func actorIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	actorID, _ := ctx.Value(actorIDContextKey{}).(string)
	return actorID
}
