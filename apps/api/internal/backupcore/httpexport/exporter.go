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
	"io/fs"
	"os"
	"strings"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/backupcore"
	"github.com/zerkc/ProxyCore/apps/api/internal/backupcore/dbexport"
	"github.com/zerkc/ProxyCore/apps/api/internal/backupcore/hostread"
	"github.com/zerkc/ProxyCore/apps/api/internal/backupcore/zipwriter"
)

const aggregateLimit int64 = 16 << 30

type IdentitySource interface {
	InstallationIDString() string
	NodeIDString() string
	RoleString() string
}

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

// bundleConfig carries the storage-specific dependencies of a backup so the
// SQLite exporter and any future database backend share one bundle assembly flow.
type bundleConfig struct {
	Identity        IdentitySource
	Version         string
	Now             func() time.Time
	MasterKeyBase64 string
	Host            hostread.HostSnapshotReader
	ExportDB        func(ctx context.Context, entries dbexport.ZipWriter, now func() time.Time, masterKeyBase64 string) (dbexport.ExportReport, error)
}

func exportBundle(ctx context.Context, w io.Writer, passphrase []byte, cfg bundleConfig) (manifestSHA256Hex string, err error) {
	if cfg.Identity == nil {
		return "", errors.New("httpexport: nil identity source")
	}
	if w == nil {
		return "", errors.New("httpexport: nil output writer")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}

	manifest := backupcore.NewManifest(cfg.Version)
	manifest.CreatedAt = now().UTC().Format(time.RFC3339)
	manifest.InstallationID = cfg.Identity.InstallationIDString()
	manifest.NodeID = cfg.Identity.NodeIDString()
	manifest.Role = cfg.Identity.RoleString()
	if manifest.InstallationID == "" || manifest.NodeID == "" || manifest.Role == "" {
		return "", errors.New("httpexport: incomplete identity")
	}

	// Every host read that can fail runs before the archive is opened, so a
	// failure here leaves the output untouched and the caller can still answer
	// with an error instead of a truncated or empty archive.
	host := cfg.Host
	envFile, err := host.EnvFile(ctx)
	envPresent := true
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// A host without a readable .env (for example a container that does not
		// mount it) is exported without the env entry.
		envFile, envPresent = backupcore.NamedFile{}, false
	case err != nil:
		return "", fmt.Errorf("read env snapshot: %w", err)
	}
	masterKeyBase64, err := resolveMasterKey(cfg.MasterKeyBase64, envFile)
	if err != nil {
		return "", err
	}
	var certFiles []backupcore.NamedFile
	appliedRevision, err := host.AppliedRevision(ctx)
	if err != nil {
		if !errors.Is(err, hostread.ErrNoAppliedRevision) {
			return "", fmt.Errorf("read applied revision: %w", err)
		}
	} else {
		certFiles, err = host.AppliedCertFiles(ctx, appliedRevision)
		if err != nil {
			return "", fmt.Errorf("read applied certificates: %w", err)
		}
	}

	zw, err := zipwriter.OpenEncrypted(w, passphrase, zipwriter.WithAggregateLimit(aggregateLimit))
	if err != nil {
		return "", fmt.Errorf("open backup archive: %w", err)
	}
	closed := false
	defer func() {
		if closed {
			return
		}
		// A failed export must not write the archive footer onto the output.
		if aborter, ok := zw.(interface{ Abort() error }); ok {
			_ = aborter.Abort()
			return
		}
		_ = zw.Close()
	}()

	entries := &dbWriter{zw: zw, manifest: &manifest}
	if envPresent {
		if err := entries.Add(envFile.Path, envFile.Size, envFile.Open); err != nil {
			return "", fmt.Errorf("write env snapshot: %w", err)
		}
	}
	manifest.EnvPresent = envPresent
	for _, certFile := range certFiles {
		if err := entries.Add(certFile.Path, certFile.Size, certFile.Open); err != nil {
			return "", fmt.Errorf("write certificate %q: %w", certFile.Path, err)
		}
		manifest.Certs = append(manifest.Certs, certFile.Path)
	}

	databaseReport, err := cfg.ExportDB(ctx, entries, now, masterKeyBase64)
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

func resolveMasterKey(explicit string, envFile backupcore.NamedFile) (string, error) {
	if value := strings.TrimSpace(explicit); value != "" {
		return value, nil
	}
	if value := strings.TrimSpace(os.Getenv("PROXYCORE_MASTER_KEY_BASE64")); value != "" {
		return value, nil
	}
	if envFile.Open == nil {
		return "", errors.New("httpexport: runtime master key is not configured")
	}
	reader, err := envFile.Open()
	if err != nil {
		return "", fmt.Errorf("read runtime master key: %w", err)
	}
	if reader == nil {
		return "", errors.New("read runtime master key: env opener returned nil reader")
	}
	data, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return "", fmt.Errorf("read runtime master key: %w", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) != "PROXYCORE_MASTER_KEY_BASE64" {
			continue
		}
		value = strings.Trim(strings.TrimSpace(value), "\"'")
		if value != "" {
			return value, nil
		}
	}
	return "", errors.New("httpexport: runtime master key is not configured")
}
