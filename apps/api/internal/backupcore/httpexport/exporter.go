package httpexport

// This adapter never logs or otherwise exposes the backup passphrase, master
// key, secret ciphertext plaintext, or certificate body.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zerkc/ProxyCore/apps/api/internal/backupcore"
	"github.com/zerkc/ProxyCore/apps/api/internal/backupcore/dbexport"
	"github.com/zerkc/ProxyCore/apps/api/internal/backupcore/hostread"
	"github.com/zerkc/ProxyCore/apps/api/internal/backupcore/zipwriter"
	"github.com/zerkc/ProxyCore/apps/api/internal/httpserver"
)

const aggregateLimit int64 = 16 << 30

type IdentitySource interface {
	InstallationIDString() string
	NodeIDString() string
	RoleString() string
}

// BackupExporter composes host and database snapshot readers into the HTTP
// backup archive without buffering the complete archive in memory.
type BackupExporter struct {
	Pool     *pgxpool.Pool
	Identity IdentitySource
	Version  string
	Now      func() time.Time
}

var _ httpserver.BackupExporter = (*BackupExporter)(nil)

type dbWriter struct {
	zw       zipwriter.Writer
	manifest *backupcore.Manifest
}

var _ dbexport.ZipWriter = (*dbWriter)(nil)

func (w *dbWriter) Add(path string, size int64, open func() (io.ReadCloser, error)) error {
	if w == nil || w.zw == nil {
		return errors.New("httpexport: nil archive writer")
	}
	if w.manifest == nil {
		return errors.New("httpexport: nil manifest")
	}
	if open == nil {
		return fmt.Errorf("httpexport: entry %q has no opener", path)
	}

	hasher := sha256.New()
	wrappedOpen := func() (io.ReadCloser, error) {
		reader, err := open()
		if err != nil {
			if reader != nil {
				_ = reader.Close()
			}
			return nil, err
		}
		if reader == nil {
			return nil, fmt.Errorf("entry %q opener returned nil reader", path)
		}
		return &checksumReadCloser{
			Reader: io.TeeReader(reader, hasher),
			closer: reader,
		}, nil
	}
	if err := w.zw.Add(path, size, wrappedOpen); err != nil {
		return err
	}
	w.manifest.EntryChecksums[path] = hex.EncodeToString(hasher.Sum(nil))
	return nil
}

type checksumReadCloser struct {
	io.Reader
	closer io.Closer
}

func (r *checksumReadCloser) Close() error {
	if r == nil || r.closer == nil {
		return nil
	}
	return r.closer.Close()
}

// Export streams the host and database entries into an optional encrypted ZIP
// and returns the SHA-256 digest of the canonical manifest bytes.
func (e *BackupExporter) Export(ctx context.Context, w io.Writer, passphrase []byte) (manifestSHA256Hex string, err error) {
	if e == nil {
		return "", errors.New("httpexport: nil exporter")
	}
	if e.Pool == nil {
		return "", errors.New("httpexport: nil database pool")
	}
	if e.Identity == nil {
		return "", errors.New("httpexport: nil identity source")
	}
	if w == nil {
		return "", errors.New("httpexport: nil output writer")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	now := e.Now
	if now == nil {
		now = time.Now
	}

	manifest := backupcore.NewManifest(e.Version)
	manifest.CreatedAt = now().UTC().Format(time.RFC3339)
	manifest.InstallationID = e.Identity.InstallationIDString()
	manifest.NodeID = e.Identity.NodeIDString()
	manifest.Role = e.Identity.RoleString()
	if manifest.InstallationID == "" || manifest.NodeID == "" || manifest.Role == "" {
		return "", errors.New("httpexport: incomplete identity")
	}

	zw, err := zipwriter.OpenEncrypted(w, passphrase, zipwriter.WithAggregateLimit(aggregateLimit))
	if err != nil {
		return "", fmt.Errorf("open backup archive: %w", err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = zw.Close()
		}
	}()

	entries := &dbWriter{zw: zw, manifest: &manifest}
	host := hostread.New(e.Pool, hostread.Config{})
	envFile, err := host.EnvFile(ctx)
	if err != nil {
		return "", fmt.Errorf("read env snapshot: %w", err)
	}
	if err := entries.Add(envFile.Path, envFile.Size, envFile.Open); err != nil {
		return "", fmt.Errorf("write env snapshot: %w", err)
	}
	manifest.EnvPresent = true

	appliedRevision, err := host.AppliedRevision(ctx)
	if err != nil {
		if !errors.Is(err, hostread.ErrNoAppliedRevision) {
			return "", fmt.Errorf("read applied revision: %w", err)
		}
	} else {
		certFiles, certErr := host.AppliedCertFiles(ctx, appliedRevision)
		if certErr != nil {
			return "", fmt.Errorf("read applied certificates: %w", certErr)
		}
		for _, certFile := range certFiles {
			if err := entries.Add(certFile.Path, certFile.Size, certFile.Open); err != nil {
				return "", fmt.Errorf("write certificate %q: %w", certFile.Path, err)
			}
			manifest.Certs = append(manifest.Certs, certFile.Path)
		}
	}

	databaseReport, err := dbexport.New(e.Pool, dbexport.ExporterOptions{Now: now}).Export(ctx, entries)
	if err != nil {
		return "", fmt.Errorf("export database: %w", err)
	}
	manifest.DBTables = make([]string, 0, len(databaseReport.Tables))
	manifest.Counts = make(map[string]int, len(databaseReport.Tables))
	for _, table := range databaseReport.Tables {
		manifest.DBTables = append(manifest.DBTables, table.Name)
		manifest.Counts[table.Name] = table.RowCount
	}

	manifestBytes, err := backupcore.MarshalManifest(manifest)
	if err != nil {
		return "", fmt.Errorf("marshal manifest: %w", err)
	}
	if err := zw.Add("manifest.json", int64(len(manifestBytes)), func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(manifestBytes)), nil
	}); err != nil {
		return "", fmt.Errorf("write manifest: %w", err)
	}
	if err := zw.Close(); err != nil {
		closed = true
		return "", fmt.Errorf("close backup archive: %w", err)
	}
	closed = true

	digest := sha256.Sum256(manifestBytes)
	return hex.EncodeToString(digest[:]), nil
}
