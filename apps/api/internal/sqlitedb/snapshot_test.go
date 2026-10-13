package sqlitedb

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
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
	const batch = 5 // initial 20 rows are a multiple of batch
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
			}
			tx, err := db.Begin()
			if err != nil {
				continue
			}
			ok := true
			for i := 0; i < batch && ok; i++ {
				_, err = tx.Exec("INSERT INTO snap_rows(v) VALUES ('live')")
				ok = err == nil
			}
			if ok {
				ok = tx.Commit() == nil
			} else {
				tx.Rollback()
			}
		}
	}()
	dir := t.TempDir()
	var err error
	paths := []string{}
	for i := 0; i < 5 && err == nil; i++ {
		p := filepath.Join(dir, strings.Repeat("s", i+1)+".sqlite")
		paths = append(paths, p)
		err = Snapshot(context.Background(), db, p)
	}
	close(stop)
	wg.Wait()
	writeOK(t, err)
	if len(snapshotEntries(t, dir)) != 5 {
		t.Fatalf("entries %v", snapshotEntries(t, dir))
	}
	prev := 0
	for _, p := range paths {
		snap, err := Open(p)
		writeOK(t, err)
		var integrity string
		writeOK(t, snap.QueryRow("PRAGMA integrity_check").Scan(&integrity))
		var n, minID, maxID, bad int
		writeOK(t, snap.QueryRow("SELECT count(*), min(id), max(id), sum(v NOT IN ('synthetic','live')) FROM snap_rows").Scan(&n, &minID, &maxID, &bad))
		snap.Close()
		// A committed prefix: whole batches only, contiguous ids, valid values, monotonic over time.
		if integrity != "ok" || n < 20 || n%batch != 0 || minID != 1 || maxID != n || bad != 0 || n < prev {
			t.Fatalf("%s inconsistent: integrity=%s n=%d min=%d max=%d bad=%d prev=%d", p, integrity, n, minID, maxID, bad, prev)
		}
		prev = n
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
	err := Snapshot(context.Background(), db, filepath.Join(dir, "i.sqlite"))
	if err == nil || !strings.Contains(err.Error(), "integrity_check failed") {
		t.Fatalf("expected integrity_check failure, got %v", err)
	}
	if names := snapshotEntries(t, dir); len(names) != 0 {
		t.Fatalf("leftovers: %v", names)
	}
}

// failingSnapshot runs Snapshot with seams installed and returns its error,
// the destination path, and the destination directory.
func failingSnapshot(t *testing.T, ctx context.Context, setup func(dir string)) (error, string, string) {
	t.Helper()
	db, _ := snapshotSource(t)
	dir := t.TempDir()
	oldVac, oldLink, oldSync := snapshotAfterVacuum, snapshotLink, snapshotSync
	t.Cleanup(func() { snapshotAfterVacuum, snapshotLink, snapshotSync = oldVac, oldLink, oldSync })
	setup(dir)
	dest := filepath.Join(dir, "out.sqlite")
	return Snapshot(ctx, db, dest), dest, dir
}

func assertNothingPublished(t *testing.T, dest, dir string) {
	t.Helper()
	if _, err := os.Lstat(dest); err == nil {
		t.Fatal("destination exists")
	}
	if names := snapshotEntries(t, dir); len(names) != 0 {
		t.Fatalf("leftovers: %v", names)
	}
}

func TestSnapshotCanceledDuringVerification(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err, dest, dir := failingSnapshot(t, ctx, func(string) {
		snapshotAfterVacuum = func(string) { cancel() }
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	assertNothingPublished(t, dest, dir)
}

func TestSnapshotLinkFailure(t *testing.T) {
	linkErr := errors.New("link unsupported")
	err, dest, dir := failingSnapshot(t, context.Background(), func(string) {
		snapshotLink = func(string, string) error { return linkErr }
	})
	if !errors.Is(err, linkErr) || !strings.Contains(err.Error(), "hard links") {
		t.Fatalf("expected actionable wrapped link error, got %v", err)
	}
	if errors.Is(err, ErrSnapshotPublishedUnsynced) {
		t.Fatal("link failure must not report published")
	}
	assertNothingPublished(t, dest, dir)
}

func TestSnapshotFileSyncFailurePublishesNothing(t *testing.T) {
	syncErr := errors.New("file sync failed")
	linked := false
	err, dest, dir := failingSnapshot(t, context.Background(), func(dir string) {
		snapshotSync = func(p string) error {
			if p != dir {
				return syncErr
			}
			return nil
		}
		snapshotLink = func(a, b string) error { linked = true; return os.Link(a, b) }
	})
	if !errors.Is(err, syncErr) || errors.Is(err, ErrSnapshotPublishedUnsynced) {
		t.Fatalf("unexpected error %v", err)
	}
	if linked {
		t.Fatal("published despite file sync failure")
	}
	assertNothingPublished(t, dest, dir)
}

func TestSnapshotDirectorySyncFailureReportsPublishedUnsynced(t *testing.T) {
	syncErr := errors.New("dir sync failed")
	err, dest, dir := failingSnapshot(t, context.Background(), func(dir string) {
		snapshotSync = func(p string) error {
			if p == dir {
				return syncErr
			}
			return syncPath(p)
		}
	})
	if !errors.Is(err, ErrSnapshotPublishedUnsynced) || !errors.Is(err, syncErr) {
		t.Fatalf("expected sentinel wrapping cause, got %v", err)
	}
	info, statErr := os.Stat(dest)
	writeOK(t, statErr)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", info.Mode().Perm())
	}
	if n, _ := countRows(t, dest, "snap_rows"); n != 20 {
		t.Fatalf("rows %d", n)
	}
	if names := snapshotEntries(t, dir); len(names) != 1 {
		t.Fatalf("expected only destination, got %v", names)
	}
}

func execOnFile(t *testing.T, path string, stmts ...string) {
	t.Helper()
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: filepath.ToSlash(path), RawQuery: "_pragma=foreign_keys(0)"}).String())
	writeOK(t, err)
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, st := range stmts {
		_, err := db.Exec(st)
		writeOK(t, err)
	}
}

func TestSnapshotForeignKeyViolationFailsClosed(t *testing.T) {
	err, dest, dir := failingSnapshot(t, context.Background(), func(string) {
		snapshotAfterVacuum = func(path string) {
			execOnFile(t, path,
				"CREATE TABLE fk_parent(id INTEGER PRIMARY KEY)",
				"CREATE TABLE fk_child(pid INTEGER REFERENCES fk_parent(id))",
				"INSERT INTO fk_child VALUES (42)")
		}
	})
	if err == nil || !strings.Contains(err.Error(), "foreign_key_check failed") {
		t.Fatalf("expected foreign_key_check failure, got %v", err)
	}
	assertNothingPublished(t, dest, dir)
}

func TestSnapshotLedgerMismatchFailsClosed(t *testing.T) {
	err, dest, dir := failingSnapshot(t, context.Background(), func(string) {
		snapshotAfterVacuum = func(path string) {
			execOnFile(t, path, "UPDATE proxycore_schema_migrations SET checksum = 'tampered'")
		}
	})
	if err == nil || !strings.Contains(err.Error(), "schema version mismatch") {
		t.Fatalf("expected ledger mismatch, got %v", err)
	}
	assertNothingPublished(t, dest, dir)
}

func TestSnapshotChmodsBeforePublish(t *testing.T) {
	old := syscall.Umask(0)
	t.Cleanup(func() { syscall.Umask(old) })
	var modeAtLink os.FileMode
	err, dest, _ := failingSnapshot(t, context.Background(), func(string) {
		// Simulate a wider-mode temp file (e.g. a looser umask or SQLite recreating it).
		snapshotAfterVacuum = func(path string) { writeOK(t, os.Chmod(path, 0o666)) }
		snapshotLink = func(tmp, dst string) error {
			info, err := os.Stat(tmp)
			writeOK(t, err)
			modeAtLink = info.Mode().Perm()
			return os.Link(tmp, dst)
		}
	})
	writeOK(t, err)
	if modeAtLink != 0o600 {
		t.Fatalf("mode at publish time %v, want 0600", modeAtLink)
	}
	info, statErr := os.Stat(dest)
	writeOK(t, statErr)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("final mode %v", info.Mode().Perm())
	}
}
