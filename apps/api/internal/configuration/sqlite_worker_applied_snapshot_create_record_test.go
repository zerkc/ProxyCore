package configuration

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

func TestSQLiteWorkerAppliedSnapshotCreateRecordFixture(t *testing.T) {
	path := filepath.Join(t.TempDir(), "applied-snapshot-create.db")
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
	var initial int
	if err := db.QueryRow(`SELECT count(*) FROM applied_snapshots`).Scan(&initial); err != nil {
		t.Fatal(err)
	}
	if initial != 0 {
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
	runner := filepath.Clean(filepath.Join(filepath.Dir(source), "../../../../packages/db/src/sqlite-foundation/applied-snapshot-create-record.bun.ts"))
	runCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(runCtx, bun, "--no-env-file", runner, path)
	cmd.Dir = t.TempDir()
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Go-migrated Bun applied-snapshot writer: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "applied-snapshot-create checks=") {
		t.Fatalf("missing Bun completion marker: %s", out)
	}
	t.Logf("%s", out)
	const raw = `SELECT json_array(id,source_primary_id,leadership_generation,
		snapshot_version,replication_version,content_hash,hex(snapshot_body),revision_id,
		status,apply_job_id,failure_code,applied_at,discarded_at)
		FROM applied_snapshots WHERE id='aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa'`
	var actual string
	if err := db.QueryRow(raw).Scan(&actual); err != nil {
		t.Fatal(err)
	}
	const expected = `["aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa","bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb",9007199254740992,-2147483648,2147483647,"  hash 雪 🦊  ","00FF8041","cccccccc-cccc-cccc-cccc-cccccccccccc","pending","dddddddd-dddd-dddd-dddd-dddddddddddd","  result 雪  ","109223372036854775807.999000000","109223372036854775808.001000000"]`
	if actual != expected {
		t.Fatalf("all thirteen fields: got %s want %s", actual, expected)
	}
	rows, err := db.Query(`SELECT content_hash,typeof(snapshot_body),length(snapshot_body),
		hex(snapshot_body) FROM applied_snapshots ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var bodies []string
	for rows.Next() {
		var hash, kind, hex string
		var length any
		if err := rows.Scan(&hash, &kind, &length, &hex); err != nil {
			t.Fatal(err)
		}
		bodies = append(bodies, fmt.Sprintf("%s|%s|%v|%s", hash, kind, length, hex))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := []string{"  hash 雪 🦊  |blob|4|00FF8041", "empty-body|blob|0|", "null-body|null|<nil>|"}
	if !reflect.DeepEqual(bodies, want) {
		t.Fatalf("raw body oracle: got %v want %v", bodies, want)
	}
	var violations int
	if err := db.QueryRow(`SELECT count(*) FROM pragma_foreign_key_check`).Scan(&violations); err != nil {
		t.Fatal(err)
	}
	if violations != 0 {
		t.Fatalf("foreign key violations: %d", violations)
	}
	var parents string
	if err := db.QueryRow(`SELECT json_array(
		(SELECT json_array(id,revision_number,checksum,snapshot,actor_user_id,created_at,source) FROM config_revisions),
		(SELECT json_array(id,revision_id,actor_user_id,target,status,correlation_id,created_at) FROM apply_jobs))`).Scan(&parents); err != nil {
			t.Fatal(err)
		}
	const wantParents = `[["cccccccc-cccc-cccc-cccc-cccccccccccc",1,"synthetic-sync-checksum","{\"fixture\":true}",null,"109223372036854775807.999000000","ordinary"],["dddddddd-dddd-dddd-dddd-dddddddddddd","cccccccc-cccc-cccc-cccc-cccccccccccc",null,"coredns","queued","synthetic-sync-correlation","109223372036854775807.999000000"]]`
	if parents != wantParents {
		t.Fatalf("synthetic parents changed: %s", parents)
	}
	var count int
	if err := db.QueryRow(`SELECT (SELECT count(*) FROM config_revisions) +
		(SELECT count(*) FROM apply_jobs)`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("synthetic parent count changed: %d", count)
	}
}
