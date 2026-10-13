package configuration

import (
	"context"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

func TestSQLiteWorkerEnrollmentAttemptCreateRecordFixture(t *testing.T) {
	if testing.Short() {
		t.Skip("external Bun fixture")
	}
	path := filepath.Join(t.TempDir(), "enrollment-create.db")
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
	foundation := filepath.Clean(filepath.Join(filepath.Dir(source), "../../../../packages/db/src/sqlite-foundation"))
	// Complete runtime files copied to an isolated cwd; never resolve repository .env.
	fixture := t.TempDir()
	for _, name := range []string{
		"enrollment-attempt-create-record.bun.ts",
		"enrollment-attempt-create-record.ts",
		"timestamp.ts",
	} {
		data, err := os.ReadFile(filepath.Join(foundation, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(fixture, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bun, "--no-env-file", filepath.Join(fixture, "enrollment-attempt-create-record.bun.ts"), path)
	cmd.Dir = fixture
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Go-migrated Bun enrollment create fixture: %v\n%s", err, out)
	}
	t.Logf("embedded Bun cases:\n%s", out)
	if !strings.Contains(string(out), "enrollment create complete:") {
		t.Fatal("Bun fixture did not reach its final assertion")
	}
	reopened, err := sqlitedb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	const nativeQuery = `SELECT json_array(id,state,primary_url,expected_primary_id,verified_primary_id,
		verified_primary_node_id,verified_leadership_generation,verified_primary_tls_spki_sha256,
		verified_primary_ca_fingerprint,preview_digest,local_node_ip,archive_id,ephemeral_private_key_wrapped,
		bootstrap_payload,node_credential_secret_id,cluster_key_id,initial_snapshot_hash,
		initial_snapshot_revision_id,initial_apply_job_id,failure_code,confirmed_at,created_at,updated_at)
		FROM main.enrollment_attempts WHERE id='aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa'`
	const expected = `["aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa","committed","  Raw 雪 ? '  ","bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb","bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb","bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb",9007199254740992,"tls synthetic","ca synthetic","preview synthetic","127.0.0.42","bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb","wrapped synthetic only","Bootstrap 雪","bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb","bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb","snapshot synthetic","bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb","bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb","failure synthetic","109223372036854775807.999000000","109223372036854775808.000000000","109223372036854775808.001000000"]`
	for name, connection := range map[string]*sql.DB{
		"original": db, "reopened": reopened,
	} {
		t.Run(name, func(t *testing.T) {
			var actual string
			if err := connection.QueryRowContext(context.Background(), nativeQuery).Scan(&actual); err != nil {
				t.Fatal(err)
			}
			if actual != expected {
				t.Fatalf("native enrollment bytes: got %s want %s", actual, expected)
			}
			var count int
			if err := connection.QueryRowContext(context.Background(), "SELECT count(*) FROM main.enrollment_attempts").Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 1 {
				t.Fatalf("unexpected final row count: %d", count)
			}
		})
	}
}
