package sqlitedb

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func snapshotSource(t *testing.T) (*sql.DB, string) {
	t.Helper()
	db, path := migrationDB(t)
	writeOK(t, Migrate(context.Background(), db))
	_, err := db.Exec("CREATE TABLE snap_rows(id INTEGER PRIMARY KEY, v TEXT NOT NULL)")
	writeOK(t, err)
	for i := 0; i < 20; i++ {
		_, err = db.Exec("INSERT INTO snap_rows(v) VALUES (?)", "synthetic")
		writeOK(t, err)
	}
	return db, path
}

func snapshotEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	writeOK(t, err)
	names := []string{}
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func countRows(t *testing.T, path, table string) (int, string) {
	t.Helper()
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: filepath.ToSlash(path), RawQuery: "mode=ro&immutable=1"}).String())
	writeOK(t, err)
	defer db.Close()
	var n int
	writeOK(t, db.QueryRow("SELECT count(*) FROM "+table).Scan(&n))
	var schema string
	writeOK(t, db.QueryRow("SELECT group_concat(sql, ';') FROM (SELECT sql FROM sqlite_schema ORDER BY name)").Scan(&schema))
	return n, schema
}

func TestSnapshotRoundTrip(t *testing.T) {
	db, _ := snapshotSource(t)
	destDir := t.TempDir()
	dest := filepath.Join(destDir, "it's a \"snap\" ünï ?#%.sqlite")
	writeOK(t, Snapshot(context.Background(), db, dest))

	info, err := os.Stat(dest)
	writeOK(t, err)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", info.Mode().Perm())
	}
	if names := snapshotEntries(t, destDir); len(names) != 1 {
		t.Fatalf("leftovers: %v", names)
	}
	n, schema := countRows(t, dest, "snap_rows")
	var srcN int
	var srcSchema string
	writeOK(t, db.QueryRow("SELECT count(*) FROM snap_rows").Scan(&srcN))
	writeOK(t, db.QueryRow("SELECT group_concat(sql, ';') FROM (SELECT sql FROM sqlite_schema ORDER BY name)").Scan(&srcSchema))
	if n != 20 || n != srcN || schema != srcSchema {
		t.Fatalf("rows %d/%d schema equal=%v", n, srcN, schema == srcSchema)
	}
	// Snapshot reopens through the app path and is a writable migrated database.
	snap, err := Open(dest)
	writeOK(t, err)
	defer snap.Close()
	var ledger int
	writeOK(t, snap.QueryRow("SELECT count(*) FROM proxycore_schema_migrations").Scan(&ledger))
	if ledger == 0 {
		t.Fatal("ledger missing from reopened snapshot")
	}
}

func TestSnapshotWhileWriting(t *testing.T) {
	db, _ := snapshotSource(t)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				db.Exec("INSERT INTO snap_rows(v) VALUES ('live')")
			}
		}
	}()
	dir := t.TempDir()
	var err error
	for i := 0; i < 5 && err == nil; i++ {
		err = Snapshot(context.Background(), db, filepath.Join(dir, strings.Repeat("s", i+1)+".sqlite"))
	}
	close(stop)
	wg.Wait()
	writeOK(t, err)
	n, _ := countRows(t, filepath.Join(dir, "s.sqlite"), "snap_rows")
	if n < 20 {
		t.Fatalf("rows %d", n)
	}
	if len(snapshotEntries(t, dir)) != 5 {
		t.Fatalf("entries %v", snapshotEntries(t, dir))
	}
}

func TestSnapshotEmptyMigratedAndSourceUnchanged(t *testing.T) {
	db, _ := migrationDB(t)
	writeOK(t, Migrate(context.Background(), db))
	var live, after int
	writeOK(t, db.QueryRow("SELECT count(*) FROM proxycore_schema_migrations").Scan(&live))
	dest := filepath.Join(t.TempDir(), "empty.sqlite")
	writeOK(t, Snapshot(context.Background(), db, dest))
	if n, _ := countRows(t, dest, "proxycore_schema_migrations"); n != live || n == 0 {
		t.Fatalf("ledger %d vs %d", n, live)
	}
	writeOK(t, db.QueryRow("SELECT count(*) FROM proxycore_schema_migrations").Scan(&after))
	if after != live {
		t.Fatalf("source ledger changed: %d -> %d", live, after)
	}
}

func TestSnapshotRejectsBadDestinations(t *testing.T) {
	db, _ := snapshotSource(t)
	ctx := context.Background()
	dir := t.TempDir()
	existing := filepath.Join(dir, "existing.sqlite")
	writeOK(t, os.WriteFile(existing, []byte("keep me"), 0o644))
	for name, dest := range map[string]string{
		"empty":          "",
		"nul":            filepath.Join(dir, "a\x00b"),
		"missing parent": filepath.Join(dir, "missing", "x.sqlite"),
		"existing":       existing,
	} {
		if err := Snapshot(ctx, db, dest); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	got, err := os.ReadFile(existing)
	writeOK(t, err)
	if string(got) != "keep me" {
		t.Fatal("existing destination modified")
	}
	if names := snapshotEntries(t, dir); len(names) != 1 {
		t.Fatalf("leftovers: %v", names)
	}
	if _, err := os.Stat(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("parent created")
	}
}

func TestSnapshotCleansTempOnFailure(t *testing.T) {
	db, _ := snapshotSource(t)
	dir := t.TempDir()
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Snapshot(canceled, db, filepath.Join(dir, "c.sqlite")); err == nil {
		t.Fatal("expected cancellation error")
	}
	// Corrupt the temp file after VACUUM so verification must fail closed.
	snapshotAfterVacuum = func(path string) {
		f, err := os.OpenFile(path, os.O_WRONLY, 0)
		writeOK(t, err)
		defer f.Close()
		_, err = f.WriteAt([]byte(strings.Repeat("\xff", 4096)), 4096)
		writeOK(t, err)
	}
	t.Cleanup(func() { snapshotAfterVacuum = nil })
	if err := Snapshot(context.Background(), db, filepath.Join(dir, "i.sqlite")); err == nil {
		t.Fatal("expected integrity failure")
	}
	if names := snapshotEntries(t, dir); len(names) != 0 {
		t.Fatalf("leftovers: %v", names)
	}
}
