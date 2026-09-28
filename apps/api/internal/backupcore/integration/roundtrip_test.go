package integration

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zerkc/ProxyCore/apps/api/internal/auth"
	"github.com/zerkc/ProxyCore/apps/api/internal/backupcore/dbexport"
	"github.com/zerkc/ProxyCore/apps/api/internal/backupcore/dbimport"
	"github.com/zerkc/ProxyCore/apps/api/internal/backupcore/zipextract"
	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
	"github.com/zerkc/ProxyCore/apps/api/internal/httpserver"
	"github.com/zerkc/ProxyCore/apps/api/internal/secrets"
)

const (
	integrationAppliedRevision = "77777777-7777-4777-8777-777777777777"
	integrationInstallationID  = "88888888-8888-4888-8888-888888888888"
	integrationNodeID          = "99999999-9999-4999-8999-999999999999"
	integrationRestoreRevision = "restored-integration-revision"
)

var configTables = append([]string(nil), dbexport.DefaultConfigTables...)

const configWipeSQL = `TRUNCATE users, zones, dns_records, resolver_pools, forwarding_rules,
	stream_routes, secrets, internal_ca, internal_ca_enrollment_state,
	certificates, provider_connections, config_revisions,
	installation_settings, installation_identity, cluster_keys,
	node_state RESTART IDENTITY CASCADE`

type roundTripFixture struct {
	pool                 *pgxpool.Pool
	masterKey            string
	bundle               []byte
	envBefore            []byte
	envPath              string
	envRestorePath       string
	candidateRoot        string
	restoreCandidateRoot string
	revisionID           string
	counts               map[string]int
	checksums            map[string]string
	preExportCounts      map[string]int
	certFiles            map[string][]byte
	passphrase           []byte
}

func TestRoundTrip(t *testing.T) {
	fixture := newRoundTripFixture(t, []byte("integration-passphrase"))
	report, audit, err := importFixture(fixture, fixture.masterKey, fixture.passphrase)
	if err != nil {
		t.Fatalf("round-trip import: %v", err)
	}
	if report.DryRun {
		t.Fatal("round-trip: report says dryRun=true for a committed import")
	}
	if report.AppliedPostImport {
		t.Fatal("round-trip: report says appliedPostImport=true for the no-op apply")
	}
	if report.BundleSHA256 == "" {
		t.Fatal("round-trip: report bundleSha256 is empty")
	}
	if !report.EnvWouldChange {
		t.Fatal("round-trip: report envWouldChange=false for an empty destination env")
	}
	if audit.emissions != 1 || audit.successful != 1 {
		t.Fatalf("round-trip: audit emissions=%d successful=%d, want one successful emission", audit.emissions, audit.successful)
	}
	assertReportCounts(t, "round-trip", report, fixture.counts)
	assertDatabaseSnapshot(t, "round-trip", fixture.pool, fixture.counts, fixture.checksums)
	assertRestoredFiles(t, "round-trip", fixture)
}

func TestOpenIntegrationPoolUsesIsolatedSchemaForEnvFallback(t *testing.T) {
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL is required for the forced environment fallback")
	}
	t.Setenv("PGX_TEST_DATABASE_URL", "")
	// LookPath must fail even on hosts with Docker installed; restore PATH at cleanup.
	t.Setenv("PATH", t.TempDir())
	pool, _ := openIntegrationPool(t)
	var schema string
	if err := pool.QueryRow(context.Background(), `select current_schema()`).Scan(&schema); err != nil {
		t.Fatalf("read integration schema: %v", err)
	}
	if schema == "" || schema == "public" {
		t.Fatalf("integration fallback schema=%q, want a non-public schema", schema)
	}
	var name string
	if err := pool.QueryRow(context.Background(), `select column_name from information_schema.columns where table_schema = current_schema() and table_name = 'resolver_pools' and column_name = 'name'`).Scan(&name); err != nil {
		t.Fatalf("integration fallback resolver_pools.name: %v", err)
	}
}

func newRoundTripFixture(t *testing.T, passphrase []byte) *roundTripFixture {
	return newRoundTripFixtureWithWipeAfterExport(t, passphrase, true)
}

func newRoundTripFixtureWithWipeAfterExport(t *testing.T, passphrase []byte, wipeAfterExport bool) *roundTripFixture {
	t.Helper()
	pool, databaseURL := openIntegrationPool(t)
	wipeConfig(t, pool)

	masterKey := randomMasterKey(t)
	hostRoot := t.TempDir()
	envPath := filepath.Join(hostRoot, ".env")
	envBefore := writeIntegrationEnv(t, envPath, masterKey, databaseURL)
	candidateRoot := filepath.Join(hostRoot, "candidates")
	certFiles := writeIntegrationCertificates(t, candidateRoot, integrationAppliedRevision)
	insertIntegrationFixture(t, pool, masterKey)

	preExportCounts, checksums := snapshotDatabase(t, pool)
	if preExportCounts["secrets"] != 5 {
		t.Fatalf("export fixture: pre-export secrets rows=%d, want 5", preExportCounts["secrets"])
	}
	bundle, _, err := exportAll(context.Background(), pool, envPath, candidateRoot, masterKey, passphrase)
	if err != nil {
		t.Fatalf("export fixture: %v", err)
	}
	counts, bundleChecksums := snapshotBundle(t, bundle, passphrase)
	if counts["secrets"] != 6 {
		t.Fatalf("export fixture: bundle secrets rows=%d, want 6 including master-key marker", counts["secrets"])
	}
	checksums["secrets"] = bundleChecksums["secrets"]
	if wipeAfterExport {
		wipeConfig(t, pool)
	}

	return &roundTripFixture{
		pool:                 pool,
		masterKey:            masterKey,
		bundle:               bundle,
		envBefore:            envBefore,
		envPath:              envPath,
		envRestorePath:       filepath.Join(t.TempDir(), ".env"),
		candidateRoot:        candidateRoot,
		restoreCandidateRoot: t.TempDir(),
		revisionID:           integrationRestoreRevision,
		counts:               counts,
		checksums:            checksums,
		preExportCounts:      preExportCounts,
		certFiles:            certFiles,
		passphrase:           append([]byte(nil), passphrase...),
	}
}

func importFixture(fixture *roundTripFixture, masterKey string, passphrase []byte) (httpserver.ImportReport, *noopAudit, error) {
	ctx := context.Background()
	archive, err := zipextract.Open(ctx, bytes.NewReader(fixture.bundle), passphrase)
	if err != nil {
		if errors.Is(err, zipextract.ErrPassphraseMismatch) {
			return httpserver.ImportReport{}, nil, httpserver.ErrPassphraseMismatch
		}
		return httpserver.ImportReport{}, nil, err
	}
	audit := &noopAudit{}
	engine, err := dbimport.New(dbimport.Options{
		Pool:            fixture.pool,
		MasterKeyBase64: masterKey,
		EnvRestorePath:  fixture.envRestorePath,
		CandidateRoot:   fixture.restoreCandidateRoot,
		Apply:           noopApply(),
		Audit:           audit,
		EnvMode:         "0600",
		NewRevisionID:   fixture.revisionID,
	})
	if err != nil {
		_ = archive.Close()
		return httpserver.ImportReport{}, audit, err
	}
	report, err := engine.Import(ctx, archive, passphrase, false)
	return report, audit, err
}

// Engine records a successful apply whenever a non-nil trigger is supplied.
// A nil ApplyTrigger is therefore the precise no-op apply for this integration
// contract; it keeps AppliedPostImport false while still exercising restore.
func noopApply() dbimport.ApplyTrigger { return nil }

type noopAudit struct {
	mu         sync.Mutex
	emissions  int
	successful int
}

func (a *noopAudit) EmitBackupImport(_ context.Context, _ string, _ string, dryRun, success bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.emissions++
	if !dryRun && success {
		a.successful++
	}
	return nil
}

func assertReportCounts(t *testing.T, name string, report httpserver.ImportReport, want map[string]int) {
	t.Helper()
	got := make(map[string]int, len(report.Tables))
	for _, table := range report.Tables {
		got[table.Name] = table.RowCount
	}
	if len(got) != len(want) {
		t.Fatalf("%s: report table count=%d, want %d", name, len(got), len(want))
	}
	for table, count := range want {
		if got[table] != count {
			t.Fatalf("%s: report row count for %s=%d, want %d", name, table, got[table], count)
		}
	}
}

func assertRestoredFiles(t *testing.T, name string, fixture *roundTripFixture) {
	t.Helper()
	gotEnv, err := os.ReadFile(fixture.envRestorePath)
	if err != nil {
		t.Fatalf("%s: read restored env: %v", name, err)
	}
	if !bytes.Equal(gotEnv, fixture.envBefore) {
		t.Fatalf("%s: restored env differs byte-for-byte", name)
	}
	for filename, want := range fixture.certFiles {
		path := filepath.Join(fixture.restoreCandidateRoot, fixture.revisionID, "nginx", "certs", filename)
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: read restored certificate %s: %v", name, filename, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("%s: restored certificate %s differs byte-for-byte", name, filename)
		}
	}
}

func assertDatabaseSnapshot(t *testing.T, name string, pool *pgxpool.Pool, wantCounts map[string]int, wantChecksums map[string]string) {
	t.Helper()
	gotCounts, gotChecksums := snapshotDatabase(t, pool)
	for _, table := range configTables {
		if gotCounts[table] != wantCounts[table] {
			t.Fatalf("%s: %s row count=%d, want %d", name, table, gotCounts[table], wantCounts[table])
		}
		if gotChecksums[table] != wantChecksums[table] {
			t.Fatalf("%s: %s checksum=%s, want %s", name, table, gotChecksums[table], wantChecksums[table])
		}
	}
}

func snapshotDatabase(t *testing.T, pool *pgxpool.Pool) (map[string]int, map[string]string) {
	t.Helper()
	ctx := context.Background()
	counts := captureConfigRowCounts(t, pool)
	checksums := make(map[string]string, len(configTables))
	for _, table := range configTables {
		rows, err := pool.Query(ctx, "select * from "+quoteIdentifier(table)+" order by 1")
		if err != nil {
			t.Fatalf("query %s: %v", table, err)
		}
		values, err := pgx.CollectRows(rows, pgx.RowToMap)
		if err != nil {
			t.Fatalf("collect %s: %v", table, err)
		}
		encoded, err := json.Marshal(values)
		if err != nil {
			t.Fatalf("marshal %s snapshot: %v", table, err)
		}
		digest := sha256.Sum256(encoded)
		checksums[table] = hex.EncodeToString(digest[:])
	}
	return counts, checksums
}

func snapshotBundle(t *testing.T, bundle []byte, passphrase []byte) (map[string]int, map[string]string) {
	t.Helper()
	archive, err := zipextract.Open(context.Background(), bytes.NewReader(bundle), passphrase)
	if err != nil {
		t.Fatalf("open bundle snapshot: %v", err)
	}
	defer archive.Close()

	files, err := archive.Files(context.Background())
	if err != nil {
		t.Fatalf("list bundle snapshot: %v", err)
	}
	byPath := make(map[string]zipextract.File, len(files))
	for _, file := range files {
		byPath[file.Path] = file
	}

	counts := make(map[string]int, len(configTables))
	checksums := make(map[string]string, len(configTables))
	for _, table := range configTables {
		file, ok := byPath["db/"+table+".json"]
		if !ok {
			t.Fatalf("bundle snapshot: missing table %s", table)
		}
		reader, err := file.Open()
		if err != nil {
			t.Fatalf("open bundle table %s: %v", table, err)
		}
		data, readErr := io.ReadAll(reader)
		closeErr := reader.Close()
		if err := errors.Join(readErr, closeErr); err != nil {
			t.Fatalf("read bundle table %s: %v", table, err)
		}

		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.UseNumber()
		var rows []map[string]any
		if err := decoder.Decode(&rows); err != nil {
			t.Fatalf("decode bundle table %s: %v", table, err)
		}
		if err := normalizeBundleSnapshotRows(table, rows); err != nil {
			t.Fatalf("normalize bundle table %s: %v", table, err)
		}
		counts[table] = len(rows)
		encoded, err := json.Marshal(rows)
		if err != nil {
			t.Fatalf("marshal bundle table %s snapshot: %v", table, err)
		}
		digest := sha256.Sum256(encoded)
		checksums[table] = hex.EncodeToString(digest[:])
	}
	return counts, checksums
}

func normalizeBundleSnapshotRows(table string, rows []map[string]any) error {
	if table != "secrets" {
		return nil
	}
	for _, row := range rows {
		encodedID, ok := row["id"].(string)
		if !ok {
			return fmt.Errorf("secret id is %T, want string", row["id"])
		}
		id, err := uuid.Parse(encodedID)
		if err != nil {
			return fmt.Errorf("parse secret id: %w", err)
		}
		row["id"] = [16]byte(id)
		if row["purpose"] != dbexport.BackupMasterKeyMarkerPurpose {
			continue
		}
		for _, column := range []string{"created_at", "updated_at"} {
			value, ok := row[column].(string)
			if !ok {
				return fmt.Errorf("marker %s is %T, want string", column, row[column])
			}
			parsed, err := time.Parse(time.RFC3339Nano, value)
			if err != nil {
				return fmt.Errorf("parse marker %s: %w", column, err)
			}
			row[column] = parsed.UTC().Truncate(time.Microsecond).Format(time.RFC3339Nano)
		}
	}
	return nil
}

// captureConfigRowCounts records only database row counts so rejected imports
// can be compared before and after the import attempt without relying on a wipe.
func captureConfigRowCounts(t *testing.T, pool *pgxpool.Pool, tables ...string) map[string]int {
	t.Helper()
	if len(tables) == 0 {
		tables = configTables
	}
	ctx := context.Background()
	counts := make(map[string]int, len(tables))
	for _, table := range tables {
		var count int
		if err := pool.QueryRow(ctx, "select count(*) from "+quoteIdentifier(table)).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		counts[table] = count
	}
	return counts
}

// assertConfigRowCountsUnchanged proves a rejected import made no row-count
// changes, checking all 16 config tables unless a focused subset is supplied.
func assertConfigRowCountsUnchanged(t *testing.T, name string, before, after map[string]int, tables ...string) {
	t.Helper()
	if len(tables) == 0 {
		tables = configTables
	}
	for _, table := range tables {
		beforeCount, beforeOK := before[table]
		afterCount, afterOK := after[table]
		if !beforeOK || !afterOK || beforeCount != afterCount {
			t.Fatalf("%s: %s row count changed: before=%d after=%d", name, table, beforeCount, afterCount)
		}
	}
}

func quoteIdentifier(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}

func wipeConfig(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), configWipeSQL); err != nil {
		t.Fatalf("wipe configuration tables: %v", err)
	}
}

func randomMasterKey(t *testing.T) string {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("generate master key: %v", err)
	}
	return base64.StdEncoding.EncodeToString(key)
}

func writeIntegrationEnv(t *testing.T, path, masterKey, databaseURL string) []byte {
	t.Helper()
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse database URL for env fixture: %v", err)
	}
	content := fmt.Sprintf("PROXYCORE_MASTER_KEY_BASE64=%s\nPOSTGRES_HOST=%s\nPOSTGRES_PORT=%d\nPOSTGRES_USER=%s\nPOSTGRES_PASSWORD=%s\nPOSTGRES_DB=%s\nPROXYCORE_INGRESS_IPV4=192.0.2.10\nPROXYCORE_INGRESS_IPV6=2001:db8::10\nPROXYCORE_ACME_EMAIL=backup@example.test\nPROXYCORE_ACME_DIRECTORY_URL=https://acme.example.test/directory\n", masterKey, config.ConnConfig.Host, config.ConnConfig.Port, config.ConnConfig.User, config.ConnConfig.Password, config.ConnConfig.Database)
	data := []byte(content)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatalf("write fixture env: %v", err)
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatalf("chmod fixture env: %v", err)
	}
	return data
}

func writeIntegrationCertificates(t *testing.T, candidateRoot, revisionID string) map[string][]byte {
	t.Helper()
	certRoot := filepath.Join(candidateRoot, revisionID, "nginx", "certs")
	if err := os.MkdirAll(certRoot, 0755); err != nil {
		t.Fatalf("create certificate root: %v", err)
	}
	files := map[string][]byte{
		"proxycore-a.crt": []byte("-----BEGIN CERTIFICATE-----\nMIIBFAKECERTIFICATEA\n-----END CERTIFICATE-----\n"),
		"proxycore-a.key": []byte("-----BEGIN PRIVATE KEY-----\nMIIEFAKEPRIVATEKEYA\n-----END PRIVATE KEY-----\n"),
		"proxycore-b.crt": []byte("-----BEGIN CERTIFICATE-----\nMIIBFAKECERTIFICATEB\n-----END CERTIFICATE-----\n"),
		"proxycore-b.key": []byte("-----BEGIN PRIVATE KEY-----\nMIIEFAKEPRIVATEKEYB\n-----END PRIVATE KEY-----\n"),
	}
	for name, data := range files {
		path := filepath.Join(certRoot, name)
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatalf("write certificate %s: %v", name, err)
		}
	}
	return files
}

func insertIntegrationFixture(t *testing.T, pool *pgxpool.Pool, masterKey string) {
	t.Helper()
	ctx := context.Background()
	ownerID := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	operatorID := uuid.MustParse("22222222-2222-4222-8222-222222222222")
	poolIDs := []uuid.UUID{
		uuid.MustParse("33333333-3333-4333-8333-333333333333"),
		uuid.MustParse("44444444-4444-4444-8444-444444444444"),
	}
	zoneIDs := []uuid.UUID{
		uuid.MustParse("55555555-5555-4555-8555-555555555555"),
		uuid.MustParse("66666666-6666-4666-8666-666666666666"),
		uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"),
	}
	secretIDs := []uuid.UUID{
		uuid.MustParse("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"),
		uuid.MustParse("cccccccc-cccc-4ccc-8ccc-cccccccccccc"),
		uuid.MustParse("dddddddd-dddd-4ddd-8ddd-dddddddddddd"),
		uuid.MustParse("eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"),
		uuid.MustParse("ffffffff-ffff-4fff-8fff-ffffffffffff"),
	}
	certificateIDs := []uuid.UUID{
		uuid.MustParse("12121212-1212-4121-8121-121212121212"),
		uuid.MustParse("13131313-1313-4131-8131-131313131313"),
	}
	revisionIDs := []uuid.UUID{
		uuid.MustParse("14141414-1414-4141-8141-141414141414"),
		uuid.MustParse("15151515-1515-4151-8151-151515151515"),
		uuid.MustParse("16161616-1616-4161-8161-161616161616"),
		uuid.MustParse("17171717-1717-4171-8171-171717171717"),
		uuid.MustParse(integrationAppliedRevision),
	}
	clusterKeyID := uuid.MustParse("18181818-1818-4181-8181-181818181818")
	base := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	hashOwner, err := auth.HashPassword("owner-integration-password")
	if err != nil {
		t.Fatalf("hash owner password: %v", err)
	}
	hashOperator, err := auth.HashPassword("operator-integration-password")
	if err != nil {
		t.Fatalf("hash operator password: %v", err)
	}

	batch := &pgx.Batch{}
	batch.Queue(`insert into users (id, username, password_hash, role, active, password_change_required, created_at, updated_at) values ($1, $2, $3, $4::proxycore_role, $5, $6, $7, $8)`, ownerID, "backup-owner", hashOwner, "owner", true, false, base, base)
	batch.Queue(`insert into users (id, username, password_hash, role, active, password_change_required, created_at, updated_at) values ($1, $2, $3, $4::proxycore_role, $5, $6, $7, $8)`, operatorID, "backup-operator", hashOperator, "operator", true, false, base.Add(time.Minute), base.Add(time.Minute))

	for index, id := range secretIDs {
		ciphertext, err := secrets.EncryptSecret(fmt.Sprintf("fixture-secret-%d", index+1), masterKey)
		if err != nil {
			t.Fatalf("encrypt secret %d: %v", index+1, err)
		}
		at := base.Add(time.Duration(index+2) * time.Minute)
		batch.Queue(`insert into secrets (id, purpose, ciphertext, created_at, updated_at) values ($1, $2, $3, $4, $5)`, id, fmt.Sprintf("integration-secret-%d", index+1), ciphertext, at, at)
	}
	batch.Queue(`insert into resolver_pools (id, name, endpoints, is_default, created_at, updated_at) values ($1, $2, $3, $4, $5, $6)`, poolIDs[0], "public-resolvers", json.RawMessage(`["1.1.1.1","8.8.8.8"]`), true, base, base)
	batch.Queue(`insert into resolver_pools (id, name, endpoints, is_default, created_at, updated_at) values ($1, $2, $3, $4, $5, $6)`, poolIDs[1], "private-resolvers", json.RawMessage(`["10.0.0.53"]`), false, base.Add(time.Minute), base.Add(time.Minute))

	for index, id := range zoneIDs {
		batch.Queue(`insert into zones (id, name, enabled, created_at, updated_at) values ($1, $2, $3, $4, $5)`, id, []string{"example.test", "internal.test", "service.test"}[index], true, base.Add(time.Duration(index)*time.Minute), base.Add(time.Duration(index)*time.Minute))
	}
	for zoneIndex, zoneID := range zoneIDs {
		for recordIndex := 0; recordIndex < 5; recordIndex++ {
			recordID := uuid.New()
			typeName := "A"
			if recordIndex == 1 {
				typeName = "TXT"
			}
			value := json.RawMessage(fmt.Sprintf(`{"value":"record-%d-%d.example.test"}`, zoneIndex+1, recordIndex+1))
			at := base.Add(time.Duration(20+zoneIndex*5+recordIndex) * time.Minute)
			batch.Queue(`insert into dns_records (id, zone_id, name, type, value, ttl, enabled, proxied, proxy_settings, comment, created_at, updated_at) values ($1, $2, $3, $4::proxycore_record_type, $5, $6, $7, $8, $9, $10, $11, $12)`, recordID, zoneID, fmt.Sprintf("host-%d", recordIndex+1), typeName, value, 300+recordIndex, true, recordIndex%2 == 0, json.RawMessage(fmt.Sprintf(`{"cache":"%d"}`, recordIndex)), fmt.Sprintf("fixture record %d", recordIndex+1), at, at)
		}
	}
	batch.Queue(`insert into forwarding_rules (id, suffix, pool_id, created_at, updated_at) values ($1, $2, $3, $4, $5)`, uuid.New(), ".example.test", poolIDs[0], base, base)
	batch.Queue(`insert into forwarding_rules (id, suffix, pool_id, created_at, updated_at) values ($1, $2, $3, $4, $5)`, uuid.New(), ".internal.test", poolIDs[1], base.Add(time.Minute), base.Add(time.Minute))
	for index, protocol := range []string{"tcp", "udp", "tcp"} {
		port := []int{443, 5353, 8443}[index]
		batch.Queue(`insert into stream_routes (id, enabled, protocol, listen_address, listen_port, upstream, created_at, updated_at) values ($1, $2, $3::proxycore_stream_protocol, $4, $5, $6, $7, $8)`, uuid.New(), true, protocol, "0.0.0.0", port, json.RawMessage(fmt.Sprintf(`{"host":"upstream-%d","port":%d}`, index+1, port)), base.Add(time.Duration(index)*time.Minute), base.Add(time.Duration(index)*time.Minute))
	}
	for index := 0; index < 3; index++ {
		keySecret := secretIDs[index]
		enrollmentSecret := secretIDs[(index+1)%len(secretIDs)]
		batch.Queue(`insert into internal_ca (id, certificate_pem, key_secret_id, enrollment_certificate_pem, enrollment_key_secret_id, created_at, updated_at) values ($1, $2, $3, $4, $5, $6, $7)`, fmt.Sprintf("internal-ca-%d", index+1), fmt.Sprintf("-----BEGIN CERTIFICATE-----\nINTERNALCA%d\n-----END CERTIFICATE-----\n", index+1), keySecret, fmt.Sprintf("-----BEGIN CERTIFICATE-----\nENROLLMENTCA%d\n-----END CERTIFICATE-----\n", index+1), enrollmentSecret, base.Add(time.Duration(index)*time.Minute), base.Add(time.Duration(index)*time.Minute))
	}
	batch.Queue(`insert into internal_ca_enrollment_state (id, established_at) values ($1, $2)`, "default", base)
	batch.Queue(`insert into certificates (id, hostnames, issuer, challenge, environment, status, expires_at, renew_after, key_secret_id, certificate_pem, failure_reason, created_at, updated_at) values ($1, $2, $3::proxycore_certificate_issuer, $4::proxycore_certificate_challenge, $5, $6::proxycore_certificate_status, $7, $8, $9, $10, $11, $12, $13)`, certificateIDs[0], json.RawMessage(`["proxycore-a.example.test"]`), "self-signed", "none", "production", "active", base.Add(365*24*time.Hour), base.Add(300*24*time.Hour), secretIDs[0], "-----BEGIN CERTIFICATE-----\nCERTIFICATEA\n-----END CERTIFICATE-----\n", nil, base, base)
	batch.Queue(`insert into certificates (id, hostnames, issuer, challenge, environment, status, expires_at, renew_after, key_secret_id, certificate_pem, failure_reason, created_at, updated_at) values ($1, $2, $3::proxycore_certificate_issuer, $4::proxycore_certificate_challenge, $5, $6::proxycore_certificate_status, $7, $8, $9, $10, $11, $12, $13)`, certificateIDs[1], json.RawMessage(`["proxycore-b.example.test"]`), "letsencrypt", "dns-01", "staging", "issued", base.Add(180*24*time.Hour), base.Add(120*24*time.Hour), secretIDs[1], "-----BEGIN CERTIFICATE-----\nCERTIFICATEB\n-----END CERTIFICATE-----\n", "fixture certificate", base.Add(time.Minute), base.Add(time.Minute))
	batch.Queue(`insert into provider_connections (id, provider, name, secret_id, scope, enabled, created_at, updated_at) values ($1, $2, $3, $4, $5, $6, $7, $8)`, uuid.New(), "cloudflare", "primary-dns", secretIDs[2], "example.test", true, base, base)
	batch.Queue(`insert into provider_connections (id, provider, name, secret_id, scope, enabled, created_at, updated_at) values ($1, $2, $3, $4, $5, $6, $7, $8)`, uuid.New(), "route53", "secondary-dns", secretIDs[3], "internal.test", false, base.Add(time.Minute), base.Add(time.Minute))
	for index, id := range revisionIDs {
		appliedAt := any(nil)
		if index == len(revisionIDs)-1 {
			appliedAt = base.Add(10 * time.Hour)
		}
		batch.Queue(`insert into config_revisions (id, revision_number, checksum, snapshot, actor_user_id, created_at, applied_at) values ($1, $2, $3, $4, $5, $6, $7)`, id, index+1, fmt.Sprintf("integration-checksum-%02d", index+1), json.RawMessage(fmt.Sprintf(`{"revision":%d,"source":"integration"}`, index+1)), ownerID, base.Add(time.Duration(index)*time.Hour), appliedAt)
	}
	batch.Queue(`insert into installation_settings (id, ingress_ipv4, ingress_ipv6, default_resolver_pool, forwarding_rules, enrollment_hostnames, retention_max_age_days, retention_max_size_mb, current_desired_revision_id, current_applied_revision_id, created_at, updated_at) values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`, "default", "192.0.2.10", "2001:db8::10", json.RawMessage(fmt.Sprintf(`{"id":"%s"}`, poolIDs[0])), json.RawMessage(`[".example.test",".internal.test"]`), json.RawMessage(`["enroll.example.test"]`), 30, 128, revisionIDs[len(revisionIDs)-1].String(), revisionIDs[len(revisionIDs)-1].String(), base, base)
	batch.Queue(`insert into cluster_keys (id, purpose, wrapped_kek, wrapping_key_version, created_at, retired_at) values ($1, $2, $3, $4, $5, $6)`, clusterKeyID, "integration-cluster", "wrapped-integration-kek", 1, base, nil)
	batch.Queue(`insert into installation_identity (id, installation_id, node_id, role, leadership_generation, latest_known_generation, cluster_key_id, updated_at) values ($1, $2, $3, $4::proxycore_topology_role, $5, $6, $7, $8)`, "primary", uuid.MustParse(integrationInstallationID), uuid.MustParse(integrationNodeID), "standalone-primary", int64(7), int64(7), clusterKeyID, base)
	batch.Queue(`insert into node_state (id, enrolled_at, enrollment_token_hash, enrollment_primary_id, last_seen_at, last_applied_snapshot_id, updated_at) values ($1, $2, $3, $4, $5, $6, $7)`, "primary", base, "enrollment-token-hash", uuid.MustParse(integrationInstallationID), base.Add(time.Hour), nil, base.Add(time.Hour))

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin fixture transaction: %v", err)
	}
	batchResults := tx.SendBatch(ctx, batch)
	for index := 0; index < batch.Len(); index++ {
		if _, err := batchResults.Exec(); err != nil {
			_ = batchResults.Close()
			_ = tx.Rollback(ctx)
			t.Fatalf("fixture batch statement %d: %v", index, err)
		}
	}
	if err := batchResults.Close(); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("close fixture batch: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit fixture: %v", err)
	}
}

func openIntegrationPool(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	if dockerURL, stop, started := startDisposablePostgres(t); started {
		t.Cleanup(stop)
		t.Log("using disposable postgres:17-alpine")
		return buildIntegrationPool(t, dockerURL), dockerURL
	}
	envURL, ok := testDatabaseURL()
	if !ok {
		t.Skip("skipping backupcore integration: neither docker nor DATABASE_URL/PGX_TEST_DATABASE_URL is available")
	}
	t.Log("using env-provided test database URL")
	return buildIntegrationPool(t, envURL), envURL
}

func buildIntegrationPool(t *testing.T, url string) *pgxpool.Pool {
	t.Helper()
	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse integration database URL: %v", err)
	}
	config.MaxConns = 8
	pool, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatalf("create integration database pool: %v", err)
	}
	t.Cleanup(pool.Close)
	pingCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	for {
		if err := pool.Ping(pingCtx); err == nil {
			break
		} else if pingCtx.Err() != nil {
			t.Fatalf("ping integration database: %v", err)
		}
		time.Sleep(250 * time.Millisecond)
	}
	basePool := pool
	pool, _, cleanupSchema := dbexport.PerTestSchema(t, basePool)
	basePool.Close()
	t.Cleanup(pool.Close)
	t.Cleanup(cleanupSchema)
	if err := ensureIntegrationSchema(context.Background(), pool); err != nil {
		t.Fatalf("ensure integration schema: %v", err)
	}
	return pool
}

func ensureIntegrationSchema(ctx context.Context, pool *pgxpool.Pool) error {
	if err := auth.NewPostgresStore(pool).EnsureSchema(ctx); err != nil {
		return fmt.Errorf("ensure auth schema: %w", err)
	}
	if err := configuration.EnsureSchema(ctx, pool); err != nil {
		return fmt.Errorf("ensure configuration schema: %w", err)
	}
	// The Go schema bootstrap is authoritative for production tables. These two
	// tables are still migration-owned in this branch, so the integration setup
	// supplies their migration-equivalent shape when running against a fresh DB.
	if _, err := pool.Exec(ctx, `
		create table if not exists resolver_pools (
			id uuid primary key default gen_random_uuid(),
			name text not null,
			endpoints jsonb not null,
			is_default boolean not null default false,
			created_at timestamptz not null default now(),
			updated_at timestamptz not null default now()
		);
		create unique index if not exists resolver_pools_name_idx on resolver_pools (name);
		create table if not exists forwarding_rules (
			id uuid primary key default gen_random_uuid(),
			suffix text not null,
			pool_id uuid not null references resolver_pools(id) on delete cascade,
			created_at timestamptz not null default now(),
			updated_at timestamptz not null default now()
		);
		create unique index if not exists forwarding_rules_suffix_idx on forwarding_rules (suffix);
	`); err != nil {
		return fmt.Errorf("ensure migration-owned tables: %w", err)
	}
	// configuration.EnsureSchema adds this self-reference for later phase-2
	// lineage. The current exporter graph treats self-edges as cycles; the
	// fixture keeps source_revision_id NULL, so remove only that test-schema
	// edge to exercise the existing exporter/importer without changing data.
	if _, err := pool.Exec(ctx, `alter table config_revisions drop constraint if exists config_revisions_source_revision_id_fkey`); err != nil {
		return fmt.Errorf("remove exporter self-edge for integration schema: %w", err)
	}
	return nil
}

func startDisposablePostgres(t *testing.T) (string, func(), bool) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		return "", nil, false
	}
	name := fmt.Sprintf("proxycore-backup-integration-%d", time.Now().UnixNano())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "run", "-d", "--rm", "--name", name,
		"-e", "POSTGRES_USER=proxycore",
		"-e", "POSTGRES_PASSWORD=proxycore",
		"-e", "POSTGRES_DB=proxycore",
		"-p", "127.0.0.1::5432",
		"--tmpfs", "/var/lib/postgresql/data:rw,size=1g",
		"postgres:17-alpine")
	output, err := cmd.Output()
	if err != nil {
		return "", nil, false
	}
	containerID := strings.TrimSpace(string(output))
	if containerID == "" {
		return "", nil, false
	}
	stop := func() {
		if t.Failed() {
			inspectCtx, inspectCancel := context.WithTimeout(context.Background(), 5*time.Second)
			inspect, inspectErr := exec.CommandContext(inspectCtx, "docker", "inspect", "--format", `state={{.State.Status}} error={{.State.Error}} ports={{json .NetworkSettings.Ports}}`, containerID).CombinedOutput()
			inspectCancel()
			t.Logf("integration container %s inspect: %s (error: %v)", containerID, strings.TrimSpace(string(inspect)), inspectErr)
			logsCtx, logsCancel := context.WithTimeout(context.Background(), 5*time.Second)
			logs, logsErr := exec.CommandContext(logsCtx, "docker", "logs", "--tail", "100", containerID).CombinedOutput()
			logsCancel()
			t.Logf("integration container %s last 100 log lines: %s (error: %v)", containerID, strings.TrimSpace(string(logs)), logsErr)
		}
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer stopCancel()
		if output, err := exec.CommandContext(stopCtx, "docker", "stop", containerID).CombinedOutput(); err != nil {
			t.Logf("stop integration container %s: %v (%s)", containerID, err, strings.TrimSpace(string(output)))
		}
	}
	portCtx, portCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer portCancel()
	portOutput, err := exec.CommandContext(portCtx, "docker", "port", containerID, "5432/tcp").Output()
	if err != nil {
		stop()
		return "", nil, false
	}
	address := strings.TrimSpace(strings.SplitN(string(portOutput), "\n", 2)[0])
	_, port, err := net.SplitHostPort(address)
	if err != nil || port == "" {
		stop()
		return "", nil, false
	}
	return "postgres://proxycore:proxycore@127.0.0.1:" + port + "/proxycore?sslmode=disable", stop, true
}

func testDatabaseURL() (string, bool) {
	if value := os.Getenv("PGX_TEST_DATABASE_URL"); value != "" {
		return value, true
	}
	if value := os.Getenv("DATABASE_URL"); value != "" {
		return value, true
	}
	return "", false
}
