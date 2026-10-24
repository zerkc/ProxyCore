// Package testsupport holds fixtures shared by the worker package tests: a
// migrated temporary SQLite database, seed rows, and a Bun runner for interop.
// It is imported only from _test files.
package testsupport

import (
	"context"
	"database/sql"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

// NewDB creates a Go-migrated SQLite file in a temp dir and returns the open
// handle plus its path. The handle is closed on test cleanup.
func NewDB(t testing.TB) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "proxycore.sqlite")
	db, err := sqlitedb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := sqlitedb.Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	return db, path
}

// UUID builds a deterministic canonical lowercase UUID from a small number.
func UUID(n int) string {
	return fmt.Sprintf("bbbbbbbb-0000-4000-8000-%012x", n)
}

// Stamp encodes a finite instant with the sortable tagged timestamp format.
func Stamp(t testing.TB, at time.Time) string {
	t.Helper()
	key, err := sqlitedb.EncodeTimestamp(sqlitedb.FiniteTimestamp{Seconds: at.Unix(), Nanos: int32(at.Nanosecond())})
	if err != nil {
		t.Fatal(err)
	}
	return *key
}

// InsertRevision seeds a config revision with a minimal snapshot document.
func InsertRevision(t testing.TB, db *sql.DB, id string, number int, appliedAt any) {
	t.Helper()
	if _, err := db.Exec(`insert into config_revisions (id,revision_number,checksum,snapshot,created_at,applied_at)
		values (?,?,?,?,?,?)`, id, number, fmt.Sprintf("checksum-%d", number), `{"zones":[]}`, "0", appliedAt); err != nil {
		t.Fatal(err)
	}
}

// InsertJob seeds a queued-or-otherwise apply job row.
func InsertJob(t testing.TB, db *sql.DB, id, revisionID, status string, createdAt string, claimedAt any) {
	t.Helper()
	if _, err := db.Exec(`insert into apply_jobs (id,revision_id,target,status,correlation_id,created_at,claimed_at)
		values (?,?,'combined',?,?,?,?)`, id, revisionID, status, "corr-"+id[len(id)-4:], createdAt, claimedAt); err != nil {
		t.Fatal(err)
	}
}

// Bun returns the bun executable path or skips the test when it is missing.
func Bun(t testing.TB) string {
	t.Helper()
	bun, err := exec.LookPath("bun")
	if err != nil {
		t.Skip("bun is not installed; skipping Bun interop")
	}
	abs, err := filepath.Abs(bun)
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

// RepoPath resolves a repository-relative path from this source file.
func RepoPath(elem ...string) string {
	_, source, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(source), "../../../../..")
	return filepath.Join(append([]string{root}, elem...)...)
}

// RunBun runs a Bun script outside the repository cwd with env files disabled
// and returns its combined output.
func RunBun(t testing.TB, script string, args ...string) string {
	t.Helper()
	bun := Bun(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bun, append([]string{"--no-env-file", script}, args...)...)
	cmd.Dir = t.TempDir()
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bun %s: %v\n%s", filepath.Base(script), err, out)
	}
	return string(out)
}
