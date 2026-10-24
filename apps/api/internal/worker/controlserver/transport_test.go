package controlserver

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/worker/control"
)

type lineLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *lineLog) add(line string) { l.mu.Lock(); l.lines = append(l.lines, line); l.mu.Unlock() }
func (l *lineLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

// shortDir keeps the unix socket path under the macOS 104-byte limit.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "pcs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func testHandlers() Handlers {
	return Handlers{
		control.ServiceCoreDNS: {
			control.OperationHealth: func(_ context.Context, r control.Request) (json.RawMessage, error) {
				return json.RawMessage(`{"service":"` + string(r.Service) + `","healthy":true}`), nil
			},
			control.OperationValidate: func(context.Context, control.Request) (json.RawMessage, error) {
				return nil, errors.New("private credential")
			},
		},
	}
}

func startServer(t *testing.T, handlers Handlers, log func(string)) string {
	t.Helper()
	socket := filepath.Join(shortDir(t), "run", "control.sock")
	srv, err := Listen(socket, NewDispatcher(handlers), log)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Serve returned %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Serve did not return after cancel")
		}
	})
	return socket
}

func dial(t *testing.T, socket string) *net.UnixConn {
	t.Helper()
	conn, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	return conn.(*net.UnixConn)
}

func line(t *testing.T, r control.Request) string {
	t.Helper()
	b, err := control.MarshalRequest(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// exchange writes payload, half-closes, and returns every reply line.
func exchange(t *testing.T, socket, payload string) []string {
	t.Helper()
	conn := dial(t, socket)
	if _, err := io.WriteString(conn, payload); err != nil {
		t.Fatal(err)
	}
	if err := conn.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	all, err := io.ReadAll(conn)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) == 0 {
		return nil
	}
	return strings.Split(strings.TrimSuffix(string(all), "\n"), "\n")
}

const invalidPrefix = `{"requestId":"invalid","operation":"health","ok":false,"error":`

func TestListenMkdirFailureLogsBoundedStage(t *testing.T) {
	dir := shortDir(t)
	if err := os.WriteFile(filepath.Join(dir, "blocker"), []byte("file"), 0o600); err != nil {
		t.Fatal(err)
	}
	var log lineLog
	if _, err := Listen(filepath.Join(dir, "blocker", "socket"), NewDispatcher(nil), log.add); err == nil {
		t.Fatal("Listen succeeded")
	}
	if got := log.all(); len(got) != 1 || got[0] != "control socket failed reason=setup_failed stage=mkdir" {
		t.Fatalf("log = %q", got)
	}
}

func TestListenFailureLogsListenStage(t *testing.T) {
	var log lineLog
	// A path longer than sun_path fails at listen, after mkdir succeeds.
	socket := filepath.Join(shortDir(t), strings.Repeat("x", 200))
	if _, err := Listen(socket, NewDispatcher(nil), log.add); err == nil {
		t.Fatal("Listen succeeded")
	}
	if got := log.all(); len(got) != 1 || got[0] != "control socket failed reason=setup_failed stage=listen" {
		t.Fatalf("log = %q", got)
	}
}

func TestListenReplacesStaleSocketFile(t *testing.T) {
	socket := filepath.Join(shortDir(t), "control.sock")
	if err := os.WriteFile(socket, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv, err := Listen(socket, NewDispatcher(nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	srv.Close()
}

func TestFailedOperationLogsOnlySafeFieldsAndKeepsResponse(t *testing.T) {
	var log lineLog
	socket := startServer(t, testHandlers(), log.add)
	got := exchange(t, socket, line(t, control.Request{RequestID: "request-1", Operation: control.OperationValidate,
		Service: control.ServiceCoreDNS, RevisionID: "revision-1", Checksum: "secret-checksum", CandidatePath: "/private/candidate"}))
	want := `{"requestId":"request-1","operation":"validate","ok":false,"error":"private credential"}`
	if len(got) != 1 || got[0] != want {
		t.Fatalf("reply = %q", got)
	}
	logged := log.all()
	if len(logged) != 1 || logged[0] != "control request failed request=request-1 operation=validate service=coredns reason=operation_failed" {
		t.Fatalf("log = %q", logged)
	}
}

func TestRequestIDRedactionInLog(t *testing.T) {
	cases := map[string]string{
		"ok_ID-1":                      "ok_ID-1",
		strings.Repeat("a", 80):        strings.Repeat("a", 80),
		strings.Repeat("a", 81):        "[redacted]",
		"":                             "[redacted]",
		"has space":                    "[redacted]",
		"line\nbreak":                  "[redacted]",
		"secret:token=1":               "[redacted]",
		"trailing-newline-is-denied\n": "[redacted]",
	}
	for id, want := range cases {
		var log lineLog
		socket := startServer(t, testHandlers(), log.add)
		exchange(t, socket, line(t, control.Request{RequestID: id, Operation: control.OperationValidate, Service: control.ServiceCoreDNS, CandidatePath: "/c"}))
		logged := log.all()
		wantLine := "control request failed request=" + want + " operation=validate service=coredns reason=operation_failed"
		if len(logged) != 1 || logged[0] != wantLine {
			t.Fatalf("id %q: log = %q, want %q", id, logged, wantLine)
		}
	}
}

func TestSeveralLinesPerConnectionAnswerInOrder(t *testing.T) {
	var log lineLog
	socket := startServer(t, testHandlers(), log.add)
	health := control.Request{RequestID: "a", Operation: control.OperationHealth, Service: control.ServiceCoreDNS, CandidatePath: "/c"}
	second := health
	second.RequestID = "b"
	payload := line(t, health) + "not json\n" + line(t, second) + `{"requestId":"x"}` + "\n"
	got := exchange(t, socket, payload)
	if len(got) != 4 {
		t.Fatalf("replies = %q", got)
	}
	if got[0] != `{"requestId":"a","operation":"health","ok":true,"output":{"service":"coredns","healthy":true}}` ||
		got[2] != `{"requestId":"b","operation":"health","ok":true,"output":{"service":"coredns","healthy":true}}` {
		t.Fatalf("replies = %q", got)
	}
	if !strings.HasPrefix(got[1], invalidPrefix+`"JSON Parse error: `) ||
		got[3] != invalidPrefix+`"Control request fields are invalid"}` {
		t.Fatalf("invalid replies = %q", got)
	}
	logged := log.all()
	if len(logged) != 2 || logged[0] != "control request failed reason=invalid_json" || logged[1] != "control request failed reason=invalid_request" {
		t.Fatalf("log = %q", logged)
	}
}

func TestSplitWritesAreReassembled(t *testing.T) {
	socket := startServer(t, testHandlers(), nil)
	conn := dial(t, socket)
	payload := line(t, control.Request{RequestID: "a", Operation: control.OperationHealth, Service: control.ServiceCoreDNS, CandidatePath: "/c"})
	for _, part := range []string{payload[:7], payload[7:20], payload[20:]} {
		if _, err := io.WriteString(conn, part); err != nil {
			t.Fatal(err)
		}
	}
	reply, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil || !strings.Contains(reply, `"requestId":"a"`) {
		t.Fatalf("reply = %q, %v", reply, err)
	}
}

func TestClientHalfCloseStillGetsReplyThenServerCloses(t *testing.T) {
	socket := startServer(t, testHandlers(), nil)
	conn := dial(t, socket)
	if _, err := io.WriteString(conn, line(t, control.Request{RequestID: "a", Operation: control.OperationHealth, Service: control.ServiceCoreDNS, CandidatePath: "/c"})); err != nil {
		t.Fatal(err)
	}
	conn.CloseWrite()
	// ReadAll only returns once the server closes its side after the client ended.
	all, err := io.ReadAll(conn)
	if err != nil || !strings.HasPrefix(string(all), `{"requestId":"a"`) || !strings.HasSuffix(string(all), "}\n") {
		t.Fatalf("reply = %q, %v", all, err)
	}
}

func TestPartialLineWithoutNewlineGetsNoReply(t *testing.T) {
	socket := startServer(t, testHandlers(), nil)
	if got := exchange(t, socket, `{"requestId":"a"`); got != nil {
		t.Fatalf("replies = %q", got)
	}
}

func TestOversizedLinesAreRejectedAndConnectionSurvives(t *testing.T) {
	var log lineLog
	socket := startServer(t, testHandlers(), log.add)
	huge := strings.Repeat("a", 5<<20)
	health := control.Request{RequestID: "after", Operation: control.OperationHealth, Service: control.ServiceCoreDNS, CandidatePath: "/c"}
	conn := dial(t, socket)
	writeErr := make(chan error, 1)
	go func() {
		_, err := io.WriteString(conn, `{"x":"`+huge+`"}`+"\n"+line(t, health))
		conn.CloseWrite()
		writeErr <- err
	}()
	all, err := io.ReadAll(conn)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-writeErr; err != nil {
		t.Fatal(err)
	}
	replies := strings.Split(strings.TrimSuffix(string(all), "\n"), "\n")
	if len(replies) != 2 || replies[0] != invalidPrefix+`"Control request is too large"}` || !strings.Contains(replies[1], `"requestId":"after"`) {
		t.Fatalf("replies = %.200q", replies)
	}
	if logged := log.all(); len(logged) != 1 || logged[0] != "control request failed reason=invalid_request" {
		t.Fatalf("log = %q", logged)
	}
}

func TestConcurrentConnections(t *testing.T) {
	socket := startServer(t, testHandlers(), nil)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got := exchange(t, socket, line(t, control.Request{RequestID: "c", Operation: control.OperationHealth, Service: control.ServiceCoreDNS, CandidatePath: "/c"}))
			if len(got) != 1 || !strings.Contains(got[0], `"ok":true`) {
				t.Errorf("reply = %q", got)
			}
		}()
	}
	wg.Wait()
}

func TestExistingGoClientRoundTripsEveryOperation(t *testing.T) {
	handlers := FixedBoundaryHandlers()
	handlers[control.ServiceNginx][control.OperationValidate] = func(context.Context, control.Request) (json.RawMessage, error) {
		return nil, errors.New("nginx -t failed: unexpected '}'")
	}
	delete(handlers[control.ServiceNginx], control.OperationRollback)
	socket := startServer(t, handlers, nil)
	client := control.NewClient(socket)
	for _, svc := range []control.Service{control.ServiceCoreDNS, control.ServiceNginx} {
		for _, op := range control.Operations {
			in := control.Request{RequestID: "id-" + string(op), Operation: op, Service: svc, RevisionID: "rev-1", Checksum: "sum", CandidatePath: "/candidates/rev-1/" + string(svc)}
			got, err := client.Execute(context.Background(), in)
			if err != nil {
				t.Fatalf("%s/%s: %v", svc, op, err)
			}
			switch {
			case svc == control.ServiceNginx && op == control.OperationValidate:
				if got.OK || got.Error != "nginx -t failed: unexpected '}'" || got.Output != nil {
					t.Fatalf("validate failure = %+v", got)
				}
			case svc == control.ServiceNginx && op == control.OperationRollback:
				if got.OK || got.Error != "Operation rollback is unavailable for nginx" {
					t.Fatalf("unavailable = %+v", got)
				}
			default:
				want := `{"service":"` + string(svc) + `","operation":"` + string(op) + `","candidatePath":"/candidates/rev-1/` + string(svc) + `","status":"accepted-by-fixed-boundary"}`
				if !got.OK || string(got.Output) != want {
					t.Fatalf("%s/%s = %+v", svc, op, got)
				}
			}
		}
	}
	bad := control.Request{RequestID: "req-bad", Operation: control.OperationHealth, Service: control.ServiceNginx, CandidatePath: "relative/../path"}
	if _, err := client.Execute(context.Background(), bad); err == nil || err.Error() != "Control response request ID does not match" {
		t.Fatalf("rejected request err = %v", err)
	}
}

func TestServeStopsOnCancelAndClosesConnections(t *testing.T) {
	socket := filepath.Join(shortDir(t), "control.sock")
	srv, err := Listen(socket, NewDispatcher(testHandlers()), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	conn := dial(t, socket)
	// The connection is idle; prove the server is serving it, then cancel.
	io.WriteString(conn, line(t, control.Request{RequestID: "a", Operation: control.OperationHealth, Service: control.ServiceCoreDNS, CandidatePath: "/c"}))
	if _, err := bufio.NewReader(conn).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return")
	}
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("idle connection stayed open after shutdown")
	}
}
