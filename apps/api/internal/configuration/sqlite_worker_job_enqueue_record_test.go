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

func jobEnqueueRecordCommand(t *testing.T, args ...string) (*exec.Cmd, context.CancelFunc) {
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
	runner := filepath.Clean(filepath.Join(filepath.Dir(source), "../../../../packages/db/src/sqlite-foundation/job-enqueue-record.bun.ts"))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	cmd := exec.CommandContext(ctx, bun, append([]string{"--no-env-file", runner}, args...)...)
	cmd.Dir = t.TempDir()
	cmd.WaitDelay = time.Second
	return cmd, cancel
}

func TestSQLiteWorkerJobEnqueueRecordFixture(t *testing.T) {
	db, path := enqueueFixture(t)
	job := enqueueCall(t, db, "").Job
	before := enqueueRevisionBytes(t, db)
	cmd, cancel := jobEnqueueRecordCommand(t, "unit", path, job.ID, job.RevisionID, job.CorrelationID)
	defer cancel()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Go-migrated public Bun enqueue fixture: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "COMPLETE enqueue cases=") {
		t.Fatalf("missing completed Bun coverage: %s", out)
	}
	t.Logf("embedded Bun cases:\n%s", out)
	if got := enqueueRevisionBytes(t, db); got != before {
		t.Fatal("enqueue changed revision bytes")
	}
	reopened, err := sqlitedb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	var count int
	if err := reopened.QueryRow("SELECT count(*) FROM apply_jobs").Scan(&count); err != nil || count != 2 {
		t.Fatal("original plus durable Bun job:", count, err)
	}
}

func jobEnqueueRecordStart(t *testing.T, path, revision string) *claimChild {
	t.Helper()
	cmd, cancel := jobEnqueueRecordCommand(t, "child", path, "", revision)
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

func TestSQLiteWorkerJobEnqueueRecordIndependentProcesses(t *testing.T) {
	db, path := enqueueFixture(t)
	job := enqueueCall(t, db, "").Job
	before := enqueueRevisionBytes(t, db)
	first, second := jobEnqueueRecordStart(t, path, job.RevisionID), jobEnqueueRecordStart(t, path, job.RevisionID)
	first.send(t, "enqueue")
	second.send(t, "enqueue")
	one, two := first.read(t), second.read(t)
	if !strings.HasPrefix(one, "ENQUEUED ") || !strings.HasPrefix(two, "ENQUEUED ") || one == two {
		t.Fatal("distinct ordinary enqueues must both commit:", one, two)
	}
	first.send(t, "hold")
	if got := first.read(t); got != "HELD" {
		t.Fatal(got)
	}
	start := time.Now()
	second.send(t, "enqueue")
	if got := second.read(t); got != "BUSY" {
		t.Fatal("BUSY must reject without publication:", got)
	}
	elapsed := time.Since(start)
	if elapsed < 100*time.Millisecond || elapsed > 2*time.Second {
		t.Fatal("bounded busy_timeout:", elapsed)
	}
	var count int
	if err := db.QueryRow("SELECT count(*) FROM apply_jobs").Scan(&count); err != nil || count != 3 {
		t.Fatal("BUSY preserved jobs:", count, err)
	}
	first.send(t, "release")
	if got := first.read(t); got != "RELEASED" {
		t.Fatal(got)
	}
	second.send(t, "enqueue")
	if got := second.read(t); !strings.HasPrefix(got, "ENQUEUED ") {
		t.Fatal(got)
	}
	if err := db.QueryRow("SELECT count(*) FROM apply_jobs").Scan(&count); err != nil || count != 4 {
		t.Fatal("release allowed next write:", count, err)
	}
	if got := enqueueRevisionBytes(t, db); got != before {
		t.Fatal("contention changed revision")
	}
}
