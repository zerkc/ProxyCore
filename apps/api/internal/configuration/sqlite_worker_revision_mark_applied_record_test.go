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

func revisionMarkAppliedCommand(t *testing.T, args ...string) (*exec.Cmd, context.CancelFunc) {
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
	runner := filepath.Clean(filepath.Join(filepath.Dir(source), "../../../../packages/db/src/sqlite-foundation/revision-mark-applied-record.bun.ts"))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	cmd := exec.CommandContext(ctx, bun, append([]string{"--no-env-file", runner}, args...)...)
	cmd.Dir = t.TempDir()
	cmd.WaitDelay = time.Second
	return cmd, cancel
}

func TestSQLiteWorkerRevisionMarkAppliedRecordFixture(t *testing.T) {
	db, path := enqueueFixture(t)
	job := enqueueCall(t, db, "").Job
	cmd, cancel := revisionMarkAppliedCommand(t, "unit", path, job.RevisionID)
	defer cancel()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Go-migrated Bun markApplied fixture: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "COMPLETE markApplied cases=") {
		t.Fatalf("missing completed Bun coverage: %s", out)
	}
	t.Logf("embedded Bun cases:\n%s", out)
	var applied, pointer, updated string
	if err := db.QueryRow("SELECT applied_at FROM config_revisions WHERE id=?", job.RevisionID).Scan(&applied); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT current_applied_revision_id,updated_at FROM installation_settings WHERE id='default'").Scan(&pointer, &updated); err != nil || pointer != job.RevisionID || updated != applied {
		t.Fatal("durable revision and pointer must agree:", err, pointer, updated, applied)
	}
}

func revisionMarkAppliedStart(t *testing.T, path, revision string) *claimChild {
	t.Helper()
	cmd, cancel := revisionMarkAppliedCommand(t, "child", path, revision)
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
				t.Errorf("public markApplied child: %v\n%s", err, stderr.String())
			}
		case <-timer.C:
			cancel()
			t.Errorf("public markApplied cleanup deadline: %v", <-finished)
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

func TestSQLiteWorkerRevisionMarkAppliedRecordIndependentProcesses(t *testing.T) {
	db, path := enqueueFixture(t)
	job := enqueueCall(t, db, "").Job
	immutable := func() string {
		t.Helper()
		var raw string
		if err := db.QueryRow(`SELECT json_array(id,revision_number,checksum,snapshot,actor_user_id,
			created_at,source,source_primary_id,source_node_id,source_revision_id,
			snapshot_content_hash,snapshot_version,replication_version,leadership_generation)
			FROM config_revisions WHERE id=?`, job.RevisionID).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		return raw
	}
	state := func() string {
		t.Helper()
		var raw string
		if err := db.QueryRow(`SELECT json_array(r.applied_at,s.current_applied_revision_id,s.updated_at)
			FROM config_revisions r CROSS JOIN installation_settings s WHERE r.id=? AND s.id='default'`, job.RevisionID).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		return raw
	}
	before := immutable()
	first, second := revisionMarkAppliedStart(t, path, job.RevisionID), revisionMarkAppliedStart(t, path, job.RevisionID)
	first.send(t, "mark 1000")
	second.send(t, "mark 2000")
	if one, two := first.read(t), second.read(t); one != "APPLIED 1000" || two != "APPLIED 2000" {
		t.Fatal("both concurrent calls must commit their captured row:", one, two)
	}
	var applied, pointer, updated string
	if err := db.QueryRow(`SELECT r.applied_at,s.current_applied_revision_id,s.updated_at FROM config_revisions r
		CROSS JOIN installation_settings s WHERE r.id=? AND s.id='default'`, job.RevisionID).Scan(&applied, &pointer, &updated); err != nil || applied != updated || pointer != job.RevisionID || (applied != "109223372036854775809.000000000" && applied != "109223372036854775810.000000000") {
		t.Fatal("last atomic writer must leave matching durable clocks/pointer:", err, applied, pointer, updated)
	}
	pending := state()
	first.send(t, "hold")
	if got := first.read(t); got != "HELD" {
		t.Fatal(got)
	}
	start := time.Now()
	second.send(t, "mark 3000")
	if got := second.read(t); got != "BUSY" {
		t.Fatal("BUSY must reject without record publication:", got)
	}
	elapsed := time.Since(start)
	if elapsed < 100*time.Millisecond || elapsed > 2*time.Second {
		t.Fatal("bounded busy_timeout:", elapsed)
	}
	t.Logf("independent-process BUSY elapsed=%s", elapsed)
	if state() != pending || immutable() != before {
		t.Fatal("BUSY changed either product table")
	}
	first.send(t, "release")
	if got := first.read(t); got != "RELEASED" {
		t.Fatal(got)
	}
	second.send(t, "mark 3000")
	if got := second.read(t); got != "APPLIED 3000" {
		t.Fatal("release must allow next write:", got)
	}
	reopened, err := sqlitedb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.QueryRow(`SELECT r.applied_at,s.current_applied_revision_id,s.updated_at FROM config_revisions r
		CROSS JOIN installation_settings s WHERE r.id=? AND s.id='default'`, job.RevisionID).Scan(&applied, &pointer, &updated); err != nil || applied != "109223372036854775811.000000000" || updated != applied || pointer != job.RevisionID {
		t.Fatal("reopened durable revision/pointer:", err, applied, pointer, updated)
	}
	if immutable() != before {
		t.Fatal("markApplied changed immutable revision bytes")
	}
}
