package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/worker/control"
)

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

func lookup(env map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		value, ok := env[key]
		return value, ok
	}
}

func socketIn(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "pcc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "sub", "control.sock")
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRunServesFallbackBackendAndShutsDownCleanly(t *testing.T) {
	socket := socketIn(t)
	var stdout, stderr safeBuffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- run(ctx, lookup(map[string]string{"WORKER_SOCKET_PATH": socket}), &stdout, &stderr) }()

	waitFor(t, "startup line", func() bool { return strings.Contains(stdout.String(), "listening on") })
	if got := stdout.String(); got != "proxycore-control listening on "+socket+"\n" {
		t.Fatalf("stdout = %q", got)
	}
	got, err := control.NewClient(socket).Execute(context.Background(), control.Request{
		RequestID: "r-1", Operation: control.OperationPromote, Service: control.ServiceNginx,
		RevisionID: "rev", Checksum: "sum", CandidatePath: "/candidates/rev/nginx"})
	if err != nil || !got.OK {
		t.Fatalf("Execute = %+v, %v", got, err)
	}
	want := `{"service":"nginx","operation":"promote","candidatePath":"/candidates/rev/nginx","status":"accepted-by-fixed-boundary"}`
	if string(got.Output) != want {
		t.Fatalf("output = %s", got.Output)
	}

	// An open idle connection must not block shutdown.
	idle, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer idle.Close()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return after cancel")
	}
	if stderr.String() != "" {
		t.Fatalf("shutdown wrote to stderr: %q", stderr.String())
	}
	if _, err := net.Dial("unix", socket); err == nil {
		t.Fatal("socket still accepts connections after shutdown")
	}
}

func TestRunLogsInvalidRequestWithTimestamp(t *testing.T) {
	socket := socketIn(t)
	var stdout, stderr safeBuffer
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx, lookup(map[string]string{"WORKER_SOCKET_PATH": socket}), &stdout, &stderr) }()
	waitFor(t, "startup line", func() bool { return strings.Contains(stdout.String(), "listening on") })
	conn, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.Write([]byte("{\n"))
	buf := make([]byte, 4096)
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	n, err := conn.Read(buf)
	if err != nil || !json.Valid(buf[:n-1]) {
		t.Fatalf("reply = %q, %v", buf[:n], err)
	}
	waitFor(t, "log line", func() bool { return stderr.String() != "" })
	cancel()
	<-done
	line := strings.TrimSuffix(stderr.String(), "\n")
	stamp, message, _ := strings.Cut(line, " ")
	if _, err := time.Parse("2006-01-02T15:04:05.000Z", stamp); err != nil || message != "control request failed reason=invalid_json" {
		t.Fatalf("log line = %q", line)
	}
}

func TestRunReturnsListenFailure(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	os.WriteFile(blocker, []byte("x"), 0o600)
	var stdout, stderr safeBuffer
	err := run(context.Background(), lookup(map[string]string{"WORKER_SOCKET_PATH": filepath.Join(blocker, "s")}), &stdout, &stderr)
	if err == nil {
		t.Fatal("run succeeded")
	}
	if !strings.HasSuffix(stderr.String(), " control socket failed reason=setup_failed stage=mkdir\n") || stdout.String() != "" {
		t.Fatalf("stdout %q stderr %q", stdout.String(), stderr.String())
	}
}

func TestDefaultSocketPath(t *testing.T) {
	if got := socketPath(lookup(nil)); got != "/run/proxycore/control.sock" {
		t.Fatalf("default = %q", got)
	}
	if got := socketPath(lookup(map[string]string{"WORKER_SOCKET_PATH": ""})); got != "" {
		t.Fatalf("empty override = %q (TS ?? keeps the empty string)", got)
	}
}
