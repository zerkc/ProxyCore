package configuration

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

func detectionCommand(t *testing.T, mode string) (*exec.Cmd, context.CancelFunc) {
	t.Helper()
	bun, err := exec.LookPath("bun")
	if err != nil {
		t.Fatal("required Bun acceptance executable:", err)
	}
	bun, err = filepath.Abs(bun)
	if err != nil {
		t.Fatal(err)
	}
	_, source, _, _ := runtime.Caller(0)
	runner := filepath.Clean(filepath.Join(filepath.Dir(source), "../../../worker/src/sqlite-job-detection.bun.ts"))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	cmd := exec.CommandContext(ctx, bun, "--no-env-file", runner, mode)
	cmd.Dir = t.TempDir()
	cmd.WaitDelay = time.Second
	return cmd, cancel
}

func TestSQLiteWorkerDetectionFixture(t *testing.T) {
	cmd, cancel := detectionCommand(t, "unit")
	defer cancel()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("real Bun fixture: %v\n%s", err, out)
	}
}

type detectionEvent struct {
	line string
	at   time.Time
}

type detectionChild struct {
	input     io.WriteCloser
	events    <-chan detectionEvent
	forbidden map[string]bool
	lastScan  int
}

func detectionStart(t *testing.T, path string) *detectionChild {
	t.Helper()
	cmd, cancel := detectionCommand(t, path)
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
	// Graceful own-child cancellation first; WaitDelay bounds a stuck child.
	cmd.Cancel = func() error { _, err := io.WriteString(input, "exit\n"); return err }
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	events := make(chan detectionEvent, 256)
	finished := make(chan error, 1)
	scanCtx, scanCancel := context.WithCancel(context.Background())
	go func() {
		scanner := bufio.NewScanner(output)
		for scanner.Scan() {
			select {
			case events <- detectionEvent{scanner.Text(), time.Now()}:
			case <-scanCtx.Done():
			}
		}
		close(events)
		err := scanner.Err()
		if waitErr := cmd.Wait(); err == nil {
			err = waitErr
		}
		finished <- err
	}()
	t.Cleanup(func() {
		_, _ = io.WriteString(input, "exit\n")
		scanCancel() // Drain output without blocking on an abandoned consumer.
		timer := time.NewTimer(2 * time.Second)
		defer timer.Stop()
		select {
		case err := <-finished:
			if err != nil {
				t.Errorf("Bun child cleanup: %v\n%s", err, stderr.String())
			}
		case <-timer.C:
			cancel()
			t.Errorf("Bun did not stop gracefully: %v", <-finished)
		}
		cancel()
		_ = input.Close()
		_ = output.Close()
	})
	return &detectionChild{input: input, events: events, forbidden: map[string]bool{}}
}

func (c *detectionChild) until(t *testing.T, deadline time.Time, match func(detectionEvent) bool) detectionEvent {
	t.Helper()
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	for {
		select {
		case event, ok := <-c.events:
			if !ok {
				t.Fatal("Bun ended before required observation")
			}
			fields := strings.Fields(event.line)
			if len(fields) == 3 && fields[0] == "JOB" {
				if c.forbidden[fields[1]] {
					t.Fatal("uncommitted/rolled-back ID observed:", fields[1])
				}
				if fields[2] != "true" {
					t.Fatal("controlled asynchronous processor was not busy")
				}
			}
			if strings.HasPrefix(event.line, "SCAN ") {
				_, _ = fmt.Sscanf(event.line, "SCAN %d", &c.lastScan)
			}
			if event.line == "FATAL" {
				t.Fatal("detector failed explicitly")
			}
			if match(event) {
				return event
			}
		case <-timer.C:
			t.Fatal("required detection exceeded deadline")
		}
	}
}

func (c *detectionChild) job(t *testing.T, id string, committed time.Time) {
	t.Helper()
	event := c.until(t, committed.Add(5*time.Second), func(e detectionEvent) bool {
		return e.line == "JOB "+id+" true"
	})
	elapsed := event.at.Sub(committed)
	if elapsed > 5*time.Second {
		t.Fatal("commit-to-observation exceeded five seconds:", elapsed)
	}
	if elapsed < 0 {
		// Already observed between COMMIT and the writer's return to this caller.
		elapsed = 0
	}
	t.Logf("commit-return to observation: %s (async processor unresolved)", elapsed)
}

type detectionCommit struct {
	err error
	at  time.Time
}

func detectionPending(t *testing.T, db *sql.DB) (ApplyResult, chan<- bool, <-chan detectionCommit) {
	t.Helper()
	type prepared struct {
		result ApplyResult
		err    error
	}
	ready := make(chan prepared, 1)
	decision := make(chan bool, 1)
	done := make(chan detectionCommit, 1)
	exited := make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	t.Cleanup(func() { cancel(); <-exited })
	go func() {
		err := settingsWriter(db).Write(ctx, func(v sqlitedb.WriteValue) error {
			result, err := sqliteCreateApplyJobInTransaction(v, "")
			ready <- prepared{result, err}
			if err != nil {
				return err
			}
			select {
			case commit := <-decision:
				if commit {
					return nil
				}
				return fmt.Errorf("fixture rollback")
			case <-ctx.Done():
				return ctx.Err()
			}
		})
		done <- detectionCommit{err, time.Now()}
		close(exited)
	}()
	select {
	case p := <-ready:
		if p.err != nil {
			t.Fatal(p.err)
		}
		return p.result, decision, done
	case failure := <-done:
		t.Fatal("writer failed before pending handshake:", failure.err)
	case <-ctx.Done():
		t.Fatal("pending handshake deadline:", ctx.Err())
	}
	return ApplyResult{}, decision, done
}

func TestSQLiteWorkerDetectionCommittedVisibility(t *testing.T) {
	db, path := enqueueFixture(t)   // Migrated disposable DB + verified default pool.
	first := enqueueCall(t, db, "") // Committed before listener startup (no notification).
	firstCommit := time.Now()
	child := detectionStart(t, path)
	child.job(t, first.Job.ID, firstCommit)
	child.until(t, time.Now().Add(3*time.Second), func(e detectionEvent) bool { return e.line == "READY" })

	for _, commit := range []bool{false, true} {
		pending, decide, done := detectionPending(t, db)
		child.forbidden[pending.Job.ID] = true
		if _, err := io.WriteString(child.input, "checkpoint\n"); err != nil {
			t.Fatal(err)
		}
		child.until(t, time.Now().Add(3*time.Second), func(e detectionEvent) bool { return e.line == "CHECKPOINT" })
		target := child.lastScan + 4
		child.until(t, time.Now().Add(6*time.Second), func(e detectionEvent) bool { return child.lastScan >= target })
		t.Logf("pending ID invisible through %d confirmed scans; commit=%v", 4, commit)
		decide <- commit
		returned := <-done // Parent-process monotonic timestamp at writer/COMMIT return.
		if commit {
			if returned.err != nil {
				t.Fatal(returned.err)
			}
			delete(child.forbidden, pending.Job.ID)
			child.job(t, pending.Job.ID, returned.at)
		} else if returned.err == nil {
			t.Fatal("expected rollback")
		}
	}
	beforeSettings := snapshotSettingsBytes(t, db)
	beforeRevision := enqueueRevisionBytes(t, db)
	var beforeJobs string
	query := "SELECT json_group_array(json_array(id,status,claimed_at,started_at,finished_at)) FROM apply_jobs"
	if err := db.QueryRow(query).Scan(&beforeJobs); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(child.input, "stop\n"); err != nil {
		t.Fatal(err)
	}
	child.until(t, time.Now().Add(3*time.Second), func(e detectionEvent) bool { return e.line == "STOP" })
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := sqlitedb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err := io.WriteString(child.input, "start\n"); err != nil {
		t.Fatal(err)
	}
	child.until(t, time.Now().Add(3*time.Second), func(e detectionEvent) bool {
		return e.line == "JOB "+first.Job.ID+" true"
	})
	t.Log("startup restart rediscovered persistent queued job after producer close/reopen")
	child.until(t, time.Now().Add(3*time.Second), func(e detectionEvent) bool { return e.line == "START" })
	var afterJobs string
	if err := reopened.QueryRow(query).Scan(&afterJobs); err != nil {
		t.Fatal(err)
	}
	if beforeJobs != afterJobs || beforeSettings != snapshotSettingsBytes(t, reopened) || beforeRevision != enqueueRevisionBytes(t, reopened) {
		t.Fatal("read-only detector mutated jobs/settings/revisions")
	}
}
