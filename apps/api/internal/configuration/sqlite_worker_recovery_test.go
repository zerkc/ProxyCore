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

func recoveryCommand(t *testing.T, args ...string) (*exec.Cmd, context.CancelFunc) {
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
	runner := filepath.Clean(filepath.Join(filepath.Dir(source), "../../../../packages/db/src/sqlite-foundation/job-recovery.bun.ts"))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	cmd := exec.CommandContext(ctx, bun, append([]string{"--no-env-file", runner}, args...)...)
	cmd.Dir = t.TempDir()
	cmd.WaitDelay = time.Second
	return cmd, cancel
}

func TestSQLiteWorkerRecoveryFixture(t *testing.T) {
	db, path := enqueueFixture(t)
	enqueueCall(t, db, "")
	cmd, cancel := recoveryCommand(t, "unit", path)
	defer cancel()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("real Go-migrated Bun recovery fixture: %v\n%s", err, out)
	}
	t.Logf("embedded Bun cases:\n%s", out)
}

// Independent processes own their connections. HELD proves a real writer lock,
// BUSY proves the competitor cannot cross it, and explicit replies fix the order.
func recoveryStart(t *testing.T, path string) *claimChild {
	t.Helper()
	cmd, cancel := recoveryCommand(t, "child", path)
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
				t.Errorf("recovery child: %v\n%s", err, stderr.String())
			}
		case <-timer.C:
			cancel()
			_ = output.Close()
			t.Errorf("recovery child cleanup deadline: %v", <-finished)
		}
		cancel()
		_ = input.Close()
		_ = output.Close()
	})
	child := &claimChild{input: input, lines: lines}
	if got := child.read(t); got != "READY" {
		t.Fatal("independent recovery READY:", got)
	}
	return child
}

func recoveryReply(t *testing.T, child *claimChild, command, want string) {
	t.Helper()
	child.send(t, command)
	if got := child.read(t); got != want {
		t.Fatalf("%s: got %s, want %s", command, got, want)
	}
}

func TestSQLiteWorkerRecoveryIndependentOrders(t *testing.T) {
	for _, order := range []string{"recover-first", "claim-first-fresh", "claim-first-stale"} {
		t.Run(order, func(t *testing.T) {
			db, path := enqueueFixture(t)
			job := enqueueCall(t, db, "").Job
			before := enqueueRevisionBytes(t, db)
			recoverer, claimant := recoveryStart(t, path), recoveryStart(t, path)
			owned := "OWNED " + job.ID
			if order == "recover-first" {
				recoveryReply(t, claimant, "claim-old", owned)
				claimDurable(t, db, job.ID, "validating")
				recoveryReply(t, claimant, "hold", "HELD")
				recoveryReply(t, recoverer, "recover", "BUSY")
				claimDurable(t, db, job.ID, "validating")
				recoveryReply(t, claimant, "release", "RELEASED")
				recoveryReply(t, recoverer, "hold", "HELD")
				recoveryReply(t, claimant, "claim-new", "BUSY")
				recoveryReply(t, recoverer, "release", "RELEASED")
				recoveryReply(t, recoverer, "recover", "COUNT 1")
				claimDurable(t, db, job.ID, "queued")
				var claimed, started *string
				if err := db.QueryRow("SELECT claimed_at,started_at FROM apply_jobs WHERE id=?", job.ID).Scan(&claimed, &started); err != nil || claimed != nil || started != nil {
					t.Fatal("recovery clears claim and start:", claimed, started, err)
				}
				recoveryReply(t, claimant, "claim-new", owned)
			} else {
				recoveryReply(t, claimant, "hold", "HELD")
				start := time.Now()
				recoveryReply(t, recoverer, "recover", "BUSY")
				elapsed := time.Since(start)
				if elapsed < 100*time.Millisecond || elapsed > 2*time.Second {
					t.Fatal("bounded recovery BUSY:", elapsed)
				}
				claimDurable(t, db, job.ID, "queued")
				recoveryReply(t, claimant, "release", "RELEASED")
				command, count := "claim-new", "COUNT 0"
				if order == "claim-first-stale" {
					command, count = "claim-old", "COUNT 1"
				}
				recoveryReply(t, claimant, command, owned)
				recoveryReply(t, recoverer, "recover", count)
				if count == "COUNT 1" {
					claimDurable(t, db, job.ID, "queued")
					recoveryReply(t, claimant, "claim-new", owned)
				}
				t.Logf("independent recovery BUSY %s; release then %s", elapsed, count)
			}
			claimDurable(t, db, job.ID, "validating")
			recoveryReply(t, recoverer, "recover", "COUNT 0")
			if got := enqueueRevisionBytes(t, db); got != before {
				t.Fatal("recovery changed revision bytes")
			}
			reopened, err := sqlitedb.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			claimDurable(t, reopened, job.ID, "validating")
			t.Log("READY/HELD independent Bun claim/recovery order and durable Go observation:", order)
		})
	}
}
