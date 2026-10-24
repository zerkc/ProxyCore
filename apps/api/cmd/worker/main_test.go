package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
	"github.com/zerkc/ProxyCore/apps/api/internal/worker/config"
	"github.com/zerkc/ProxyCore/apps/api/internal/worker/schemawait"
	"github.com/zerkc/ProxyCore/apps/api/internal/worker/testsupport"
)

// safeBuffer is a concurrency-safe io.Writer for capturing log output.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func envLookup(env map[string]string) config.Lookup {
	return func(key string) (string, bool) {
		value, ok := env[key]
		return value, ok
	}
}

// controlServer answers every control request ok on a unix socket; validate
// blocks until release is closed when block is set.
type controlServer struct {
	path    string
	block   chan struct{}
	entered chan struct{}
	once    sync.Once
	mu      sync.Mutex
	ops     []string
}

func startControl(t *testing.T, block bool) *controlServer {
	t.Helper()
	dir, err := os.MkdirTemp("", "pcw")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	s := &controlServer{path: filepath.Join(dir, "c.sock"), entered: make(chan struct{})}
	if block {
		s.block = make(chan struct{})
	}
	listener, err := net.Listen("unix", s.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go s.serve(conn)
		}
	}()
	return s
}

func (s *controlServer) serve(conn net.Conn) {
	defer conn.Close()
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return
	}
	var request struct {
		RequestID, Operation, Service string
	}
	var raw map[string]string
	if json.Unmarshal(line, &raw) != nil {
		return
	}
	request.RequestID, request.Operation, request.Service = raw["requestId"], raw["operation"], raw["service"]
	s.mu.Lock()
	s.ops = append(s.ops, request.Service+":"+request.Operation)
	s.mu.Unlock()
	if request.Operation == "validate" && s.block != nil {
		s.once.Do(func() { close(s.entered) })
		<-s.block
	}
	response, _ := json.Marshal(map[string]any{
		"requestId": request.RequestID, "ok": true, "operation": request.Operation,
		"output": map[string]string{"status": "healthy"},
	})
	conn.Write(append(response, '\n'))
}

func (s *controlServer) operations() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.ops...)
}

type workerEnv struct {
	env        map[string]string
	heartbeat  string
	candidates string
	stdout     *safeBuffer
	stderr     *safeBuffer
}

func newWorkerEnv(t *testing.T, socket string) (*workerEnv, string) {
	t.Helper()
	db, path := testsupport.NewDB(t)
	root := t.TempDir()
	w := &workerEnv{
		heartbeat: filepath.Join(root, "run", "worker.heartbeat"), candidates: filepath.Join(root, "candidates"),
		stdout: &safeBuffer{}, stderr: &safeBuffer{},
	}
	w.env = map[string]string{
		"PROXYCORE_SQLITE_PATH": path, "WORKER_SOCKET_PATH": socket,
		"WORKER_HEARTBEAT_PATH": w.heartbeat, "WORKER_CANDIDATE_ROOT": w.candidates,
		"WORKER_RECONCILIATION_INTERVAL_MS": "1500",
	}
	// Seed one revision with a renderable snapshot and a queued coredns job.
	snapshot := `{"settings":{"ingress":{"ipv4":"192.0.2.10"},"defaultPool":{"id":"d","endpoints":[{"host":"1.1.1.1","port":53}]},
	  "forwardingRules":[],"retentionMaxAgeDays":7,"retentionMaxSizeMb":50},"zones":[],"streams":[],"certificates":[]}`
	if _, err := db.Exec(`insert into config_revisions (id,revision_number,checksum,snapshot,created_at) values (?,?,?,?,?)`,
		testsupport.UUID(1), 1, "checksum-1", snapshot, "0"); err != nil {
		t.Fatal(err)
	}
	return w, testsupport.UUID(1)
}

func (w *workerEnv) start(t *testing.T, options runOptions) (cancel func(), done chan error) {
	t.Helper()
	ctx, cancelCtx := context.WithCancel(context.Background())
	done = make(chan error, 1)
	go func() { done <- run(ctx, envLookup(w.env), w.stdout, w.stderr, options) }()
	t.Cleanup(cancelCtx)
	return cancelCtx, done
}

func waitFor(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func jobStatus(t *testing.T, path string) string {
	t.Helper()
	db, err := sqlitedb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var status string
	if err := db.QueryRow("select status from apply_jobs limit 1").Scan(&status); err != nil {
		return ""
	}
	return status
}

func TestRunRequiresTheSQLitePath(t *testing.T) {
	var out, errOut safeBuffer
	err := run(context.Background(), envLookup(map[string]string{}), &out, &errOut, runOptions{})
	if err == nil || err.Error() != "PROXYCORE_SQLITE_PATH is required" {
		t.Fatalf("err = %v", err)
	}
}

func TestRunNeverCreatesTheDatabaseAndReportsTheSchemaWait(t *testing.T) {
	root := t.TempDir()
	missing := filepath.Join(root, "missing.sqlite")
	var out, errOut safeBuffer
	err := run(context.Background(), envLookup(map[string]string{
		"PROXYCORE_SQLITE_PATH": missing, "WORKER_HEARTBEAT_PATH": filepath.Join(root, "hb"),
	}), &out, &errOut, runOptions{schema: schemawait.Options{Wait: 80 * time.Millisecond, Poll: 10 * time.Millisecond}})
	if err == nil || !strings.Contains(err.Error(), "sqlite schema not ready after 80ms") {
		t.Fatalf("err = %v", err)
	}
	if _, statErr := os.Stat(missing); !os.IsNotExist(statErr) {
		t.Fatalf("the worker created the database: %v", statErr)
	}
	if !strings.Contains(out.String(), "waiting for sqlite schema version 9") {
		t.Fatalf("stdout = %q", out.String())
	}
	// The heartbeat is written before the schema wait so the healthcheck passes.
	if _, statErr := os.Stat(filepath.Join(root, "hb")); statErr != nil {
		t.Fatalf("heartbeat: %v", statErr)
	}
}

var isoPrefix = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z `)

func TestRunAppliesAQueuedJobAndShutsDownCleanly(t *testing.T) {
	server := startControl(t, false)
	w, revisionID := newWorkerEnv(t, server.path)
	db, err := sqlitedb.Open(w.env["PROXYCORE_SQLITE_PATH"])
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`insert into apply_jobs (id,revision_id,target,status,correlation_id,created_at) values (?,?,?,?,?,?)`,
		testsupport.UUID(2), revisionID, "coredns", "queued", "corr-1", "0"); err != nil {
		t.Fatal(err)
	}
	cancel, done := w.start(t, runOptions{wakeupPoll: 20 * time.Millisecond})
	waitFor(t, "job applied", func() bool { return jobStatus(t, w.env["PROXYCORE_SQLITE_PATH"]) == "applied" })

	for _, line := range strings.Split(strings.TrimSpace(w.stdout.String()), "\n") {
		if !isoPrefix.MatchString(line) {
			t.Fatalf("stdout line without ISO prefix: %q", line)
		}
	}
	if !strings.Contains(w.stdout.String(), "proxycore-worker listening for jobs; reconciling every 1500ms") {
		t.Fatalf("stdout = %q", w.stdout.String())
	}
	if _, err := os.Stat(filepath.Join(w.candidates, revisionID, "coredns", "Corefile")); err != nil {
		t.Fatalf("candidate not written: %v", err)
	}
	beat, err := os.ReadFile(w.heartbeat)
	if err != nil || !regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$`).Match(beat) {
		t.Fatalf("heartbeat = %q, %v", beat, err)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return after cancellation")
	}
	if strings.Contains(w.stderr.String(), "error") {
		t.Fatalf("stderr = %q", w.stderr.String())
	}
}

func TestHeartbeatKeepsTickingWhileAnApplyIsStuck(t *testing.T) {
	server := startControl(t, true)
	w, revisionID := newWorkerEnv(t, server.path)
	db, err := sqlitedb.Open(w.env["PROXYCORE_SQLITE_PATH"])
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`insert into apply_jobs (id,revision_id,target,status,correlation_id,created_at) values (?,?,?,?,?,?)`,
		testsupport.UUID(2), revisionID, "coredns", "queued", "corr-1", "0"); err != nil {
		t.Fatal(err)
	}
	cancel, done := w.start(t, runOptions{heartbeatInterval: 20 * time.Millisecond, wakeupPoll: 20 * time.Millisecond})
	select {
	case <-server.entered:
	case <-time.After(15 * time.Second):
		t.Fatal("the apply never reached the control server")
	}
	first, err := os.ReadFile(w.heartbeat)
	if err != nil {
		t.Fatal(err)
	}
	// The poller goroutine is blocked inside validate; only the ticker can move this.
	waitFor(t, "heartbeat to advance during the stuck apply", func() bool {
		next, err := os.ReadFile(w.heartbeat)
		return err == nil && string(next) != string(first)
	})
	close(server.block)
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return")
	}
}

func TestHeartbeatWritesAnISOTimestampAndCreatesItsDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "run", "hb")
	fixed := time.Date(2026, 5, 6, 7, 8, 9, 123_000_000, time.FixedZone("x", 3600))
	h := heartbeat{path: path, now: func() time.Time { return fixed }}
	if err := h.prepare(); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "2026-05-06T06:08:09.123Z" {
		t.Fatalf("heartbeat = %q", got)
	}
}

func TestHeartbeatTickStopsOnCancelAndReportsFailures(t *testing.T) {
	h := heartbeat{path: filepath.Join(t.TempDir(), "missing-dir", "hb"), now: time.Now}
	ctx, cancel := context.WithCancel(context.Background())
	errs := make(chan error, 8)
	finished := make(chan struct{})
	go func() {
		h.tick(ctx, 5*time.Millisecond, func(err error) { errs <- err })
		close(finished)
	}()
	select {
	case <-errs:
	case <-time.After(5 * time.Second):
		t.Fatal("no error reported")
	}
	cancel()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("tick did not stop")
	}
}
