package sqlitedb

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

type fixtureMigration struct {
	Version  int    `json:"version"`
	Name     string `json:"name"`
	File     string `json:"file"`
	Checksum string `json:"checksum"`
}

type migrationFixture struct {
	entries []fixtureMigration
	files   map[string]string
	raw     string
}

func (f migrationFixture) registry() []byte {
	if f.raw != "" {
		return []byte(f.raw)
	}
	raw, _ := json.Marshal(f.entries)
	return raw
}

func newMigrationFixture() migrationFixture {
	f := migrationFixture{files: map[string]string{
		"one.sql": "CREATE TABLE owned (id INTEGER PRIMARY KEY);",
		"two.sql": "CREATE TABLE related (id INTEGER REFERENCES owned(id));",
	}}
	for i, file := range []string{"one.sql", "two.sql"} {
		f.entries = append(f.entries, fixtureMigration{i + 1, "Fixture " + file, file, fmt.Sprintf("%x", sha256.Sum256([]byte(f.files[file])))})
	}
	return f
}

func migrationDB(t *testing.T) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "migrations.sqlite")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db, path
}

func fixtureDirectory(t *testing.T, f migrationFixture) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "registry.json"), f.registry(), 0600); err != nil {
		t.Fatal(err)
	}
	for file, text := range f.files {
		if err := os.WriteFile(filepath.Join(dir, file), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func bunMigrationCommand(t *testing.T, path, dir, mode string) *exec.Cmd {
	t.Helper()
	bun, err := exec.LookPath("bun")
	if err != nil {
		t.Fatal("Bun required:", err)
	}
	_, source, _, _ := runtime.Caller(0)
	script := filepath.Clean(filepath.Join(filepath.Dir(source), "../../../..", "packages/db/src/sqlite-foundation/migrate.bun.ts"))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, bun, "--no-env-file", script, path, dir, mode)
	cmd.Dir = filepath.Dir(path)
	return cmd
}

func runMigrationFixture(t *testing.T, engine string, db *sql.DB, path string, f migrationFixture) error {
	t.Helper()
	if engine == "go" {
		return migrateAuthority(context.Background(), db, f.registry(), func(file string) ([]byte, error) {
			text, ok := f.files[file]
			if !ok {
				return nil, fmt.Errorf("missing fixture SQL")
			}
			return []byte(text), nil
		})
	}
	output, err := bunMigrationCommand(t, path, fixtureDirectory(t, f), "fixture").CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, output)
	}
	return nil
}

func TestMigrationProductionAuthority(t *testing.T) {
	for _, engine := range []string{"go", "bun"} {
		t.Run(engine, func(t *testing.T) {
			db, path := migrationDB(t)
			apply := func() error {
				if engine == "go" {
					return Migrate(context.Background(), db)
				}
				output, err := bunMigrationCommand(t, path, "-", "default").CombinedOutput()
				if err != nil {
					return fmt.Errorf("%w: %s", err, output)
				}
				return nil
			}
			for i := 0; i < 2; i++ {
				if err := apply(); err != nil {
					t.Fatal(err)
				}
			}
			var count int
			if err := db.QueryRow("SELECT count(*) FROM proxycore_schema_migrations").Scan(&count); err != nil || count != 8 {
				t.Fatalf("production registry ledger count=%d error=%v", count, err)
			}
			if _, err := db.Exec("CREATE TABLE unrelated(id INTEGER)"); err != nil {
				t.Fatal(err)
			}
			if err := apply(); err == nil {
				t.Fatal("production authority adopted an unrelated table")
			}
		})
	}
}

type migrationCase struct {
	name    string
	before  string
	change  func(*migrationFixture)
	after   string
	prefix  bool
	fail    bool
	capture bool
}

func structCase(name string, change func(*migrationFixture)) migrationCase {
	return migrationCase{name: name, change: change, fail: true}
}

func TestMigrationCases(t *testing.T) {
	cases := []migrationCase{
		{name: "success"},
		{name: "escaped canonical", change: func(f *migrationFixture) {
			f.raw = " \n" + strings.Replace(string(f.registry()), `"version":`, `"ver\u0073ion":`, 1) + "\n"
		}},
		{name: "append to prefix", prefix: true},
		{name: "null registry", change: func(f *migrationFixture) { f.entries = nil }, fail: true},
		{name: "duplicate name", change: func(f *migrationFixture) { f.entries[1].Name = f.entries[0].Name }, fail: true},
		{name: "exact byte checksum", change: func(f *migrationFixture) { f.files["one.sql"] += "\n" }, fail: true},
		{name: "checksum", change: func(f *migrationFixture) { f.entries[0].Checksum = fmt.Sprintf("%064d", 0) }, fail: true},
		{name: "missing SQL", change: func(f *migrationFixture) { delete(f.files, "two.sql") }, fail: true},
		{name: "registry hole", change: func(f *migrationFixture) { f.entries[1].Version = 3 }, fail: true},
		{name: "duplicate version", change: func(f *migrationFixture) { f.entries[1].Version = 1 }, fail: true},
		{name: "unsafe filename", change: func(f *migrationFixture) { f.entries[0].File = "../one.sql" }, fail: true},
		{name: "bad pending SQL", change: func(f *migrationFixture) {
			f.files["two.sql"] += " INVALID SQL;"
			f.entries[1].Checksum = fmt.Sprintf("%x", sha256.Sum256([]byte(f.files["two.sql"])))
		}, fail: true},
		{name: "rollback pending after prefix", prefix: true, change: func(f *migrationFixture) {
			f.files["two.sql"] = "DELETE FROM owned; " + f.files["two.sql"] + " INVALID SQL;"
			f.entries[1].Checksum = fmt.Sprintf("%x", sha256.Sum256([]byte(f.files["two.sql"])))
		}, fail: true},
		{name: "transaction escape", change: func(f *migrationFixture) {
			f.files["two.sql"] = "COMMIT; CREATE TABLE escaped(id INTEGER); INVALID SQL;"
			f.entries[1].Checksum = fmt.Sprintf("%x", sha256.Sum256([]byte(f.files["two.sql"])))
		}, fail: true},
		{name: "unknown table", before: "CREATE TABLE foreign_table(id INTEGER)", fail: true},
		{name: "malformed ledger", before: "CREATE TABLE proxycore_schema_migrations(version INTEGER)", fail: true},
		{name: "wrong ledger collation", before: strings.Replace(ledgerDDL, "COLLATE BINARY", "COLLATE NOCASE", 1), fail: true},
		{name: "malformed applied clock", after: "UPDATE proxycore_schema_migrations SET applied_at = '0' WHERE version = 1", fail: true},
		{name: "blob checksum", after: "UPDATE proxycore_schema_migrations SET checksum = CAST(checksum AS BLOB)", fail: true},
		{name: "blob name", after: "UPDATE proxycore_schema_migrations SET name = CAST(name AS BLOB)", fail: true},
		{name: "blob clock", after: "UPDATE proxycore_schema_migrations SET applied_at = CAST(applied_at AS BLOB)", fail: true},
		{name: "blob version in non-rowid ledger", before: strings.Replace(ledgerDDL, "version INTEGER", "version INT", 1) + "; INSERT INTO proxycore_schema_migrations VALUES (CAST('1' AS BLOB), 'name', 'hash', 'clock'); CREATE TABLE owned(id INTEGER); INSERT INTO owned VALUES(42)", capture: true, fail: true},
		{name: "changed applied hash", after: "UPDATE proxycore_schema_migrations SET checksum = 'changed' WHERE version = 1", fail: true},
		{name: "changed applied name", after: "UPDATE proxycore_schema_migrations SET name = 'changed' WHERE version = 1", fail: true},
		{name: "future applied version", after: "UPDATE proxycore_schema_migrations SET version = 3 WHERE version = 2", fail: true},
		{name: "applied hole", after: "DELETE FROM proxycore_schema_migrations WHERE version = 1", fail: true},
		{name: "unexpected later table", after: "CREATE TABLE foreign_table(id INTEGER)", fail: true},
	}
	for _, key := range []string{"version", "name", "file", "checksum"} {
		shadow := "99"
		if key != "version" {
			shadow = `"shadow"`
		}
		cases = append(cases, structCase(key+" duplicate", func(f *migrationFixture) {
			f.raw = strings.Replace(string(f.registry()), `"`+key+`":`, `"`+key+`":`+shadow+`,"`+key+`":`, 1)
		}), structCase(key+" alias", func(f *migrationFixture) {
			f.raw = strings.Replace(string(f.registry()), `"`+key+`":`, `"`+strings.ToUpper(key)+`":`, 1)
		}))
	}
	cases = append(cases,
		structCase("escaped duplicate", func(f *migrationFixture) {
			f.raw = strings.Replace(string(f.registry()), `"version":`, `"vers\u0069on":99,"version":`, 1)
		}),
		structCase("nested value", func(f *migrationFixture) {
			f.raw = strings.Replace(string(f.registry()), `"version":1`, `"version":{"name":"quoted } [ \" text"}`, 1)
		}),
		structCase("trailing JSON", func(f *migrationFixture) { f.raw = string(f.registry()) + " []" }),
		structCase("malformed JSON", func(f *migrationFixture) { f.raw = strings.TrimSuffix(string(f.registry()), "]") }),
		structCase("unknown member", func(f *migrationFixture) {
			f.raw = strings.Replace(string(f.registry()), `"version":`, `"extra":0,"version":`, 1)
		}),
		structCase("escaped case alias", func(f *migrationFixture) {
			f.raw = strings.Replace(string(f.registry()), `"version":`, `"\u0056ERSION":`, 1)
		}),
		structCase("decimal version", func(f *migrationFixture) {
			f.raw = strings.Replace(string(f.registry()), `"version":1`, `"version":1.0`, 1)
		}),
		structCase("exponent version", func(f *migrationFixture) {
			f.raw = strings.Replace(string(f.registry()), `"version":1`, `"version":1e0`, 1)
		}),
	)
	for _, engine := range []string{"go", "bun"} {
		for _, c := range cases {
			t.Run(engine+"/"+c.name, func(t *testing.T) {
				db, path := migrationDB(t)
				f := newMigrationFixture()
				if c.before != "" {
					if _, err := db.Exec(c.before); err != nil {
						t.Fatal(err)
					}
				}
				if c.after != "" {
					if err := runMigrationFixture(t, engine, db, path, f); err != nil {
						t.Fatal(err)
					}
					if _, err := db.Exec(c.after); err != nil {
						t.Fatal(err)
					}
				}
				if c.prefix {
					prefix := f
					prefix.entries = f.entries[:1]
					if err := runMigrationFixture(t, engine, db, path, prefix); err != nil {
						t.Fatal(err)
					}
				}
				if c.prefix || c.after != "" {
					if _, err := db.Exec("INSERT INTO owned VALUES (42)"); err != nil {
						t.Fatal(err)
					}
				}
				if c.change != nil {
					c.change(&f)
				}
				var dataBefore []string
				if c.prefix || c.after != "" || c.capture {
					dataBefore = migrationData(t, db)
				}
				var before sql.NullString
				if err := db.QueryRow("SELECT group_concat(sql,'|') FROM (SELECT sql FROM sqlite_schema ORDER BY name)").Scan(&before); err != nil {
					t.Fatal(err)
				}
				err := runMigrationFixture(t, engine, db, path, f)
				if (err != nil) != c.fail {
					t.Fatalf("expected failure=%v, error=%v", c.fail, err)
				}
				if !c.fail {
					var first string
					if err := db.QueryRow("SELECT min(applied_at) FROM proxycore_schema_migrations").Scan(&first); err != nil {
						t.Fatal(err)
					}
					if err := runMigrationFixture(t, engine, db, path, f); err != nil {
						t.Fatal(err)
					}
					var count, clocks int
					var repeated string
					if err := db.QueryRow("SELECT count(*), count(DISTINCT applied_at), min(applied_at) FROM proxycore_schema_migrations").Scan(&count, &clocks, &repeated); err != nil || count != 2 || (!c.prefix && clocks != 1) || repeated != first {
						t.Fatalf("ledger count=%d clocks=%d repeated=%q first=%q error=%v", count, clocks, repeated, first, err)
					}
				} else {
					var after sql.NullString
					if err := db.QueryRow("SELECT group_concat(sql,'|') FROM (SELECT sql FROM sqlite_schema ORDER BY name)").Scan(&after); err != nil || after != before {
						t.Fatalf("failed migration changed schema: before=%v after=%v error=%v", before, after, err)
					}
					if dataBefore != nil && !reflect.DeepEqual(dataBefore, migrationData(t, db)) {
						t.Fatal("failed migration changed ledger rows or product data")
					}
				}
			})
		}
	}
}

func migrationData(t *testing.T, db *sql.DB) []string {
	t.Helper()
	var state []string
	for _, query := range []string{
		"SELECT coalesce(group_concat(quote(version)||':'||quote(name)||':'||quote(checksum)||':'||quote(applied_at),'|'),'') FROM (SELECT * FROM proxycore_schema_migrations ORDER BY version)",
		"SELECT coalesce(group_concat(quote(id),'|'),'') FROM (SELECT id FROM owned ORDER BY id)",
	} {
		var value string
		if err := db.QueryRow(query).Scan(&value); err != nil {
			t.Fatal(err)
		}
		state = append(state, value)
	}
	return state
}

func TestMigrationCancelledCleanup(t *testing.T) {
	db, _ := migrationDB(t)
	db.SetMaxOpenConns(1)
	f := newMigrationFixture()
	f.entries = f.entries[:1]
	f.files["one.sql"] = "CREATE TABLE owned(id INTEGER); WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x < 1000000000) INSERT INTO owned SELECT x FROM n;"
	f.entries[0].Checksum = fmt.Sprintf("%x", sha256.Sum256([]byte(f.files["one.sql"])))
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := migrateAuthority(ctx, db, f.registry(), func(file string) ([]byte, error) { return []byte(f.files[file]), nil })
	if err == nil || ctx.Err() == nil {
		t.Fatalf("expected cancellation during migration: error=%v context=%v", err, ctx.Err())
	}
	if _, err := db.Exec("BEGIN IMMEDIATE; ROLLBACK"); err != nil {
		t.Fatalf("pooled connection retained transaction: %v", err)
	}
	var count int
	if err := db.QueryRow("SELECT count(*) FROM main.sqlite_schema").Scan(&count); err != nil || count != 0 {
		t.Fatalf("cancelled migration retained schema: count=%d error=%v", count, err)
	}
}

func TestMigrationTemporarySchema(t *testing.T) {
	db, path := migrationDB(t)
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("CREATE TEMP TABLE foreign_table(id INTEGER)"); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(context.Background(), db); err == nil {
		t.Fatal("Go adopted temporary foreign schema")
	}
	if output, err := bunMigrationCommand(t, path, "-", "temporary").CombinedOutput(); err == nil {
		t.Fatalf("Bun adopted temporary foreign schema: %s", output)
	}
	var count int
	if err := db.QueryRow("SELECT count(*) FROM main.sqlite_schema").Scan(&count); err != nil || count != 0 {
		t.Fatalf("temporary-schema rejection mutated main: count=%d error=%v", count, err)
	}
}

func TestMigrationCrossLanguageContention(t *testing.T) {
	db, path := migrationDB(t)
	f := newMigrationFixture()
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	defer conn.ExecContext(context.Background(), "ROLLBACK")
	cmd := bunMigrationCommand(t, path, fixtureDirectory(t, f), "ready")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() || scanner.Text() != "ready" {
		t.Fatal("Bun did not reach lock boundary")
	}
	goResult := make(chan error, 1)
	bunResult := make(chan error, 1)
	go func() { goResult <- runMigrationFixture(t, "go", db, path, f) }()
	go func() { bunResult <- cmd.Wait() }()
	select {
	case err := <-goResult:
		t.Fatalf("Go escaped held write lock: %v", err)
	case err := <-bunResult:
		t.Fatalf("Bun escaped held write lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if _, err := conn.ExecContext(context.Background(), "COMMIT"); err != nil {
		t.Fatal(err)
	}
	if err := <-goResult; err != nil {
		t.Fatal(err)
	}
	if err := <-bunResult; err != nil {
		t.Fatal(err)
	}
	if err := runMigrationFixture(t, "go", db, path, f); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRow("SELECT count(*) FROM proxycore_schema_migrations").Scan(&count); err != nil || count != 2 {
		t.Fatalf("concurrent ledger count=%d error=%v", count, err)
	}
}
