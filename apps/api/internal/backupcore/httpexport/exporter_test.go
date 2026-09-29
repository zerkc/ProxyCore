package httpexport

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zerkc/ProxyCore/apps/api/internal/auth"
	"github.com/zerkc/ProxyCore/apps/api/internal/backupcore/dbexport"
	"github.com/zerkc/ProxyCore/apps/api/internal/backupcore/zipextract"
	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
	"github.com/zerkc/ProxyCore/apps/api/internal/secrets"
)

func TestBackupExporterRejectsNilPool(t *testing.T) {
	var out bytes.Buffer
	_, err := (&BackupExporter{}).Export(context.Background(), &out, nil)
	if err == nil {
		t.Fatal("Export unexpectedly succeeded without a database pool")
	}
	if out.Len() != 0 {
		t.Fatalf("Export wrote %d bytes before rejecting nil pool", out.Len())
	}
}

func TestBackupExporterPostgresRawRoundTrip(t *testing.T) {
	fixture := newHTTPExportFixture(t)
	exporter := &BackupExporter{
		Pool:            fixture.pool,
		Identity:        staticIdentitySource{},
		Version:         "test-exporter",
		Now:             func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) },
		MasterKeyBase64: fixture.masterKey,
	}
	var bundle bytes.Buffer
	digest, err := exporter.Export(context.Background(), &bundle, nil)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if bundle.Len() == 0 || digest == "" {
		t.Fatalf("Export produced bundle length=%d digest=%q", bundle.Len(), digest)
	}

	archive, err := zipextract.Open(context.Background(), bytes.NewReader(bundle.Bytes()), nil)
	if err != nil {
		t.Fatalf("Open exported bundle: %v", err)
	}
	defer archive.Close()
	files, err := archive.Files(context.Background())
	if err != nil {
		t.Fatalf("Files: %v", err)
	}
	var manifestBytes []byte
	for _, file := range files {
		if file.Path != "manifest.json" {
			continue
		}
		reader, err := file.Open()
		if err != nil {
			t.Fatalf("open manifest: %v", err)
		}
		manifestBytes, err = io.ReadAll(reader)
		closeErr := reader.Close()
		if err != nil || closeErr != nil {
			t.Fatalf("read manifest: read=%v close=%v", err, closeErr)
		}
	}
	if len(manifestBytes) == 0 {
		t.Fatal("exported archive has no manifest")
	}
	if got := manifestDigestForTest(manifestBytes); got != digest {
		t.Fatalf("manifest digest=%q, Export digest=%q", got, digest)
	}
}

func TestBackupExporterFixtureUsesIsolatedSchema(t *testing.T) {
	fixture := newHTTPExportFixture(t)
	var schema string
	if err := fixture.pool.QueryRow(context.Background(), `select current_schema()`).Scan(&schema); err != nil {
		t.Fatalf("read fixture schema: %v", err)
	}
	if schema == "" || schema == "public" {
		t.Fatalf("fixture schema=%q, want a non-public schema", schema)
	}
}

type staticIdentitySource struct{}

func (staticIdentitySource) InstallationIDString() string { return "installation-test" }
func (staticIdentitySource) NodeIDString() string         { return "node-test" }
func (staticIdentitySource) RoleString() string           { return "standalone-primary" }

type httpExportFixture struct {
	pool      *pgxpool.Pool
	masterKey string
	hostRoot  string
}

func newHTTPExportFixture(t *testing.T) *httpExportFixture {
	t.Helper()
	pool := dbexport.NewTestPoolFromEnvWithSchema(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("ping database: %v", err)
	}
	if err := auth.NewPostgresStore(pool).EnsureSchema(ctx); err != nil {
		t.Fatalf("ensure auth schema: %v", err)
	}
	if err := configuration.EnsureSchema(ctx, pool); err != nil {
		t.Fatalf("ensure configuration schema: %v", err)
	}
	for _, statement := range []string{
		`create table if not exists resolver_pools (id text primary key)`,
		`create table if not exists forwarding_rules (id text primary key)`,
		// The production schema has a self-reference used for lineage. The
		// exporter-order package reports that self-edge as a cycle; keep this
		// fixture focused on the HTTP composition contract.
		`alter table config_revisions drop constraint if exists config_revisions_source_revision_id_fkey`,
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("ensure fixture table: %v", err)
		}
	}
	if _, err := pool.Exec(ctx, `truncate users, zones, dns_records, resolver_pools, forwarding_rules,
	stream_routes, secrets, internal_ca, internal_ca_enrollment_state,
	certificates, provider_connections, config_revisions,
	installation_settings, installation_identity, cluster_keys,
	node_state restart identity cascade`); err != nil {
		t.Fatalf("wipe configuration tables: %v", err)
	}

	masterKey := testImporterMasterKey()
	ciphertext, err := secrets.EncryptSecret("fixture", masterKey)
	if err != nil {
		t.Fatalf("EncryptSecret: %v", err)
	}
	if _, err := pool.Exec(ctx, `insert into secrets (id, purpose, ciphertext, created_at, updated_at)
		values ($1, $2, $3, $4, $4)`, "11111111-1111-4111-8111-111111111111", "fixture", ciphertext, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)); err != nil {
		t.Fatalf("seed secret: %v", err)
	}

	hostRoot := t.TempDir()
	t.Setenv("PROXYCORE_HOST_HOME", hostRoot)
	if err := os.WriteFile(filepath.Join(hostRoot, ".env"), []byte("PROXYCORE_MASTER_KEY_BASE64="+masterKey+"\n"), 0600); err != nil {
		t.Fatalf("write host env: %v", err)
	}
	return &httpExportFixture{pool: pool, masterKey: masterKey, hostRoot: hostRoot}
}

func manifestDigestForTest(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}
