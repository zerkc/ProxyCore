package sqlitedb

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenPolicy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "literal ?#%&_pragma=foreign_keys(0).sqlite")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("literal filename was not created: %v", err)
	}
	ctx := context.Background()
	// Hold connections concurrently so every assertion tests a distinct connection.
	for i := 0; i < 3; i++ {
		conn, err := db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		var mode string
		var foreignKeys, timeout int
		if err := conn.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
			t.Fatal(err)
		}
		if err := conn.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
			t.Fatal(err)
		}
		if err := conn.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&timeout); err != nil {
			t.Fatal(err)
		}
		if mode != "wal" || foreignKeys != 1 || timeout != 5000 {
			t.Fatalf("connection %d: mode=%s foreign_keys=%d timeout=%d", i, mode, foreignKeys, timeout)
		}
	}
}

func TestOpenRejectsInvalidPaths(t *testing.T) {
	for _, path := range []string{"", "bad\x00path", t.TempDir(), filepath.Join(t.TempDir(), "missing", "db.sqlite")} {
		t.Run(path, func(t *testing.T) {
			db, err := Open(path)
			if err == nil || db != nil {
				t.Fatalf("expected nil handle and error, got %v, %v", db, err)
			}
		})
	}
}

func TestForeignKeysAndRollback(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("CREATE TABLE parent (id INTEGER PRIMARY KEY); CREATE TABLE child (id INTEGER REFERENCES parent(id))"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO child VALUES (1)"); err == nil {
		t.Fatal("foreign key violation accepted")
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec("INSERT INTO parent VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRow("SELECT count(*) FROM parent").Scan(&count); err != nil || count != 0 {
		t.Fatalf("rollback count=%d error=%v", count, err)
	}
}
