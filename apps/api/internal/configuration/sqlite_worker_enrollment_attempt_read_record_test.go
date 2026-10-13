package configuration

import (
	"context"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

func TestSQLiteWorkerEnrollmentAttemptReadRecordFixture(t *testing.T) {
	path := filepath.Join(t.TempDir(), "enrollment-attempt-read.db")
	db, err := sqlitedb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := sqlitedb.Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	// Synthetic NULL-FK fixture: no parent records or actual key material.
	if _, err := db.Exec(`INSERT INTO enrollment_attempts
		(id,state,primary_url,local_node_ip,ephemeral_private_key_wrapped,
		 bootstrap_payload,verified_leadership_generation,created_at,updated_at)
		VALUES('aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa','failed',' literal 雪 ',
		 'not-an-ip','synthetic-wrapped-text','not JSON: 雪 🦊',9223372036854775807,
		 '109223372036854775807.999999999','109223372036854775808.000000001')`); err != nil {
		t.Fatal(err)
	}
	const bytesQuery = `SELECT json_array(id,state,primary_url,expected_primary_id,
		verified_primary_id,verified_primary_node_id,verified_leadership_generation,
		verified_primary_tls_spki_sha256,verified_primary_ca_fingerprint,preview_digest,
		local_node_ip,archive_id,ephemeral_private_key_wrapped,bootstrap_payload,
		node_credential_secret_id,cluster_key_id,initial_snapshot_hash,
		initial_snapshot_revision_id,initial_apply_job_id,failure_code,
		confirmed_at,created_at,updated_at) FROM enrollment_attempts ORDER BY id`
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
	runner := filepath.Clean(filepath.Join(filepath.Dir(source), "../../../../packages/db/src/sqlite-foundation/enrollment-attempt-read-record.bun.ts"))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bun, "--no-env-file", runner, path)
	cmd.Dir = t.TempDir()
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Go-migrated Bun enrollment-attempt reader: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "enrollment-attempt-read checks=") {
		t.Fatalf("missing Bun completion marker: %s", out)
	}
	t.Logf("%s", out)
	var after string
	if err := db.QueryRow(bytesQuery).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("reader fixture changed stored fields: before=%s after=%s", before, after)
	}
	var violations int
	if err := db.QueryRow(`SELECT count(*) FROM pragma_foreign_key_check`).Scan(&violations); err != nil {
		t.Fatal(err)
	}
	if violations != 0 {
		t.Fatalf("foreign key violations: %d", violations)
	}
}
