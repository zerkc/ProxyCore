package controlserver

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/worker/control"
	"github.com/zerkc/ProxyCore/apps/api/internal/worker/testsupport"
)

// bunHandlers mirrors the fixed handlers of testdata/control-server.bun.ts.
func bunHandlers() Handlers {
	return Handlers{
		control.ServiceCoreDNS: {
			control.OperationHealth: func(_ context.Context, r control.Request) (json.RawMessage, error) {
				return json.RawMessage(`{"service":` + quoteJS(string(r.Service)) + `,"healthy":true}`), nil
			},
			control.OperationStage: func(_ context.Context, r control.Request) (json.RawMessage, error) {
				echo := `{"requestId":` + quoteJS(r.RequestID) + `,"operation":` + quoteJS(string(r.Operation)) +
					`,"service":` + quoteJS(string(r.Service)) + `,"revisionId":` + quoteJS(r.RevisionID) +
					`,"checksum":` + quoteJS(r.Checksum) + `,"candidatePath":` + quoteJS(r.CandidatePath) + `}`
				text := quoteJS("héllo <b>&</b>   \"quoted\"")
				return json.RawMessage(`{"echo":` + echo + `,"text":` + text + `,"list":[1,2.5,-3,1e+21]}`), nil
			},
			control.OperationReload: func(context.Context, control.Request) (json.RawMessage, error) {
				return json.RawMessage("null"), nil
			},
		},
		control.ServiceNginx: {
			control.OperationValidate: func(context.Context, control.Request) (json.RawMessage, error) {
				return nil, errors.New("nginx -t failed: unexpected '}'")
			},
		},
	}
}

func startBunServer(t *testing.T) string {
	t.Helper()
	bun := testsupport.Bun(t)
	socket := filepath.Join(shortDir(t), "bun.sock")
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, bun, "--no-env-file", testsupport.RepoPath("apps/api/internal/worker/testdata/control-server.bun.ts"), socket)
	cmd.Dir = filepath.Dir(socket)
	cmd.WaitDelay = time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); cmd.Wait() })
	ready := make(chan string, 1)
	go func() {
		text, _ := bufio.NewReader(stdout).ReadString('\n')
		ready <- strings.TrimSpace(text)
	}()
	select {
	case text := <-ready:
		if text != "READY" {
			t.Fatalf("bun control server said %q", text)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("bun control server did not become ready")
	}
	return socket
}

func rawExchange(t *testing.T, socket, payload string) string {
	t.Helper()
	conn, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(15 * time.Second))
	if _, err := io.WriteString(conn, payload); err != nil {
		t.Fatal(err)
	}
	conn.(*net.UnixConn).CloseWrite()
	all, err := io.ReadAll(conn)
	if err != nil {
		t.Fatal(err)
	}
	return string(all)
}

// TestRepliesAreByteIdenticalToTheBunServer sends the same lines to the real
// apps/control server and to this one and compares the raw reply bytes.
func TestRepliesAreByteIdenticalToTheBunServer(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns Bun; skipped with -short")
	}
	bunSocket := startBunServer(t)
	goSocket := startServer(t, bunHandlers(), nil)
	mk := func(id string, op control.Operation, svc control.Service, checksum, path string) string {
		return line(t, control.Request{RequestID: id, Operation: op, Service: svc, RevisionID: "rev-1", Checksum: checksum, CandidatePath: path})
	}
	lines := []string{
		mk("req-health", control.OperationHealth, control.ServiceCoreDNS, "sum", "/candidates/rev-1"),
		mk("req-stage", control.OperationStage, control.ServiceCoreDNS, "<&> 'q'   \x01\x1f\u007f", "/c/é-日本😀"),
		mk("req-reload", control.OperationReload, control.ServiceCoreDNS, "", "/c"),
		mk("req-validate", control.OperationValidate, control.ServiceNginx, "", "/c"),
		mk("req-rollback", control.OperationRollback, control.ServiceCoreDNS, "", "/c"),
		mk("tab\there \"q\" \\", control.OperationHealth, control.ServiceCoreDNS, "", "/c"),
		mk("bad", control.OperationHealth, control.ServiceCoreDNS, "", "relative/../path"),
		`{"requestId":"r","operation":"exec","service":"nginx","command":"rm -rf /"}` + "\n",
		`{"requestId":"r","operation":"exec","service":"nginx","revisionId":"","checksum":"","candidatePath":"/c"}` + "\n",
		`{"requestId":"r","operation":"health","service":"redis","revisionId":"","checksum":"","candidatePath":"/c"}` + "\n",
		`{"requestId":"r","operation":"health","service":"nginx","revisionId":"","checksum":"","candidatePath":"/c\u0000"}` + "\n",
		`{"requestId":"r","operation":"health"}` + "\n",
		`[]` + "\n",
		`null` + "\n",
		`"s"` + "\n",
		`5` + "\n",
		"\n",
		"nul\n",
		"[1,\n",
		"{\n",
		"[\n",
		"x\n",
		`{"requestId":"r","extra":1,"requestId":"dup","operation":"health","service":"coredns","revisionId":"","checksum":"","candidatePath":"/c"}` + "\n",
		`{"x":"` + strings.Repeat("a", 70_000) + `"}` + "\n",
		`{"requestId":"r"` + "\n",
	}
	for i, in := range lines {
		want := rawExchange(t, bunSocket, in)
		got := rawExchange(t, goSocket, in)
		if got != want {
			t.Errorf("line %d %.80q\n go: %.300q\nbun: %.300q", i, in, got, want)
		}
	}
	// All lines on one connection keep their order too.
	all := strings.Join(lines[:7], "")
	if got, want := rawExchange(t, goSocket, all), rawExchange(t, bunSocket, all); got != want {
		t.Errorf("multi-line exchange differs\n go: %q\nbun: %q", got, want)
	}
}
