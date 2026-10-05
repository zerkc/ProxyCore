package configuration

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

func jobClaimRecordCommand(t *testing.T, args ...string) (*exec.Cmd, context.CancelFunc) {
	t.Helper()
	if testing.Short() {
		t.Skip("external Bun integration fixture")
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
	runner := filepath.Clean(filepath.Join(filepath.Dir(source), "../../../../packages/db/src/sqlite-foundation/job-claim-record.bun.ts"))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	cmd := exec.CommandContext(ctx, bun, append([]string{"--no-env-file", runner}, args...)...)
	cmd.Dir = t.TempDir()
	cmd.WaitDelay = time.Second
	return cmd, cancel
}

func TestSQLiteWorkerJobClaimRecordFixture(t *testing.T) {
	db, path := enqueueFixture(t)
	job := enqueueCall(t, db, "").Job
	before := enqueueRevisionBytes(t, db)
	cmd, cancel := jobClaimRecordCommand(t, "unit", path, job.ID, job.RevisionID, job.CorrelationID)
	defer cancel()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Go-migrated public Bun claim fixture: %v\n%s", err, out)
	}
	t.Logf("embedded Bun cases:\n%s", out)
	if got := enqueueRevisionBytes(t, db); got != before {
		t.Fatal("public claim changed revision bytes")
	}
	reopened, err := sqlitedb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	claimDurable(t, reopened, job.ID, "validating")
	var claimed, started, created string
	if err := reopened.QueryRow("SELECT claimed_at,started_at,created_at FROM apply_jobs WHERE id=?", job.ID).Scan(&claimed, &started, &created); err != nil {
		t.Fatal(err)
	}
	if claimed != started || claimed == created {
		t.Fatal("public native clock/start transition:", claimed, started, created)
	}
	var count int
	if err := reopened.QueryRow("SELECT count(*) FROM apply_jobs").Scan(&count); err != nil || count != 1 {
		t.Fatal("only original Go job retained:", count, err)
	}
}

// Reuse the verified bounded READY/OWNED/EMPTY/BUSY protocol, but run the public
// async wrapper in distinct Bun processes, never an in-process simulation.
func jobClaimRecordStart(t *testing.T, path string) *claimChild {
	t.Helper()
	cmd, cancel := jobClaimRecordCommand(t, "child", path)
	input, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Cancel = func() error { _, err := io.WriteString(input, "exit\n"); return err }
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	lines := make(chan string, 32)
	finished := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(output)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
		close(lines)
		err := scanner.Err()
		if waitErr := cmd.Wait(); err == nil {
			err = waitErr
		}
		finished <- err
	}()
	t.Cleanup(func() {
		_, _ = io.WriteString(input, "exit\n")
		timer := time.NewTimer(2 * time.Second)
		defer timer.Stop()
		select {
		case err := <-finished:
			if err != nil {
				t.Errorf("public Bun child: %v\n%s", err, stderr.String())
			}
		case <-timer.C:
			cancel()
			t.Errorf("public Bun cleanup deadline: %v", <-finished)
		}
		cancel()
		_ = input.Close()
		_ = output.Close()
	})
	child := &claimChild{input: input, lines: lines}
	if got := child.read(t); got != "READY" {
		t.Fatal("missing independent handshake:", got)
	}
	return child
}

func TestSQLiteWorkerJobClaimRecordIndependentOwnership(t *testing.T) {
	db, path := enqueueFixture(t)
	job := enqueueCall(t, db, "").Job
	before := enqueueRevisionBytes(t, db)
	first, second := jobClaimRecordStart(t, path), jobClaimRecordStart(t, path)
	first.send(t, "claim")
	second.send(t, "claim")
	one, two := first.read(t), second.read(t)
	want := "OWNED " + job.ID
	if !(one == want && two == "EMPTY" || two == want && one == "EMPTY") {
		t.Fatal("exactly one public owner:", one, two)
	}
	claimDurable(t, db, job.ID, "validating")
	if got := enqueueRevisionBytes(t, db); got != before {
		t.Fatal("ownership changed revision")
	}
	t.Log("independent public claims: one OWNED, one EMPTY")
}

func TestSQLiteWorkerJobClaimRecordBusyThenRelease(t *testing.T) {
	db, path := enqueueFixture(t)
	job := enqueueCall(t, db, "").Job
	before := enqueueRevisionBytes(t, db)
	holder, claimant := jobClaimRecordStart(t, path), jobClaimRecordStart(t, path)
	holder.send(t, "hold")
	if got := holder.read(t); got != "HELD" {
		t.Fatal(got)
	}
	start := time.Now()
	claimant.send(t, "claim")
	if got := claimant.read(t); got != "BUSY" {
		t.Fatal("public BUSY rejection must not become empty:", got)
	}
	elapsed := time.Since(start)
	if elapsed < 100*time.Millisecond || elapsed > 2*time.Second {
		t.Fatal("bounded busy_timeout:", elapsed)
	}
	claimDurable(t, db, job.ID, "queued")
	holder.send(t, "release")
	if got := holder.read(t); got != "RELEASED" {
		t.Fatal(got)
	}
	claimant.send(t, "claim")
	if got := claimant.read(t); got != "OWNED "+job.ID {
		t.Fatal(got)
	}
	claimDurable(t, db, job.ID, "validating")
	if got := enqueueRevisionBytes(t, db); got != before {
		t.Fatal("contention changed revision")
	}
	enqueueCall(t, db, "")
	t.Logf("public independent BUSY in %s; release then success", elapsed)
}
