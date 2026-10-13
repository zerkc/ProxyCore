package configuration

import (
	"context"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

func TestSQLiteWorkerNodeStateRecordFixture(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node-state.db")
	db, err := sqlitedb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := sqlitedb.Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	// The ignored value is a disposable test sentinel, never a credential.
	if _, err := db.Exec(`INSERT INTO node_state(id,updated_at,enrollment_token_hash)
		VALUES('default','109223372036854775808.000000001','synthetic-ignored-test-only')`); err != nil {
		t.Fatal(err)
	}
	const bytesQuery = `SELECT json_array(id,enrolled_at,enrollment_token_hash,enrollment_primary_id,
		last_seen_at,last_applied_snapshot_id,enrollment_attempt_id,primary_url,primary_installation_id,
		primary_tls_spki_sha256,credential_id,sync_enabled,last_attempt_at,last_success_at,
		consecutive_failures,next_attempt_at,last_error_code,updated_at) FROM node_state WHERE id='default'`
	var before string
	if err := db.QueryRow(bytesQuery).Scan(&before); err != nil {
		t.Fatal(err)
	}
	bun, err := exec.LookPath("bun")
	if err != nil {
		t.Fatal("required Bun executable:", err)
	}
	bun, err = filepath.Abs(bun)
	if err != nil {
		t.Fatal(err)
	}
	_, source, _, _ := runtime.Caller(0)
	runner := filepath.Clean(filepath.Join(filepath.Dir(source), "../../../../packages/db/src/sqlite-foundation/node-state-record.bun.ts"))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bun, "--no-env-file", runner, path)
	cmd.Dir = t.TempDir()
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Go-migrated Bun node-state fixture: %v\n%s", err, out)
	}
	t.Logf("embedded Bun cases:\n%s", out)
	reopened, err := sqlitedb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	var after string
	if err := reopened.QueryRow(bytesQuery).Scan(&after); err != nil || after != before {
		t.Fatal("reopened native node bytes changed:", err, after)
	}
}
