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

func recoveryRecordCommand(t *testing.T, args ...string) (*exec.Cmd, context.CancelFunc) {
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
	runner := filepath.Clean(filepath.Join(filepath.Dir(source), "../../../../packages/db/src/sqlite-foundation/job-recovery-record.bun.ts"))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	cmd := exec.CommandContext(ctx, bun, append([]string{"--no-env-file", runner}, args...)...)
	cmd.Dir = t.TempDir()
	cmd.WaitDelay = time.Second
	return cmd, cancel
}

func TestSQLiteWorkerJobRecoveryRecordFixture(t *testing.T) {
	db, path := enqueueFixture(t)
	job := enqueueCall(t, db, "").Job
	before := enqueueRevisionBytes(t, db)
	cmd, cancel := recoveryRecordCommand(t, "unit", path, job.ID)
	defer cancel()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Go-migrated public Bun recovery: %v\n%s", err, out)
	}
	t.Logf("embedded Bun cases:\n%s", out)
	if got := enqueueRevisionBytes(t, db); got != before {
		t.Fatal("public recovery changed revision bytes")
	}
}

// Each child has its own connection and bounded protocol. No shared in-process
// facade is used to prove lock contention or the two serialization orders.
func recoveryRecordStart(t *testing.T, path string) *claimChild {
	t.Helper()
	cmd, cancel := recoveryRecordCommand(t, "child", path)
	input, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		_ = input.Close()
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		cancel()
		_ = input.Close()
		_ = output.Close()
		t.Fatal(err)
	}
	lines := make(chan string, 32)
	finished := make(chan error, 1)
	stop := make(chan struct{})
	go func() {
		scanner := bufio.NewScanner(output)
		for scanner.Scan() {
			select {
			case lines <- scanner.Text():
			case <-stop:
			}
		}
		close(lines)
		err := scanner.Err()
		if waitErr := cmd.Wait(); err == nil {
			err = waitErr
		}
		finished <- err
	}()
	t.Cleanup(func() {
		close(stop)
		_, _ = io.WriteString(input, "exit\n")
		timer := time.NewTimer(2 * time.Second)
		defer timer.Stop()
		select {
		case err := <-finished:
			if err != nil {
				t.Errorf("public recovery child: %v\n%s", err, stderr.String())
			}
		case <-timer.C:
			cancel()
			_ = output.Close()
			t.Errorf("public recovery child deadline: %v", <-finished)
		}
		cancel()
		_ = input.Close()
		_ = output.Close()
	})
	child := &claimChild{input: input, lines: lines}
	if got := child.read(t); got != "READY" {
		t.Fatal("public recovery READY:", got)
	}
	return child
}

func TestSQLiteWorkerJobRecoveryRecordIndependentOrders(t *testing.T) {
	for _, order := range []string{"recover-first", "claim-first"} {
		t.Run(order, func(t *testing.T) {
			db, path := enqueueFixture(t)
			job := enqueueCall(t, db, "").Job
			before := enqueueRevisionBytes(t, db)
			recoverer, claimant := recoveryRecordStart(t, path), recoveryRecordStart(t, path)
			owned := "OWNED " + job.ID
			recoveryReply(t, claimant, "hold", "HELD")
			start := time.Now()
			recoveryReply(t, recoverer, "recover", "BUSY")
			elapsed := time.Since(start)
			if elapsed < 100*time.Millisecond || elapsed > 2*time.Second {
				t.Fatal("bounded public BUSY:", elapsed)
			}
			claimDurable(t, db, job.ID, "queued")
			recoveryReply(t, claimant, "release", "RELEASED")
			if order == "recover-first" {
				recoveryReply(t, recoverer, "recover", "COUNT 0")
				recoveryReply(t, recoverer, "hold", "HELD")
				recoveryReply(t, claimant, "claim", "BUSY")
				claimDurable(t, db, job.ID, "queued")
				recoveryReply(t, recoverer, "release", "RELEASED")
				recoveryReply(t, claimant, "claim", owned)
				recoveryReply(t, recoverer, "recover-fresh", "COUNT 0")
			} else {
				recoveryReply(t, claimant, "claim", owned)
				recoveryReply(t, claimant, "hold", "HELD")
				recoveryReply(t, recoverer, "recover", "BUSY")
				claimDurable(t, db, job.ID, "validating")
				recoveryReply(t, claimant, "release", "RELEASED")
				recoveryReply(t, recoverer, "recover", "COUNT 1")
				claimDurable(t, db, job.ID, "queued")
				var claimed, started *string
				if err := db.QueryRow("SELECT claimed_at,started_at FROM apply_jobs WHERE id=?", job.ID).Scan(&claimed, &started); err != nil || claimed != nil || started != nil {
					t.Fatal("public recovery clears claim/start:", claimed, started, err)
				}
				recoveryReply(t, claimant, "claim", owned)
			}
			claimDurable(t, db, job.ID, "validating")
			if got := enqueueRevisionBytes(t, db); got != before {
				t.Fatal("public process order changed revision bytes")
			}
			reopened, err := sqlitedb.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			claimDurable(t, reopened, job.ID, "validating")
			t.Logf("independent %s: BUSY %s; release-success and reopen verified", order, elapsed)
		})
	}
}
