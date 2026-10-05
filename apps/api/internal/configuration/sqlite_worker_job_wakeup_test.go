package configuration

import (
	"bufio"
	"context"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

func TestSQLiteWorkerJobWakeup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "job-wakeup.db")
	db, err := sqlitedb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := sqlitedb.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	const revision = "cccccccc-cccc-cccc-cccc-cccccccccccc"
	const job = "ffffffff-ffff-ffff-ffff-ffffffffffff"
	// Synthetic parent only; the Go side is the raw cross-process writer.
	if _, err := db.Exec(`INSERT INTO config_revisions(id,revision_number,checksum,snapshot,actor_user_id,created_at)
		VALUES(?,1,'fixture-only-checksum','{}',NULL,'0')`, revision); err != nil {
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
	runner := filepath.Clean(filepath.Join(filepath.Dir(source), "../../../../packages/db/src/sqlite-foundation/job-wakeup.bun.ts"))
	start := func(mode string) (*exec.Cmd, *bufio.Scanner, context.CancelFunc) {
		t.Helper()
		runCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		cmd := exec.CommandContext(runCtx, bun, "--no-env-file", runner, path, mode)
		cmd.Dir = t.TempDir()
		cmd.WaitDelay = time.Second
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		cmd.Stderr = cmd.Stdout
		if err := cmd.Start(); err != nil {
			cancel()
			t.Fatal(err)
		}
		return cmd, bufio.NewScanner(stdout), cancel
	}
	// Deterministic fake-clock and abort/error semantics, then the real cursor on the migrated DB.
	for _, mode := range []string{"fake", "migrated"} {
		cmd, scanner, cancel := start(mode)
		var out []string
		for scanner.Scan() {
			out = append(out, scanner.Text())
		}
		if err := cmd.Wait(); err != nil {
			cancel()
			t.Fatalf("Bun job wakeup %s: %v\n%s", mode, err, strings.Join(out, "\n"))
		}
		cancel()
		if !strings.Contains(strings.Join(out, "\n"), "job-wakeup checks=") {
			t.Fatalf("missing Bun completion marker for %s: %v", mode, out)
		}
		t.Logf("%s", strings.Join(out, "\n"))
	}
	// Cross-process: the runner polls; this separate connection inserts, then updates, a job.
	if _, err := db.Exec(`DELETE FROM apply_jobs`); err != nil {
		t.Fatal(err)
	}
	cmd, scanner, cancel := start("cross")
	defer cancel()
	var out []string
	step := func(prefix string) {
		t.Helper()
		for scanner.Scan() {
			out = append(out, scanner.Text())
			if strings.HasPrefix(scanner.Text(), prefix) {
				return
			}
		}
		t.Fatalf("runner ended before %q: %v", prefix, out)
	}
	step("job-wakeup ready")
	if _, err := db.Exec(`INSERT INTO apply_jobs(id,revision_id,actor_user_id,target,correlation_id,created_at)
		VALUES(?,?,NULL,'combined','fixture-only','0')`, job, revision); err != nil {
		t.Fatal(err)
	}
	step("job-wakeup insert-detected")
	if _, err := db.Exec(`UPDATE apply_jobs SET status='validating',claimed_at='0' WHERE id=?`, job); err != nil {
		t.Fatal(err)
	}
	step("job-wakeup status-detected")
	for scanner.Scan() {
		out = append(out, scanner.Text())
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("Bun cross-process wakeup: %v\n%s", err, strings.Join(out, "\n"))
	}
	if !strings.Contains(strings.Join(out, "\n"), "job-wakeup checks=") {
		t.Fatalf("missing cross completion marker: %v", out)
	}
	t.Logf("%s", strings.Join(out, "\n"))
}
