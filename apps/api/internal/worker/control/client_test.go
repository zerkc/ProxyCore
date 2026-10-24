package control_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/worker/control"
)

func request() control.Request {
	return control.Request{
		RequestID: "req-1", Operation: control.OperationValidate, Service: control.ServiceCoreDNS,
		RevisionID: "rev-1", Checksum: "abc", CandidatePath: "/candidates/rev-1/coredns",
	}
}

// fakeServer accepts connections and runs handle per connection, recording the
// raw request line it received.
type fakeServer struct {
	path   string
	mu     sync.Mutex
	lines  []string
	accept int
}

func newServer(t *testing.T, handle func(line string, conn net.Conn)) *fakeServer {
	t.Helper()
	dir, err := os.MkdirTemp("", "pc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	s := &fakeServer{path: filepath.Join(dir, "c.sock")}
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
			s.mu.Lock()
			s.accept++
			s.mu.Unlock()
			go func() {
				defer conn.Close()
				line, _ := bufio.NewReader(conn).ReadString('\n')
				s.mu.Lock()
				s.lines = append(s.lines, line)
				s.mu.Unlock()
				handle(line, conn)
			}()
		}
	}()
	return s
}

func respond(response string) func(string, net.Conn) {
	return func(_ string, conn net.Conn) { conn.Write([]byte(response)) }
}

func TestRequestLineMatchesTheGoldenWireBytes(t *testing.T) {
	got, err := control.MarshalRequest(request())
	if err != nil {
		t.Fatal(err)
	}
	want := `{"requestId":"req-1","operation":"validate","service":"coredns","revisionId":"rev-1","checksum":"abc","candidatePath":"/candidates/rev-1/coredns"}` + "\n"
	if string(got) != want {
		t.Fatalf("request line =\n%s\nwant\n%s", got, want)
	}
	// HTML-significant characters must stay literal, as JSON.stringify leaves them.
	r := request()
	r.Checksum = "<a>&b</a>"
	got, _ = control.MarshalRequest(r)
	if want := `"checksum":"<a>&b</a>"`; !strings.Contains(string(got), want) {
		t.Fatalf("HTML escaped in %s", got)
	}
}

func TestExecuteRoundTripsAndKeepsOutputBytes(t *testing.T) {
	server := newServer(t, respond(`{"requestId":"req-1","ok":true,"operation":"validate","output":{"big":12345678901234567890,"f":1.50,"s":"<x>"}}`+"\n"))
	client := control.NewClient(server.path)
	response, err := client.Execute(context.Background(), request())
	if err != nil {
		t.Fatal(err)
	}
	if !response.OK || response.RequestID != "req-1" || response.Operation != control.OperationValidate {
		t.Fatalf("response = %+v", response)
	}
	if string(response.Output) != `{"big":12345678901234567890,"f":1.50,"s":"<x>"}` {
		t.Fatalf("output was re-encoded: %s", response.Output)
	}
	if len(server.lines) != 1 || server.lines[0][len(server.lines[0])-1] != '\n' {
		t.Fatalf("server saw %q", server.lines)
	}
}

func TestExecuteDistinguishesAbsentAndNullOutput(t *testing.T) {
	absent := newServer(t, respond(`{"requestId":"req-1","ok":false,"operation":"validate","error":"boom"}`+"\n"))
	got, err := control.NewClient(absent.path).Execute(context.Background(), request())
	if err != nil || got.OK || got.Error != "boom" || got.Output != nil {
		t.Fatalf("failure response = %+v, %v", got, err)
	}
	null := newServer(t, respond(`{"requestId":"req-1","ok":true,"operation":"validate","output":null}`+"\n"))
	got, err = control.NewClient(null.path).Execute(context.Background(), request())
	if err != nil || string(got.Output) != "null" {
		t.Fatalf("null output = %q, %v", got.Output, err)
	}
}

func TestExecuteUsesOneConnectionPerRequest(t *testing.T) {
	server := newServer(t, func(line string, conn net.Conn) {
		var req control.Request
		json.Unmarshal([]byte(line), &req)
		resp, _ := json.Marshal(control.Response{RequestID: req.RequestID, OK: true, Operation: req.Operation})
		conn.Write(append(resp, '\n'))
	})
	client := control.NewClient(server.path)
	for i := 0; i < 3; i++ {
		if _, err := client.Execute(context.Background(), request()); err != nil {
			t.Fatal(err)
		}
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.accept != 3 {
		t.Fatalf("connections = %d, want 3", server.accept)
	}
}

func TestExecuteReadsUntilTheFirstNewlineAcrossChunks(t *testing.T) {
	server := newServer(t, func(_ string, conn net.Conn) {
		conn.Write([]byte(`{"requestId":"req-1","ok":true,`))
		time.Sleep(30 * time.Millisecond)
		conn.Write([]byte(`"operation":"validate"}` + "\n" + `{"ignored":true}` + "\n"))
	})
	got, err := control.NewClient(server.path).Execute(context.Background(), request())
	if err != nil || !got.OK {
		t.Fatalf("chunked response = %+v, %v", got, err)
	}
}

func TestExecuteRejectsMismatchedCorrelation(t *testing.T) {
	cases := []struct{ name, response, want string }{
		{"request id", `{"requestId":"other","ok":true,"operation":"validate"}`, "Control response request ID does not match"},
		{"operation", `{"requestId":"req-1","ok":true,"operation":"reload"}`, "Control response operation does not match"},
		{"server-side parse failure", `{"requestId":"invalid","ok":false,"operation":"health","error":"Control request fields are invalid"}`, "Control response request ID does not match"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			server := newServer(t, respond(tt.response+"\n"))
			_, err := control.NewClient(server.path).Execute(context.Background(), request())
			if err == nil || err.Error() != tt.want {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestExecuteFailureModes(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		server := newServer(t, func(string, net.Conn) { time.Sleep(2 * time.Second) })
		start := time.Now()
		_, err := control.NewClient(server.path, control.WithTimeout(100*time.Millisecond)).Execute(context.Background(), request())
		if err == nil || err.Error() != "Control request timed out" || time.Since(start) > time.Second {
			t.Fatalf("err = %v after %v", err, time.Since(start))
		}
	})
	t.Run("closed before newline", func(t *testing.T) {
		server := newServer(t, respond(`{"requestId":"req-1"`))
		if _, err := control.NewClient(server.path).Execute(context.Background(), request()); err == nil {
			t.Fatal("expected an error for a truncated response")
		}
	})
	t.Run("invalid json", func(t *testing.T) {
		server := newServer(t, respond("not json\n"))
		if _, err := control.NewClient(server.path).Execute(context.Background(), request()); err == nil {
			t.Fatal("expected a decode error")
		}
	})
	t.Run("missing socket", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "none.sock")
		if _, err := control.NewClient(missing).Execute(context.Background(), request()); err == nil {
			t.Fatal("expected a dial error")
		}
	})
	t.Run("context cancelled", func(t *testing.T) {
		server := newServer(t, func(string, net.Conn) { time.Sleep(2 * time.Second) })
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		_, err := control.NewClient(server.path).Execute(ctx, request())
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want the context error", err)
		}
	})
}

func TestOperationsMatchTheControlProtocol(t *testing.T) {
	want := []control.Operation{"stage", "validate", "promote", "reload", "health", "rollback"}
	if len(control.Operations) != len(want) {
		t.Fatalf("Operations = %v", control.Operations)
	}
	for i, op := range want {
		if control.Operations[i] != op {
			t.Fatalf("Operations[%d] = %s, want %s", i, control.Operations[i], op)
		}
	}
	if control.ServiceCoreDNS != "coredns" || control.ServiceNginx != "nginx" {
		t.Fatal("service constants drifted from the control protocol")
	}
}
