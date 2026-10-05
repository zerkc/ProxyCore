package configuration

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

func conditionalEnqueueCommand(t *testing.T, args ...string) (*exec.Cmd, context.CancelFunc) {
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
	runner := filepath.Clean(filepath.Join(filepath.Dir(source), "../../../../packages/db/src/sqlite-foundation/job-enqueue-if-not-active-record.bun.ts"))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	cmd := exec.CommandContext(ctx, bun, append([]string{"--no-env-file", runner}, args...)...)
	cmd.Dir = t.TempDir()
	cmd.WaitDelay = time.Second
	return cmd, cancel
}

func TestSQLiteWorkerJobEnqueueIfNotActiveRecordFixture(t *testing.T) {
	db, path := enqueueFixture(t)
	job := enqueueCall(t, db, "").Job
	before := enqueueRevisionBytes(t, db)
	cmd, cancel := conditionalEnqueueCommand(t, "unit", path, job.ID, job.RevisionID, job.CorrelationID)
	defer cancel()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Go-migrated conditional Bun enqueue: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "COMPLETE conditional enqueue cases=") {
		t.Fatalf("missing completed Bun coverage: %s", out)
	}
	t.Logf("embedded Bun cases:\n%s", out)
	if got := enqueueRevisionBytes(t, db); got != before {
		t.Fatal("conditional enqueue changed revision bytes")
	}
	reopened, err := sqlitedb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	var count int
	if err := reopened.QueryRow("SELECT count(*) FROM apply_jobs").Scan(&count); err != nil || count != 2 {
		t.Fatal("original plus durable conditional job:", count, err)
	}
}

func conditionalEnqueueStart(t *testing.T, path, revision, target string) *claimChild {
	t.Helper()
	cmd, cancel := conditionalEnqueueCommand(t, "child", path, "", revision, target)
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
				t.Errorf("conditional Bun child: %v\n%s", err, stderr.String())
			}
		case <-timer.C:
			cancel()
			t.Errorf("conditional child cleanup deadline: %v", <-finished)
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

func TestSQLiteWorkerJobEnqueueIfNotActiveRecordIndependentProcesses(t *testing.T) {
	db, path := enqueueFixture(t)
	job := enqueueCall(t, db, "").Job
	before := enqueueRevisionBytes(t, db)
	terminal := func() {
		t.Helper()
		if _, err := db.Exec("UPDATE apply_jobs SET status='applied'"); err != nil {
			t.Fatal(err)
		}
	}
	count := func(want int) {
		t.Helper()
		var got int
		if err := db.QueryRow("SELECT count(*) FROM apply_jobs").Scan(&got); err != nil || got != want {
			t.Fatal("durable job count:", got, want, err)
		}
	}
	terminal()
	first := conditionalEnqueueStart(t, path, job.RevisionID, "certificate")
	second := conditionalEnqueueStart(t, path, job.RevisionID, "nginx")
	first.send(t, "conditional")
	second.send(t, "conditional")
	one, two := first.read(t), second.read(t)
	if !(strings.HasPrefix(one, "ENQUEUED ") && two == "EMPTY" || one == "EMPTY" && strings.HasPrefix(two, "ENQUEUED ")) {
		t.Fatal("one global reservation across different targets:", one, two)
	}
	count(2)
	terminal()
	first.send(t, "hold")
	if got := first.read(t); got != "HELD" {
		t.Fatal(got)
	}
	start := time.Now()
	second.send(t, "conditional")
	if got := second.read(t); got != "BUSY" {
		t.Fatal("BUSY is rejection, not undefined:", got)
	}
	elapsed := time.Since(start)
	if elapsed < 100*time.Millisecond || elapsed > 2*time.Second {
		t.Fatal("bounded busy timeout:", elapsed)
	}
	count(2)
	first.send(t, "release")
	if got := first.read(t); got != "RELEASED" {
		t.Fatal(got)
	}
	second.send(t, "conditional")
	if got := second.read(t); !strings.HasPrefix(got, "ENQUEUED ") {
		t.Fatal("release permits reservation:", got)
	}
	count(3)
	terminal()
	// Conditional first does not prevent a subsequent ordinary insertion.
	first.send(t, "conditional")
	if got := first.read(t); !strings.HasPrefix(got, "ENQUEUED ") {
		t.Fatal(got)
	}
	second.send(t, "ordinary")
	if got := second.read(t); !strings.HasPrefix(got, "ENQUEUED ") {
		t.Fatal(got)
	}
	count(5)
	terminal()
	// Ordinary first must block the conditional reservation globally.
	first.send(t, "ordinary")
	if got := first.read(t); !strings.HasPrefix(got, "ENQUEUED ") {
		t.Fatal(got)
	}
	second.send(t, "conditional")
	if got := second.read(t); got != "EMPTY" {
		t.Fatal(got)
	}
	count(6)
	if got := enqueueRevisionBytes(t, db); got != before {
		t.Fatal("contention changed revision bytes")
	}
	t.Logf("independent processes and both cross-orderings; BUSY duration=%s", elapsed)
}
