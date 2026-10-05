package configuration

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"io"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

func claimCommand(t *testing.T, args ...string) (*exec.Cmd, context.CancelFunc) {
	t.Helper()
	bun, err := exec.LookPath("bun")
	if err != nil {
		t.Fatal("required Bun executable:", err)
	}
	bun, err = filepath.Abs(bun)
	if err != nil {
		t.Fatal(err)
	}
	_, source, _, _ := runtime.Caller(0)
	runner := filepath.Clean(filepath.Join(filepath.Dir(source), "../../../../packages/db/src/sqlite-foundation/job-claim.bun.ts"))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	cmd := exec.CommandContext(ctx, bun, append([]string{"--no-env-file", runner}, args...)...)
	cmd.Dir = t.TempDir()
	cmd.WaitDelay = time.Second
	return cmd, cancel
}

func TestSQLiteWorkerClaimFixture(t *testing.T) {
	cmd, cancel := claimCommand(t, "unit")
	defer cancel()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("real Bun claim fixture: %v\n%s", err, out)
	}
	t.Logf("real migrated Bun cases:\n%s", out)
}

// Each child opens its own connection in an independent Bun process. Both must
// reach READY before either is sent a claim; no in-process ownership simulation.
type claimChild struct {
	input io.WriteCloser
	lines <-chan string
}

func claimStart(t *testing.T, path string) *claimChild {
	t.Helper()
	cmd, cancel := claimCommand(t, path)
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
				t.Errorf("Bun child: %v\n%s", err, stderr.String())
			}
		case <-timer.C:
			cancel()
			t.Errorf("Bun graceful cleanup deadline: %v", <-finished)
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

func (c *claimChild) send(t *testing.T, command string) {
	t.Helper()
	if _, err := io.WriteString(c.input, command+"\n"); err != nil {
		t.Fatal(err)
	}
}

func (c *claimChild) read(t *testing.T) string {
	t.Helper()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	select {
	case line, ok := <-c.lines:
		if !ok {
			t.Fatal("Bun closed before observation")
		}
		return line
	case <-timer.C:
		t.Fatal("bounded claim handshake timed out")
		return ""
	}
}

func claimDurable(t *testing.T, db *sql.DB, id, status string) {
	t.Helper()
	var actual string
	if err := db.QueryRow("SELECT status FROM apply_jobs WHERE id=?", id).Scan(&actual); err != nil || actual != status {
		t.Fatal("durable ownership:", actual, err)
	}
}

func TestSQLiteWorkerClaimIndependentOwnership(t *testing.T) {
	db, path := enqueueFixture(t)
	job := enqueueCall(t, db, "").Job
	before := enqueueRevisionBytes(t, db)
	first, second := claimStart(t, path), claimStart(t, path)
	first.send(t, "claim")
	second.send(t, "claim")
	one, two := first.read(t), second.read(t)
	want := "OWNED " + job.ID
	if !(one == want && two == "EMPTY" || two == want && one == "EMPTY") {
		t.Fatal("exactly one independent owner:", one, two)
	}
	claimDurable(t, db, job.ID, "validating")
	if got := enqueueRevisionBytes(t, db); got != before {
		t.Fatal("claim modified revision")
	}
	var claimed, started, created string
	if err := db.QueryRow("SELECT claimed_at,started_at,created_at FROM apply_jobs").Scan(&claimed, &started, &created); err != nil {
		t.Fatal(err)
	}
	stamp, _ := sqlitedb.EncodeTimestamp(sqlitedb.FiniteTimestamp{Seconds: 123, Nanos: 456})
	if claimed != *stamp || started != claimed || created != claimed {
		t.Fatal("shared Go/Bun native clock:", claimed, started, created)
	}
	reopened, err := sqlitedb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	claimDurable(t, reopened, job.ID, "validating")
	t.Log("two independent Bun processes: one owner, one EMPTY, reopened validating")
}

func TestSQLiteWorkerClaimBusyThenRelease(t *testing.T) {
	db, path := enqueueFixture(t)
	job := enqueueCall(t, db, "").Job
	holder, claimant := claimStart(t, path), claimStart(t, path)
	holder.send(t, "hold")
	if got := holder.read(t); got != "HELD" {
		t.Fatal(got)
	}
	start := time.Now()
	claimant.send(t, "claim")
	if got := claimant.read(t); got != "BUSY" {
		t.Fatal("BUSY must not become empty:", got)
	}
	elapsed := time.Since(start)
	if elapsed < 100*time.Millisecond || elapsed > 2*time.Second {
		t.Fatal("bounded synchronous busy_timeout:", elapsed)
	}
	claimDurable(t, db, job.ID, "queued")
	holder.send(t, "release")
	if got := holder.read(t); got != "RELEASED" {
		t.Fatal(got)
	}
	claimant.send(t, "claim")
	if got := claimant.read(t); !strings.HasPrefix(got, "OWNED ") || got != "OWNED "+job.ID {
		t.Fatal(got)
	}
	claimDurable(t, db, job.ID, "validating")
	// The Go caller's database and writer remain usable after Bun contention.
	enqueueCall(t, db, "")
	t.Logf("independent holder: BUSY in %s, queued unchanged; release then success", elapsed)
}
