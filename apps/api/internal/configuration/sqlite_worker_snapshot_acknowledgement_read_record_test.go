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

func TestSQLiteWorkerSnapshotAcknowledgementReadRecordFixture(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snapshot-acknowledgement-read.db")
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
	const credential = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	const revision = "cccccccc-cccc-cccc-cccc-cccccccccccc"
	// Fixture-only dummy hashes and UUIDs; no authentication or cryptography.
	if _, err := db.Exec(`INSERT INTO node_credentials
		(id,node_id,credential_hash,hash_version,created_at)
		VALUES(?,?,'fixture-only-dummy-hash','fixture-only-dummy-version','0')`, credential, node); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO enrolled_nodes
		(node_id,installation_id,primary_id,credential_id,enrolled_at,created_by_attempt_id)
		VALUES(?,'dddddddd-dddd-dddd-dddd-dddddddddddd',
		'eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee',?,'0',
		'11111111-1111-1111-1111-111111111111')`, node, credential); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO config_revisions
		(id,revision_number,checksum,snapshot,actor_user_id,created_at)
		VALUES(?,1,'fixture-only-ack-read-checksum','{}',NULL,'0')`, revision); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO node_snapshot_acks
		(node_id,content_hash,snapshot_version,replication_version,revision_id,
		leadership_generation,applied_at,received_at) VALUES
		(?,'  hash 雪 🦊  ',-2147483648,2147483647,?,9007199254740993,
		'109223372036854775807.999999999','109223372036854775808.001999999'),
		(?,'',0,0,?,0,'0','2')`, node, revision, node, revision); err != nil {
		t.Fatal(err)
	}
	const raw = `SELECT json_array(node_id,content_hash,snapshot_version,
		replication_version,revision_id,leadership_generation,applied_at,received_at)
		FROM node_snapshot_acks WHERE content_hash='  hash 雪 🦊  '`
	var before string
	if err := db.QueryRow(raw).Scan(&before); err != nil {
		t.Fatal(err)
	}
	const want = `["bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb","  hash 雪 🦊  ",-2147483648,2147483647,"cccccccc-cccc-cccc-cccc-cccccccccccc",9007199254740993,"109223372036854775807.999999999","109223372036854775808.001999999"]`
	if before != want {
		diff := 0
		for diff < len(before) && diff < len(want) && before[diff] == want[diff] {
			diff++
		}
		t.Fatalf("native eight-field oracle: actual=%q expected=%q actual_len=%d expected_len=%d first_differing_byte_offset=%d", before, want, len(before), len(want), diff)
	}
	const parents = `SELECT json_array(
		(SELECT json_array(id,node_id,credential_hash,hash_version,created_at,last_authenticated_at,revoked_at) FROM node_credentials),
		(SELECT json_array(node_id,installation_id,primary_id,display_name,credential_id,enrolled_at,revoked_at,created_by_attempt_id) FROM enrolled_nodes),
		(SELECT json_array(id,revision_number,checksum,snapshot,actor_user_id,created_at,applied_at,source,source_primary_id,source_node_id,snapshot_content_hash,snapshot_version,replication_version,leadership_generation) FROM config_revisions))`
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
	runner := filepath.Clean(filepath.Join(filepath.Dir(source), "../../../../packages/db/src/sqlite-foundation/snapshot-acknowledgement-read-record.bun.ts"))
	runCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(runCtx, bun, "--no-env-file", runner, path)
	cmd.Dir = t.TempDir()
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Go-migrated Bun snapshot acknowledgement reader: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "snapshot-acknowledgement-read checks=") {
		t.Fatalf("missing Bun completion marker: %s", out)
	}
	t.Logf("%s", out)
	var after, parentAfter string
	if err := db.QueryRow(raw).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(parents).Scan(&parentAfter); err != nil {
		t.Fatal(err)
	}
	if before != after || parentBefore != parentAfter {
		t.Fatalf("native acknowledgement or three FK parents changed: %q / %q", after, parentAfter)
	}
	var count, violations, parentCount int
	if err := db.QueryRow(`SELECT count(*) FROM node_snapshot_acks`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM pragma_foreign_key_check`).Scan(&violations); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT (SELECT count(*) FROM node_credentials)+
		(SELECT count(*) FROM enrolled_nodes)+(SELECT count(*) FROM config_revisions)`).Scan(&parentCount); err != nil {
		t.Fatal(err)
	}
	if count != 2 || violations != 0 || parentCount != 3 {
		t.Fatalf("native counts: acknowledgements=%d violations=%d parents=%d", count, violations, parentCount)
	}
}
