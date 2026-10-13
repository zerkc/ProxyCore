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

func TestSQLiteWorkerEnrollmentGrantReadRecordFixture(t *testing.T) {
	path := filepath.Join(t.TempDir(), "enrollment-grant-read.db")
	db, err := sqlitedb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := sqlitedb.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	// Only synthetic, noncredential parent data. Native constraints remain enabled.
	const user = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	const token = "cccccccc-cccc-cccc-cccc-cccccccccccc"
	const attempt = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	if _, err := db.Exec(`INSERT INTO users
		(id,username,password_hash,role,created_at,updated_at)
		VALUES(?, 'grant-read-fixture-user', 'dummy-NONcredential-password-hash', 'owner', '0', '0')`, user); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO enrollment_tokens
		(id,token_selector,token_hash,hash_version,created_by_user_id,created_at,expires_at)
		VALUES(?, 'dummy-selector', 'dummy-hash', 'dummy-version', ?, '0', '2')`, token, user); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO enrollment_grants
		(attempt_id,token_id,installation_id,node_id,primary_id,primary_generation,
		verified_preview_digest,node_ephemeral_public_key,sealed_bootstrap_payload,
		payload_hash,created_at,expires_at)
		VALUES(?, ?, ?, ?, ?, 9007199254740993, '  digest 雪 🦊  ',
		'  public-key 雪  ', '  sealed-payload 雪  ', '  payload-hash 雪  ',
		'109223372036854775807.999000000', '109223372036854775808.001000000')`, attempt, token, user, user, user); err != nil {
		t.Fatal(err)
	}
	const rawFields = `SELECT json_array(attempt_id,token_id,installation_id,node_id,
		primary_id,primary_generation,verified_preview_digest,node_ephemeral_public_key,
		sealed_bootstrap_payload,payload_hash,created_at,expires_at)
		FROM enrollment_grants ORDER BY attempt_id`
	var before string
	if err := db.QueryRow(rawFields).Scan(&before); err != nil {
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
	runner := filepath.Clean(filepath.Join(filepath.Dir(source), "../../../../packages/db/src/sqlite-foundation/enrollment-grant-read-record.bun.ts"))
	runCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(runCtx, bun, "--no-env-file", runner, path)
	cmd.Dir = t.TempDir()
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Go-migrated Bun enrollment-grant reader: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "enrollment-grant-read checks=") {
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
	if len(actual) != 1 || actual[0] != before {
		t.Fatalf("cross-engine native fields changed: got %v want [%s]", actual, before)
	}
	var violations, parents int
	if err := db.QueryRow(`SELECT count(*) FROM pragma_foreign_key_check`).Scan(&violations); err != nil {
		t.Fatal(err)
	}
	if violations != 0 {
		t.Fatalf("foreign key violations: %d", violations)
	}
	if err := db.QueryRow(`SELECT count(*) FROM enrollment_tokens`).Scan(&parents); err != nil {
		t.Fatal(err)
	}
	if parents != 1 {
		t.Fatalf("caller-owned token fixtures changed: %d", parents)
	}
}
