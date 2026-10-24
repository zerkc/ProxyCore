package control_test

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/worker/control"
	"github.com/zerkc/ProxyCore/apps/api/internal/worker/testsupport"
)

func TestRequestBytesMatchBunSerializeControlRequest(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns Bun; skipped with -short")
	}
	testsupport.Bun(t)
	script := testsupport.RepoPath("apps/api/internal/worker/testdata/control-serialize.bun.ts")
	requests := []control.Request{
		request(),
		{RequestID: "9b2c", Operation: control.OperationHealth, Service: control.ServiceNginx,
			RevisionID: "", Checksum: "<a href=\"x\">&amp;</a> 'q'", CandidatePath: "/var/lib/proxycore/candidates/é-日本/nginx"},
		{RequestID: "tab\there", Operation: control.OperationRollback, Service: control.ServiceCoreDNS,
			RevisionID: "line\nbreak \\ \"quote\" \b\f", Checksum: "\u007f\u0001", CandidatePath: "/c"},
	}
	for _, r := range requests {
		payload, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		want := testsupport.RunBun(t, script, string(payload))
		got, err := control.MarshalRequest(r)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Fatalf("Go bytes differ from Bun serializeControlRequest\n go: %q\nbun: %q", got, want)
		}
	}
}

// startBunServer runs the real apps/control server and waits for READY.
func startBunServer(t *testing.T) string {
	t.Helper()
	bun := testsupport.Bun(t)
	dir, err := os.MkdirTemp("", "pc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "control.sock")
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, bun, "--no-env-file", testsupport.RepoPath("apps/api/internal/worker/testdata/control-server.bun.ts"), socket)
	cmd.Dir = dir
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
		line, _ := bufio.NewReader(stdout).ReadString('\n')
		ready <- strings.TrimSpace(line)
	}()
	select {
	case line := <-ready:
		if line != "READY" {
			t.Fatalf("bun control server said %q", line)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("bun control server did not become ready")
	}
	return socket
}

func TestGoClientAgainstTheRealBunControlServer(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns Bun; skipped with -short")
	}
	client := control.NewClient(startBunServer(t))
	ctx := context.Background()

	health := control.Request{RequestID: "req-health", Operation: control.OperationHealth, Service: control.ServiceCoreDNS,
		RevisionID: "rev-1", Checksum: "sum", CandidatePath: "/candidates/rev-1/coredns"}
	got, err := client.Execute(ctx, health)
	if err != nil || !got.OK || got.RequestID != "req-health" || got.Operation != control.OperationHealth {
		t.Fatalf("health = %+v, %v", got, err)
	}
	if string(got.Output) != `{"service":"coredns","healthy":true}` {
		t.Fatalf("health output = %s", got.Output)
	}

	stage := health
	stage.RequestID, stage.Operation = "req-stage", control.OperationStage
	stage.Checksum = "<&>"
	got, err = client.Execute(ctx, stage)
	if err != nil || !got.OK {
		t.Fatalf("stage = %+v, %v", got, err)
	}
	var output struct {
		Echo control.Request `json:"echo"`
		Text string          `json:"text"`
		List []json.Number   `json:"list"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(got.Output)))
	decoder.UseNumber()
	if err := decoder.Decode(&output); err != nil {
		t.Fatalf("decode %s: %v", got.Output, err)
	}
	if output.Echo != stage {
		t.Fatalf("server decoded %+v, want %+v", output.Echo, stage)
	}
	if output.Text != "héllo <b>&</b>   \"quoted\"" {
		t.Fatalf("text = %q", output.Text)
	}
	if len(output.List) != 4 || output.List[3].String() != "1e+21" {
		t.Fatalf("numbers = %v (raw output %s)", output.List, got.Output)
	}

	reload := health
	reload.RequestID, reload.Operation = "req-reload", control.OperationReload
	got, err = client.Execute(ctx, reload)
	if err != nil || !got.OK || string(got.Output) != "null" {
		t.Fatalf("reload (null output) = %+v, %v", got, err)
	}

	failing := health
	failing.RequestID, failing.Operation, failing.Service = "req-validate", control.OperationValidate, control.ServiceNginx
	got, err = client.Execute(ctx, failing)
	if err != nil || got.OK || got.Error != "nginx -t failed: unexpected '}'" || got.Output != nil {
		t.Fatalf("handler failure = %+v, %v", got, err)
	}

	unavailable := health
	unavailable.RequestID, unavailable.Operation = "req-rollback", control.OperationRollback
	got, err = client.Execute(ctx, unavailable)
	if err != nil || got.OK || got.Error != "Operation rollback is unavailable for coredns" {
		t.Fatalf("unavailable operation = %+v, %v", got, err)
	}

	// The server answers a request it rejects with requestId "invalid"; the client
	// surfaces that as a correlation error, exactly like the TS client.
	bad := health
	bad.RequestID, bad.CandidatePath = "req-bad", "relative/../path"
	if _, err := client.Execute(ctx, bad); err == nil || err.Error() != "Control response request ID does not match" {
		t.Fatalf("rejected request err = %v", err)
	}
}
