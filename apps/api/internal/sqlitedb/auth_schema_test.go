package sqlitedb

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const authJSON = `{"n":922337203685477580812345,"text":"雪🦊"}`

func authBun(t *testing.T, path, mode string) {
	t.Helper()
	bun, err := exec.LookPath("bun")
	writeOK(t, err)
	bun, err = filepath.Abs(bun)
	writeOK(t, err)
	_, source, _, _ := runtime.Caller(0)
	script := filepath.Clean(filepath.Join(filepath.Dir(source), "../../../..", "packages/db/src/sqlite-foundation/auth-schema.bun.ts"))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bun, "--no-env-file", script, path, mode)
	cmd.Dir = filepath.Dir(path)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Bun auth: %v\n%s", err, output)
	}
}

func authInsert(t *testing.T, db *sql.DB) {
	t.Helper()
	calls := 0
	writer := NewWriter(db, func() FiniteTimestamp {
		calls++
		return FiniteTimestamp{Seconds: -1, Nanos: 123}
	})
	writeOK(t, writer.Write(context.Background(), func(v WriteValue) error {
		user, err := v.NewUUID()
		writeOK(t, err)
		session, err := v.NewUUID()
		writeOK(t, err)
		audit, err := v.NewUUID()
		writeOK(t, err)
		_, err = v.Conn().ExecContext(v.Context(), "INSERT INTO users(id,username,password_hash,role,created_at,updated_at) VALUES(?, 'go', '$hash雪', 'owner', ?, ?)", user, v.Timestamp(), v.Timestamp())
		writeOK(t, err)
		_, err = v.Conn().ExecContext(v.Context(), "INSERT INTO sessions(id,user_id,token_hash,expires_at,created_at) VALUES(?, ?, 'go-token', '2', ?)", session, user, v.Timestamp())
		writeOK(t, err)
		_, err = v.Conn().ExecContext(v.Context(), "INSERT INTO audit_events(id,actor_user_id,action,resource_type,before_value,after_value,correlation_id,result,created_at) VALUES(?, ?, 'write', 'user', NULL, ?, 'go', 'ok', ?)", audit, user, authJSON, v.Timestamp())
		return err
	}))
	if calls != 1 {
		t.Fatalf("clock captures=%d", calls)
	}
}

func authVerify(t *testing.T, db *sql.DB, count int) {
	t.Helper()
	for _, table := range []string{"users", "sessions", "audit_events", "proxycore_schema_migrations"} {
		want := count
		if table == "proxycore_schema_migrations" {
			want = 9
		}
		var n int
		writeOK(t, db.QueryRow("SELECT count(*) FROM "+table).Scan(&n))
		if n != want {
			t.Fatalf("%s rows=%d want=%d", table, n, want)
		}
	}
	var n int
	writeOK(t, db.QueryRow("SELECT count(*) FROM users u JOIN sessions s ON s.user_id=u.id JOIN audit_events a ON a.actor_user_id=u.id WHERE u.created_at=u.updated_at AND u.created_at=s.created_at AND u.created_at=a.created_at AND u.active=1 AND u.password_change_required=0 AND u.password_hash='$hash雪' AND s.expires_at='2' AND s.revoked_at IS NULL AND s.last_seen_at IS NULL AND a.before_value IS NULL AND a.after_value=?", authJSON).Scan(&n))
	if n != count {
		t.Fatalf("shared clocks/raw values/defaults=%d", n)
	}
}

func TestAuthSharedMigration(t *testing.T) {
	if testing.Short() {
		t.Skip("Bun subprocess integration")
	}
	for _, first := range []string{"go", "bun"} {
		t.Run(first, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "auth.sqlite")
			if first == "bun" {
				authBun(t, path, "insert")
			}
			db, err := Open(path)
			writeOK(t, err)
			defer func() { db.Close() }()
			writeOK(t, Migrate(context.Background(), db))
			authInsert(t, db)
			writeOK(t, db.Close())
			if first == "go" {
				authBun(t, path, "insert")
			} else {
				authBun(t, path, "verify")
			}
			db, err = Open(path)
			writeOK(t, err)
			writeOK(t, Migrate(context.Background(), db))
			authVerify(t, db, 2)
			writeOK(t, Migrate(context.Background(), db))
		})
	}
}

func TestAuthPristineAndChecksum(t *testing.T) {
	db, _ := migrationDB(t)
	writeOK(t, Migrate(context.Background(), db))
	for _, table := range []string{"users", "sessions", "audit_events"} {
		var n int
		writeOK(t, db.QueryRow("SELECT count(*) FROM "+table).Scan(&n))
		if n != 0 {
			t.Fatalf("pristine %s rows=%d", table, n)
		}
	}
	body, err := migrationAssets.ReadFile("migrations/001_auth.sql")
	writeOK(t, err)
	f := migrationFixture{
		entries: []fixtureMigration{{1, "auth_session_audit", "001_auth.sql", fmt.Sprintf("%x", sha256.Sum256(body))}},
		files:   map[string]string{"001_auth.sql": string(body) + "\n"},
	}
	for _, engine := range []string{"go", "bun"} {
		t.Run(engine, func(t *testing.T) {
			if testing.Short() && engine == "bun" {
				t.Skip("Bun subprocess integration")
			}
			db, path := migrationDB(t)
			if err := runMigrationFixture(t, engine, db, path, f); err == nil {
				t.Fatal("auth SQL exact-byte checksum change accepted")
			}
		})
	}
}

func TestAuthConstraints(t *testing.T) {
	db, _ := migrationDB(t)
	writeOK(t, Migrate(context.Background(), db))
	authInsert(t, db)
	for table, columns := range map[string][]string{
		"users":        {"id", "username", "password_hash", "role", "created_at", "updated_at"},
		"sessions":     {"id", "user_id", "token_hash", "expires_at", "revoked_at", "last_seen_at", "created_at"},
		"audit_events": {"id", "actor_user_id", "action", "resource_type", "resource_id", "before_value", "after_value", "correlation_id", "result", "created_at"},
	} {
		for _, column := range columns {
			for _, value := range []string{"coalesce(" + column + ",'0') || char(0) || 'suffix'", "CAST(coalesce(" + column + ",'null') AS BLOB)"} {
				if _, err := db.Exec("UPDATE " + table + " SET " + column + "=" + value); err == nil {
					t.Errorf("%s.%s accepted %s", table, column, value)
				}
			}
		}
	}
	for _, assignment := range []string{
		"role='admin'", "active=2", "password_change_required=-1", "id=NULL", "id='bad'",
		"active=1.5",
		"id='AAAAAAAA-0000-0000-0000-000000000000'", "created_at=NULL", "updated_at='yesterday'",
		"created_at='118446744073709551616.000000000'", "created_at='100000000000000000000.00000000x'",
	} {
		t.Run(assignment, func(t *testing.T) {
			if _, err := db.Exec("UPDATE users SET " + assignment); err == nil {
				t.Fatal("invalid value accepted")
			}
		})
	}
	for _, statement := range []string{
		"INSERT INTO users(username,password_hash,role,created_at,updated_at) VALUES('missing','$h','owner','0','2')",
		"INSERT INTO users(id,username,password_hash,role) VALUES('00000000-0000-0000-0000-000000000001','missing','$h','owner')",
		"UPDATE sessions SET user_id='00000000-0000-0000-0000-000000000001'",
		"UPDATE sessions SET id=NULL", "UPDATE sessions SET expires_at=NULL", "UPDATE sessions SET revoked_at='bad'",
		"INSERT INTO sessions(user_id,token_hash,expires_at,created_at) SELECT id,'missing','0','2' FROM users",
		"INSERT INTO audit_events(action,resource_type,correlation_id,result,created_at) VALUES('write','user','missing','ok','0')",
		"UPDATE audit_events SET after_value='{'", "UPDATE sessions SET created_at=NULL",
		"UPDATE audit_events SET actor_user_id='00000000-0000-0000-0000-000000000001'",
		"UPDATE audit_events SET before_value='{'", "UPDATE audit_events SET id=NULL", "UPDATE audit_events SET created_at=NULL",
		"INSERT INTO users SELECT '00000000-0000-0000-0000-000000000001',username,password_hash,role,active,password_change_required,created_at,updated_at FROM users",
		"INSERT INTO sessions SELECT '00000000-0000-0000-0000-000000000001',user_id,token_hash,expires_at,revoked_at,last_seen_at,created_at FROM sessions",
		"INSERT INTO users SELECT '00000000-0000-0000-0000-000000000001',CAST(username AS BLOB),password_hash,role,active,password_change_required,created_at,updated_at FROM users",
		"INSERT INTO sessions SELECT '00000000-0000-0000-0000-000000000001',user_id,CAST(token_hash AS BLOB),expires_at,revoked_at,last_seen_at,created_at FROM sessions",
		"DELETE FROM users",
	} {
		t.Run(statement, func(t *testing.T) {
			if _, err := db.Exec(statement); err == nil {
				t.Fatal("constraint not enforced")
			}
		})
	}
	_, err := db.Exec("UPDATE users SET role='operator',created_at='0',updated_at='2'; UPDATE sessions SET revoked_at='0',last_seen_at='2'; UPDATE audit_events SET before_value='null'")
	writeOK(t, err)
	var before, after string
	writeOK(t, db.QueryRow("SELECT before_value,after_value FROM audit_events").Scan(&before, &after))
	if before != "null" || after != authJSON {
		t.Fatal("JSON text changed")
	}
	_, err = db.Exec("DELETE FROM audit_events; DELETE FROM users")
	writeOK(t, err)
	var n int
	writeOK(t, db.QueryRow("SELECT count(*) FROM sessions").Scan(&n))
	if n != 0 {
		t.Fatal("session cascade missing")
	}
	_, err = db.Exec("DROP INDEX users_username_idx")
	writeOK(t, err)
	if err := Migrate(context.Background(), db); err == nil || !strings.Contains(err.Error(), "schema") {
		t.Fatalf("schema drift accepted: %v", err)
	}
}
