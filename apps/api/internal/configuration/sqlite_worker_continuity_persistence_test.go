package configuration

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

func TestSQLiteWorkerContinuityPersistenceFixture(t *testing.T) {
	path := filepath.Join(t.TempDir(), "continuity-persistence.db")
	db, err := sqlitedb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := sqlitedb.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	const node = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	const revision = "cccccccc-cccc-cccc-cccc-cccccccccccc"
	const token = "dddddddd-dddd-dddd-dddd-dddddddddddd"
	const primary = "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee"
	const job = "ffffffff-ffff-ffff-ffff-ffffffffffff"
	const credential = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	const attempt = "11111111-1111-1111-1111-111111111111"
	// Synthetic fixture-only parents; no authentication or cryptographic work.
	seeds := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO users(id,username,password_hash,role,created_at,updated_at)
			VALUES(?,'continuity-fixture','dummy-NONcredential-hash','owner','0','0')`, []any{node}},
		{`INSERT INTO enrollment_tokens(id,token_selector,token_hash,hash_version,created_by_user_id,created_at,expires_at)
			VALUES(?,'dummy-selector','dummy-hash','dummy-version',?,'0','2')`, []any{token, node}},
		{`INSERT INTO node_credentials(id,node_id,credential_hash,hash_version,created_at)
			VALUES(?,?,'dummy-hash','dummy-version','0')`, []any{credential, node}},
		{`INSERT INTO enrolled_nodes(node_id,installation_id,primary_id,credential_id,enrolled_at,created_by_attempt_id)
			VALUES(?,?,?,?,'0',?)`, []any{node, credential, primary, credential, attempt}},
		{`INSERT INTO config_revisions(id,revision_number,checksum,snapshot,actor_user_id,created_at)
			VALUES(?,1,'fixture-only-checksum','{}',NULL,'0')`, []any{revision}},
		{`INSERT INTO apply_jobs(id,revision_id,actor_user_id,target,correlation_id,created_at)
			VALUES(?,?,NULL,'combined','fixture-only','0')`, []any{job, revision}},
	}
	for _, seed := range seeds {
		if _, err := db.Exec(seed.sql, seed.args...); err != nil {
			t.Fatal(err)
		}
	}
	// Capture every parent column, not just IDs/counts. No parent may be updated.
	parents := func() string {
		t.Helper()
		var parts []string
		for _, table := range []string{"users", "enrollment_tokens", "node_credentials", "enrolled_nodes", "config_revisions", "apply_jobs"} {
			var value string
			if err := db.QueryRow("SELECT json_group_array(json_object('row',json(t.row))) FROM (SELECT json_array(" +
				map[string]string{
					"users": "id,username,password_hash,role,active,password_change_required,created_at,updated_at",
					"enrollment_tokens": "id,token_selector,token_hash,hash_version,created_by_user_id,created_at,expires_at,consumed_at,consumed_by_attempt_id,revoked_at",
					"node_credentials": "id,node_id,credential_hash,hash_version,created_at,last_authenticated_at,revoked_at",
					"enrolled_nodes": "node_id,installation_id,primary_id,display_name,credential_id,enrolled_at,revoked_at,created_by_attempt_id",
					"config_revisions": "id,revision_number,checksum,snapshot,actor_user_id,created_at,applied_at,source,source_primary_id,source_node_id,source_revision_id,snapshot_content_hash,snapshot_version,replication_version,leadership_generation",
					"apply_jobs": "id,revision_id,actor_user_id,target,status,correlation_id,validation_output,apply_output,health_output,error_message,claimed_at,started_at,finished_at,created_at,source,source_primary_id,source_node_id,snapshot_content_hash,snapshot_version,replication_version,leadership_generation",
				}[table] + ") AS row FROM " + table + ") t").Scan(&value); err != nil {
				t.Fatal(err)
			}
			parts = append(parts, value)
		}
		return strings.Join(parts, "\n")
	}
	before := parents()
	bun, err := exec.LookPath("bun")
	if err != nil {
		t.Fatal("required Bun executable:", err)
	}
	bun, err = filepath.Abs(bun)
	if err != nil {
		t.Fatal(err)
	}
	_, source, _, _ := runtime.Caller(0)
	runner := filepath.Clean(filepath.Join(filepath.Dir(source), "../../../../packages/db/src/sqlite-foundation/continuity-persistence.bun.ts"))
	run := func(mode string) string {
		t.Helper()
		runCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(runCtx, bun, "--no-env-file", runner, path, mode)
		cmd.Dir = t.TempDir()
		cmd.WaitDelay = time.Second
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("Go-migrated Bun continuity persistence %s: %v\n%s", mode, err, out)
		}
		if !strings.Contains(string(out), "continuity-persistence checks=") {
			t.Fatalf("missing Bun completion marker: %s", out)
		}
		t.Logf("%s", out)
		for _, line := range strings.Split(string(out), "\n") {
			if strings.HasPrefix(line, "continuity-persistence durable=") {
				return line
			}
		}
		return ""
	}
	run("fake")
	durable := run("migrated")
	if durable == "" {
		t.Fatal("missing migrated durable marker")
	}
	if reopened := run("reopen"); reopened != durable {
		t.Fatalf("separate-process raw/projection reopen: actual=%q expected=%q", reopened, durable)
	}
	const raw = `SELECT json_array(
		(SELECT json_array(id,sync_enabled,consecutive_failures,enrollment_attempt_id,credential_id) FROM node_state),
		(SELECT json_array(id,state,bootstrap_payload,archive_id) FROM enrollment_attempts),
		(SELECT json_array(attempt_id,token_id,primary_generation,verified_preview_digest) FROM enrollment_grants),
		(SELECT json_array(node_id,revision_id,apply_job_id,status,leadership_generation) FROM sync_attempts),
		(SELECT json_group_array(json_array(typeof(snapshot_body),hex(snapshot_body),revision_id,apply_job_id,failure_code)) FROM (SELECT * FROM applied_snapshots ORDER BY id)),
		(SELECT json_array(node_id,content_hash,snapshot_version,replication_version,revision_id,leadership_generation,applied_at,received_at) FROM node_snapshot_acks))`
	var actual string
	if err := db.QueryRow(raw).Scan(&actual); err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf(`[["default",1,3,%q,null],[%q,"draft","AB",null],[%q,%q,9007199254740992,null],[%q,%q,%q,"queued",null],[["blob","",%q,%q,null],["blob","00FF8041",%q,%q,null],["null","",%q,%q,null]],[%q,"  hash 雪  ",3,4,%q,9007199254740992,"109223372036854775808.000000000","109223372036854775808.001000000"]]`,
		attempt, attempt, attempt, token, node, revision, job, revision, job, revision, job, revision, job, node, revision)
	if actual != want {
		t.Fatalf("native relationships/NULL/empty/binary oracle: actual=%q expected=%q", actual, want)
	}
	if after := parents(); after != before {
		t.Fatalf("parents changed: actual=%q expected=%q", after, before)
	}
	var counts string
	if err := db.QueryRow(`SELECT json_array(
		(SELECT count(*) FROM node_state),(SELECT count(*) FROM enrollment_attempts),
		(SELECT count(*) FROM enrollment_grants),(SELECT count(*) FROM sync_attempts),
		(SELECT count(*) FROM applied_snapshots),(SELECT count(*) FROM node_snapshot_acks),
		(SELECT count(*) FROM pragma_foreign_key_check))`).Scan(&counts); err != nil {
		t.Fatal(err)
	}
	if counts != "[1,1,1,1,3,1,0]" {
		t.Fatalf("durable counts/rollback absence: actual=%q expected=%q", counts, "[1,1,1,1,3,1,0]")
	}
}
