package configuration

import (
	"context"
	"database/sql"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

func TestSQLiteWorkerNodeStateSaveRecordFixture(t *testing.T) {
	if testing.Short() {
		t.Skip("external Bun fixture")
	}
	path := filepath.Join(t.TempDir(), "node-save.db")
	db, err := sqlitedb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := sqlitedb.Migrate(context.Background(), db); err != nil {
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
	runner := filepath.Clean(filepath.Join(filepath.Dir(source), "../../../../packages/db/src/sqlite-foundation/node-state-save-record.bun.ts"))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bun, "--no-env-file", runner, path)
	cmd.Dir = t.TempDir()
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Go-migrated Bun node save fixture: %v\n%s", err, out)
	}
	t.Logf("embedded Bun cases:\n%s", out)
	reopened, err := sqlitedb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	const nativeQuery = `SELECT json_array(id,enrolled_at,enrollment_token_hash,enrollment_primary_id,
		last_seen_at,last_applied_snapshot_id,enrollment_attempt_id,primary_url,primary_installation_id,
		primary_tls_spki_sha256,credential_id,sync_enabled,last_attempt_at,last_success_at,
		consecutive_failures,next_attempt_at,last_error_code,updated_at) FROM main.node_state WHERE id='default'`
	const expected = `["default","109223372036854775807.999000000","synthetic-ignored-test-only","aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa","109223372036854775807.999000000","aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa","aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa","  Raw 雪 ? '  ","aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa","  Raw 雪 ? '  ","aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",1,"109223372036854775807.999000000","109223372036854775807.999000000",-7,"109223372036854775807.999000000","  Raw 雪 ? '  ","109223372036854775807.999000000"]`
	for name, connection := range map[string]*sql.DB{
		"original": db, "reopened": reopened,
	} {
		t.Run(name, func(t *testing.T) {
			var actual string
			if err := connection.QueryRowContext(context.Background(), nativeQuery).Scan(&actual); err != nil {
				t.Fatal(err)
			}
			if actual != expected {
				t.Fatalf("native node bytes: got %s want %s", actual, expected)
			}
		})
	}
}
