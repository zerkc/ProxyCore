package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zerkc/ProxyCore/apps/api/internal/backupcore"
	"github.com/zerkc/ProxyCore/apps/api/internal/backupcore/dbexport"
	"github.com/zerkc/ProxyCore/apps/api/internal/backupcore/hostread"
	"github.com/zerkc/ProxyCore/apps/api/internal/backupcore/zipwriter"
	"github.com/zerkc/ProxyCore/apps/api/internal/httpserver"
)

// namedFileCollector is the test-only output port used to turn the streaming
// database exporter output into NamedFiles before the archive is finalized.
// Production export code streams directly to the HTTP response; this adapter
// exists only so the integration test can compute the manifest checksums before
// writing the manifest entry.
type namedFileCollector struct {
	files []backupcore.NamedFile
}

func (c *namedFileCollector) Add(path string, size int64, open func() (io.ReadCloser, error)) error {
	if open == nil {
		return fmt.Errorf("collect %q: nil opener", path)
	}
	reader, err := open()
	if err != nil {
		if reader != nil {
			_ = reader.Close()
		}
		return fmt.Errorf("collect %q: open: %w", path, err)
	}
	if reader == nil {
		return fmt.Errorf("collect %q: opener returned nil reader", path)
	}
	data, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return fmt.Errorf("collect %q: %w", path, err)
	}
	c.files = append(c.files, backupcore.NamedFile{
		Path: path,
		Size: int64(len(data)),
		Open: func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(data)), nil
		},
	})
	return nil
}

// exportAll is deliberately kept in the integration package. It composes the
// real host reader, database exporter, and ZIP writer without adding a second
// production orchestration boundary before the REST layer exists.
func exportAll(ctx context.Context, pool *pgxpool.Pool, env, candidateRoot string, passphrase []byte) ([]byte, httpserver.ImportReport, error) {
	var emptyReport httpserver.ImportReport
	if ctx == nil {
		ctx = context.Background()
	}
	if pool == nil {
		return nil, emptyReport, errors.New("integration export: nil database pool")
	}

	h := hostread.New(pool, hostread.Config{EnvPath: env, CandidateRoot: candidateRoot})
	ex := dbexport.New(pool, dbexport.ExporterOptions{})

	var bundle bytes.Buffer
	zw, err := zipwriter.OpenEncrypted(&bundle, passphrase)
	if err != nil {
		return nil, emptyReport, err
	}
	closed := false
	defer func() {
		if !closed {
			_ = zw.Close()
		}
	}()

	envFile, err := h.EnvFile(ctx)
	if err != nil {
		return nil, emptyReport, fmt.Errorf("read env snapshot: %w", err)
	}
	appliedRevision, err := h.AppliedRevision(ctx)
	if err != nil {
		return nil, emptyReport, fmt.Errorf("read applied revision: %w", err)
	}
	certFiles, err := h.AppliedCertFiles(ctx, appliedRevision)
	if err != nil {
		return nil, emptyReport, fmt.Errorf("read applied certificates: %w", err)
	}

	collector := &namedFileCollector{}
	exportReport, err := ex.Export(ctx, collector)
	if err != nil {
		return nil, emptyReport, fmt.Errorf("export database: %w", err)
	}

	installationID, nodeID, role, err := readManifestIdentity(ctx, pool)
	if err != nil {
		return nil, emptyReport, err
	}

	files := make([]backupcore.NamedFile, 0, 1+len(certFiles)+len(collector.files))
	files = append(files, envFile)
	files = append(files, certFiles...)
	files = append(files, collector.files...)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })

	checksums, err := backupcore.ComputeEntryChecksums(files)
	if err != nil {
		return nil, emptyReport, fmt.Errorf("compute bundle checksums: %w", err)
	}

	manifest := backupcore.NewManifest("0.4.0")
	manifest.InstallationID = installationID
	manifest.NodeID = nodeID
	manifest.Role = role
	manifest.EnvPresent = true
	manifest.Certs = make([]string, 0, len(certFiles))
	for _, file := range certFiles {
		manifest.Certs = append(manifest.Certs, file.Path)
	}
	manifest.DBTables = make([]string, 0, len(exportReport.Tables))
	manifest.Counts = make(map[string]int, len(exportReport.Tables))
	for _, table := range exportReport.Tables {
		manifest.DBTables = append(manifest.DBTables, table.Name)
		manifest.Counts[table.Name] = table.RowCount
	}
	// The importer truncates each table with CASCADE. node_state has an
	// optional FK into an excluded enrollment table, so keep it after the
	// config_revisions truncate that cascades through that runtime graph.
	manifest.DBTables = moveTableToEnd(manifest.DBTables, "node_state")
	manifest.EntryChecksums = checksums

	for _, file := range files {
		if err := zw.Add(file.Path, file.Size, file.Open); err != nil {
			return nil, emptyReport, fmt.Errorf("write bundle entry %q: %w", file.Path, err)
		}
	}
	manifestBytes, err := backupcore.MarshalManifest(manifest)
	if err != nil {
		return nil, emptyReport, fmt.Errorf("marshal manifest: %w", err)
	}
	if err := zw.Add("manifest.json", int64(len(manifestBytes)), func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(manifestBytes)), nil
	}); err != nil {
		return nil, emptyReport, fmt.Errorf("write manifest: %w", err)
	}
	if err := zw.Close(); err != nil {
		closed = true
		return nil, emptyReport, fmt.Errorf("close bundle: %w", err)
	}
	closed = true

	report := httpserver.ImportReport{
		BundleSHA256:    manifestDigest(manifestBytes),
		FormatVersion:   manifest.FormatVersion,
		ExporterVersion: manifest.ExporterVersion,
		CreatedAt:       manifest.CreatedAt,
		InstallationID:  manifest.InstallationID,
		NodeID:          manifest.NodeID,
		EnvWouldChange:  manifest.EnvPresent,
		CertsToRestore:  append([]string(nil), manifest.Certs...),
		Tables:          make([]httpserver.TablePreview, 0, len(exportReport.Tables)),
	}
	for _, table := range exportReport.Tables {
		report.Tables = append(report.Tables, httpserver.TablePreview{
			Name:     table.Name,
			RowCount: table.RowCount,
			Action:   "export",
		})
	}
	return bundle.Bytes(), report, nil
}

func moveTableToEnd(tables []string, target string) []string {
	result := make([]string, 0, len(tables))
	found := false
	for _, table := range tables {
		if table == target {
			found = true
			continue
		}
		result = append(result, table)
	}
	if found {
		result = append(result, target)
	}
	return result
}

func readManifestIdentity(ctx context.Context, pool *pgxpool.Pool) (string, string, string, error) {
	var installationID, nodeID, role string
	if err := pool.QueryRow(ctx, `
		select installation_id::text, node_id::text, role::text
		from installation_identity
		order by id
		limit 1
	`).Scan(&installationID, &nodeID, &role); err != nil {
		return "", "", "", fmt.Errorf("read manifest identity: %w", err)
	}
	return installationID, nodeID, role, nil
}

func manifestDigest(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}
