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

func TestSQLiteWorkerEnrollmentGrantCreateRecordFixture(t *testing.T) {
	path := filepath.Join(t.TempDir(), "enrollment-grant-create.db")
	db, err := sqlitedb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := sqlitedb.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	// Synthetic noncredential hashes/selectors only; real parent constraints stay on.
	const user = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	if _, err := db.Exec(`INSERT INTO users
		(id,username,password_hash,role,created_at,updated_at)
		VALUES(?, 'grant-fixture-user', 'dummy-NONcredential-password-hash', 'owner', '0', '0')`, user); err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{
		"cccccccc-cccc-cccc-cccc-cccccccccccc",
		"dddddddd-dddd-dddd-dddd-dddddddddddd",
		"eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee",
	} {
		if _, err := db.Exec(`INSERT INTO enrollment_tokens
			(id,token_selector,token_hash,hash_version,created_by_user_id,created_at,expires_at)
			VALUES(?, ?, 'dummy-hash', 'dummy-version', ?, '0', '2')`, token, "dummy-selector-"+token, user); err != nil {
			t.Fatal(err)
		}
	}
	const rawFields = `SELECT json_array(attempt_id,token_id,installation_id,node_id,
		primary_id,primary_generation,verified_preview_digest,node_ephemeral_public_key,
		sealed_bootstrap_payload,payload_hash,created_at,expires_at)
		FROM enrollment_grants ORDER BY attempt_id`
	rowsBefore, err := db.Query(rawFields)
	if err != nil {
		t.Fatal(err)
	}
	if rowsBefore.Next() {
		t.Fatal("new fixture unexpectedly contains grants")
	}
	if err := rowsBefore.Err(); err != nil {
		t.Fatal(err)
	}
	rowsBefore.Close()
	bun, err := exec.LookPath("bun")
	if err != nil {
		t.Fatal("required Bun executable:", err)
	}
	bun, err = filepath.Abs(bun)
	if err != nil {
		t.Fatal(err)
	}
	_, source, _, _ := runtime.Caller(0)
	runner := filepath.Clean(filepath.Join(filepath.Dir(source), "../../../../packages/db/src/sqlite-foundation/enrollment-grant-create-record.bun.ts"))
	runCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(runCtx, bun, "--no-env-file", runner, path)
	cmd.Dir = t.TempDir()
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Go-migrated Bun enrollment-grant writer: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "enrollment-grant-create checks=") {
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
	const expected = `["aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa","cccccccc-cccc-cccc-cccc-cccccccccccc","bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb","bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb","bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb",9007199254740992,"  digest 雪 🦊  ","  public-key 雪  ","  sealed-payload 雪  ","  payload-hash 雪  ","109223372036854775807.999000000","109223372036854775808.001000000"]`
	if len(actual) != 1 || actual[0] != expected {
		t.Fatalf("cross-engine raw fields: got %v want [%s]", actual, expected)
	}
	var violations int
	if err := db.QueryRow(`SELECT count(*) FROM pragma_foreign_key_check`).Scan(&violations); err != nil {
		t.Fatal(err)
	}
	if violations != 0 {
		t.Fatalf("foreign key violations: %d", violations)
	}
	var parents int
	if err := db.QueryRow(`SELECT count(*) FROM enrollment_tokens`).Scan(&parents); err != nil {
		t.Fatal(err)
	}
	if parents != 3 {
		t.Fatalf("caller-owned token fixtures changed: %d", parents)
	}
}
