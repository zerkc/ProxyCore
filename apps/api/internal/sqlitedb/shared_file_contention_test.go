package sqlitedb

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

// TestSQLiteGoWriterAndBunWorkerSharedFileContention runs the API-side writer
// discipline (Writer, BEGIN IMMEDIATE) from several goroutines while the worker's
// real Bun path (claim + status update + wakeup cursor) runs against the same WAL file.
// Go enqueues queued apply jobs; Bun claims and applies every one of them.
func TestSQLiteGoWriterAndBunWorkerSharedFileContention(t *testing.T) {
	bun, err := exec.LookPath("bun")
	if err != nil {
		t.Skip("bun not on PATH; shared-file contention gate skipped")
	}
	bun, err = filepath.Abs(bun)
	writeOK(t, err)
	_, source, _, _ := runtime.Caller(0)
	script := filepath.Clean(filepath.Join(filepath.Dir(source), "../../../..", "packages/db/src/sqlite-foundation/shared-file-contention.bun.ts"))

	dir := t.TempDir()
	path := filepath.Join(dir, "contention.sqlite")
	db, err := Open(path)
	writeOK(t, err)
	t.Cleanup(func() { db.Close() })
	writeOK(t, Migrate(context.Background(), db))
	w := NewWriter(db, nil)

	var revisionID string
	writeOK(t, w.Write(context.Background(), func(v WriteValue) error {
		id, err := v.NewUUID()
		if err != nil {
			return err
		}
		revisionID = id
		_, err = v.Conn().ExecContext(v.Context(),
			`INSERT INTO config_revisions (id, revision_number, checksum, snapshot, created_at) VALUES (?, 1, 'contention', '{}', ?)`,
			id, v.Timestamp())
		return err
	}))

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	stopFile := filepath.Join(dir, "stop")
	cmd := exec.CommandContext(ctx, bun, "--no-env-file", script, path, stopFile)
	cmd.Dir = dir
	var bunOut, bunErr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &bunOut, &bunErr
	writeOK(t, cmd.Start())
	bunDone := make(chan error, 1)
	go func() { bunDone <- cmd.Wait() }()

	const goWriters = 4
	const contention = 3 * time.Second
	deadline := time.Now().Add(contention)
	counts := make([]int, goWriters)
	errs := make([]error, goWriters)
	var wg sync.WaitGroup
	for i := 0; i < goWriters; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for time.Now().Before(deadline) {
				err := w.Write(ctx, func(v WriteValue) error {
					id, err := v.NewUUID()
					if err != nil {
						return err
					}
					correlation, err := v.NewUUID()
					if err != nil {
						return err
					}
					_, err = v.Conn().ExecContext(v.Context(),
						`INSERT INTO apply_jobs (id, revision_id, target, status, source, correlation_id, created_at)
						 VALUES (?, ?, 'combined', 'queued', 'ordinary', ?, ?)`,
						id, revisionID, correlation, v.Timestamp())
					return err
				})
				if err != nil {
					errs[i] = err
					return
				}
				counts[i]++
				if counts[i]%25 == 0 {
					var observed int
					if err := db.QueryRowContext(ctx, "SELECT count(*) FROM apply_jobs").Scan(&observed); err != nil {
						errs[i] = err
						return
					}
				}
			}
		}(i)
	}
	wg.Wait()
	// Writers have finished (or failed): release the worker so it drains and exits.
	writeOK(t, os.WriteFile(stopFile, nil, 0o600))
	bunErrValue := <-bunDone

	goTotal := 0
	for i, err := range errs {
		if err != nil {
			t.Fatalf("Go writer %d: %v", i, err)
		}
		goTotal += counts[i]
	}
	if bunErrValue != nil {
		t.Fatalf("Bun worker: %v\nstdout: %s\nstderr: %s", bunErrValue, bunOut.String(), bunErr.String())
	}
	var summary struct {
		Claimed     int `json:"claimed"`
		Applied     int `json:"applied"`
		CursorReads int `json:"cursorReads"`
	}
	writeOK(t, json.Unmarshal(bytes.TrimSpace(bunOut.Bytes()), &summary))
	t.Logf("writes: go_enqueued=%d per_writer=%v bun_claimed=%d bun_applied=%d bun_cursor_reads=%d",
		goTotal, counts, summary.Claimed, summary.Applied, summary.CursorReads)
	if goTotal == 0 {
		t.Fatal("Go writers made no progress")
	}
	if summary.Claimed != goTotal || summary.Applied != goTotal {
		t.Fatalf("Bun claimed=%d applied=%d, want %d each", summary.Claimed, summary.Applied, goTotal)
	}

	var total, applied, queued, withHealth int
	writeOK(t, db.QueryRow(`SELECT count(*),
		coalesce(sum(status = 'applied'), 0),
		coalesce(sum(status = 'queued'), 0),
		coalesce(sum(status = 'applied' AND finished_at IS NOT NULL AND json_extract(health_output, '$.observedBy') = 'worker'), 0)
		FROM apply_jobs`).Scan(&total, &applied, &queued, &withHealth))
	if total != goTotal || applied != goTotal || queued != 0 || withHealth != goTotal {
		t.Fatalf("durable jobs total=%d applied=%d queued=%d applied_with_health=%d, want total=applied=applied_with_health=%d queued=0",
			total, applied, queued, withHealth, goTotal)
	}

	var integrity string
	writeOK(t, db.QueryRow("PRAGMA integrity_check").Scan(&integrity))
	if integrity != "ok" {
		t.Fatalf("integrity_check=%q", integrity)
	}
	fkRows, err := db.Query("PRAGMA foreign_key_check")
	writeOK(t, err)
	fkViolations := 0
	for fkRows.Next() {
		fkViolations++
	}
	writeOK(t, fkRows.Err())
	writeOK(t, fkRows.Close())
	if fkViolations != 0 {
		t.Fatalf("foreign_key_check reported %d violations", fkViolations)
	}

	var busy, logFrames, checkpointed int
	writeOK(t, db.QueryRow("PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logFrames, &checkpointed))
	if busy != 0 || logFrames != checkpointed {
		t.Fatalf("wal_checkpoint(TRUNCATE) busy=%d log=%d checkpointed=%d", busy, logFrames, checkpointed)
	}
	if info, err := os.Stat(path + "-wal"); err == nil && info.Size() != 0 {
		t.Fatalf("-wal not truncated after checkpoint: %d bytes", info.Size())
	}
}
