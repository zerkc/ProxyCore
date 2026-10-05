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

func TestSQLiteWorkerSyncAttemptCreateRecordFixture(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sync-attempt-create.db")
	db, err := sqlitedb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := sqlitedb.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	// Synthetic FK parents, actor NULL and native ordinary/queued defaults.
	const revision = "cccccccc-cccc-cccc-cccc-cccccccccccc"
	const job = "dddddddd-dddd-dddd-dddd-dddddddddddd"
	const clock = "109223372036854775807.999000000"
	if _, err := db.Exec(`INSERT INTO config_revisions
		(id,revision_number,checksum,snapshot,created_at)
		VALUES(?, 1, 'synthetic-sync-checksum', '{"fixture":true}', ?)`, revision, clock); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO apply_jobs
		(id,revision_id,target,correlation_id,created_at)
		VALUES(?, ?, 'coredns', 'synthetic-sync-correlation', ?)`, job, revision, clock); err != nil {
		t.Fatal(err)
	}
	const rawFields = `SELECT json_array(id,node_id,"trigger",status,source_primary_id,
		leadership_generation,snapshot_version,replication_version,content_hash,
		revision_id,apply_job_id,result_code,started_at,finished_at,created_at,updated_at)
		FROM sync_attempts ORDER BY id`
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM sync_attempts`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("fixture must start empty")
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
	runner := filepath.Clean(filepath.Join(filepath.Dir(source), "../../../../packages/db/src/sqlite-foundation/sync-attempt-create-record.bun.ts"))
	runCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(runCtx, bun, "--no-env-file", runner, path)
	cmd.Dir = t.TempDir()
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Go-migrated Bun sync-attempt writer: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "sync-attempt-create checks=") {
		t.Fatalf("missing Bun completion marker: %s", out)
	}
	t.Logf("%s", out)
	rows, err := db.Query(rawFields)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var actual []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			t.Fatal(err)
		}
		actual = append(actual, value)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	const expected = `["aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa","bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb","manual","running","bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb",9007199254740992,-2147483648,2147483647,"  hash 雪 🦊  ","cccccccc-cccc-cccc-cccc-cccccccccccc","dddddddd-dddd-dddd-dddd-dddddddddddd","  result 雪  ","109223372036854775807.999000000","109223372036854775808.001000000","109223372036854775807.999000000","109223372036854775808.001000000"]`
	if len(actual) != 1 || actual[0] != expected {
		t.Fatalf("cross-engine all sixteen fields: got %v want [%s]", actual, expected)
	}
	var violations int
	if err := db.QueryRow(`SELECT count(*) FROM pragma_foreign_key_check`).Scan(&violations); err != nil {
		t.Fatal(err)
	}
	if violations != 0 {
		t.Fatalf("foreign key violations: %d", violations)
	}
	var parents int
	if err := db.QueryRow(`SELECT (SELECT count(*) FROM config_revisions) +
		(SELECT count(*) FROM apply_jobs)`).Scan(&parents); err != nil {
		t.Fatal(err)
	}
	if parents != 2 {
		t.Fatalf("synthetic parents changed: %d", parents)
	}
}
