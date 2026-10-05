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

func TestSQLiteWorkerAppliedSnapshotReadRecordFixture(t *testing.T) {
	path := filepath.Join(t.TempDir(), "applied-snapshot-read.db")
	db, err := sqlitedb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := sqlitedb.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	const revision = "cccccccc-cccc-cccc-cccc-cccccccccccc"
	const job = "dddddddd-dddd-dddd-dddd-dddddddddddd"
	const id = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	const node = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	const clock = "109223372036854775807.999000000"
	// Synthetic FK parents only, with no actor or user fixtures.
	if _, err := db.Exec(`INSERT INTO config_revisions
		(id,revision_number,checksum,snapshot,created_at)
		VALUES(?,1,'synthetic-read-checksum','{"fixture":true}',?)`, revision, clock); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO apply_jobs
		(id,revision_id,target,correlation_id,created_at)
		VALUES(?,?,'coredns','synthetic-read-correlation',?)`, job, revision, clock); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO applied_snapshots
		(id,source_primary_id,leadership_generation,snapshot_version,replication_version,
		content_hash,snapshot_body,revision_id,status,apply_job_id,failure_code,applied_at,discarded_at)
		VALUES(?,?,9007199254740993,-2147483648,2147483647,'  hash 雪 🦊  ',X'00FF8041',
		?,'pending',?,'  result 雪  ',?,'109223372036854775808.001000000')`, id, node, revision, job, clock); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO applied_snapshots
		(id,source_primary_id,leadership_generation,snapshot_version,replication_version,
		content_hash,snapshot_body,applied_at) VALUES
		('eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee',?,0,0,0,'empty-body',X'',?),
		('11111111-1111-1111-1111-111111111111',?,0,0,0,'null-body',NULL,?)`, node, clock, node, clock); err != nil {
		t.Fatal(err)
	}
	const bodies = `SELECT json_group_array(json_array(content_hash,typeof(snapshot_body),
		length(snapshot_body),hex(snapshot_body))) FROM
		(SELECT * FROM applied_snapshots ORDER BY id)`
	var bodyBefore string
	if err := db.QueryRow(bodies).Scan(&bodyBefore); err != nil {
		t.Fatal(err)
	}
	const wantBodies = `[["null-body","null",null,""],["  hash 雪 🦊  ","blob",4,"00FF8041"],["empty-body","blob",0,""]]`
	if bodyBefore != wantBodies {
		t.Fatalf("native body storage oracle: got %q want %q", bodyBefore, wantBodies)
	}
	const raw = `SELECT json_array(id,source_primary_id,leadership_generation,
		snapshot_version,replication_version,content_hash,typeof(snapshot_body),
		length(snapshot_body),hex(snapshot_body),revision_id,status,apply_job_id,
		failure_code,applied_at,discarded_at) FROM applied_snapshots
		WHERE id='aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa'`
	var before string
	if err := db.QueryRow(raw).Scan(&before); err != nil {
		t.Fatal(err)
	}
	const want = `["aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa","bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb",9007199254740993,-2147483648,2147483647,"  hash 雪 🦊  ","blob",4,"00FF8041","cccccccc-cccc-cccc-cccc-cccccccccccc","pending","dddddddd-dddd-dddd-dddd-dddddddddddd","  result 雪  ","109223372036854775807.999000000","109223372036854775808.001000000"]`
	if before != want {
		diff := 0
		for diff < len(before) && diff < len(want) && before[diff] == want[diff] {
			diff++
		}
		t.Fatalf("native binary oracle: actual=%q expected=%q actual_len=%d expected_len=%d first_differing_byte_offset=%d", before, want, len(before), len(want), diff)
	}
	const parents = `SELECT json_array(
		(SELECT json_array(id,revision_number,checksum,snapshot,actor_user_id,created_at,source) FROM config_revisions),
		(SELECT json_array(id,revision_id,actor_user_id,target,status,correlation_id,created_at) FROM apply_jobs))`
	var parentBefore string
	if err := db.QueryRow(parents).Scan(&parentBefore); err != nil {
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
	runner := filepath.Clean(filepath.Join(filepath.Dir(source), "../../../../packages/db/src/sqlite-foundation/applied-snapshot-read-record.bun.ts"))
	runCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(runCtx, bun, "--no-env-file", runner, path)
	cmd.Dir = t.TempDir()
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Go-migrated Bun applied-snapshot reader: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "applied-snapshot-read checks=") {
		t.Fatalf("missing Bun completion marker: %s", out)
	}
	t.Logf("%s", out)
	var bodyAfter string
	if err := db.QueryRow(bodies).Scan(&bodyAfter); err != nil {
		t.Fatal(err)
	}
	if bodyAfter != bodyBefore {
		t.Fatalf("native body storage changed: %q", bodyAfter)
	}
	var after, parentAfter string
	if err := db.QueryRow(raw).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(parents).Scan(&parentAfter); err != nil {
		t.Fatal(err)
	}
	if before != after || parentBefore != parentAfter {
		t.Fatalf("native fields or FK parents changed: %s / %s", after, parentAfter)
	}
	var count, violations, parentCount int
	if err := db.QueryRow(`SELECT count(*) FROM applied_snapshots`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM pragma_foreign_key_check`).Scan(&violations); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT (SELECT count(*) FROM config_revisions)+(SELECT count(*) FROM apply_jobs)`).Scan(&parentCount); err != nil {
		t.Fatal(err)
	}
	if count != 3 || violations != 0 || parentCount != 2 {
		t.Fatalf("native counts: snapshots=%d violations=%d parents=%d", count, violations, parentCount)
	}
}
