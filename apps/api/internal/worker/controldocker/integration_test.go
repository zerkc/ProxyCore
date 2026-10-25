package controldocker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zerkc/ProxyCore/apps/api/internal/worker/control"
)

// Real-daemon test, skipped unless PROXYCORE_DOCKER_IT=1. It needs the docker
// CLI, a reachable daemon and the images nginx:alpine and coredns/coredns
// (DOCKER_SOCKET_PATH selects the socket). It creates and removes its own
// uniquely named containers.

func dockerCLI(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func integrationBackend(t *testing.T) (layout, *Client) {
	t.Helper()
	if os.Getenv("PROXYCORE_DOCKER_IT") != "1" {
		t.Skip("set PROXYCORE_DOCKER_IT=1 to run against a real Docker daemon")
	}
	socket := os.Getenv("DOCKER_SOCKET_PATH")
	if socket == "" {
		socket = DefaultSocketPath
	}
	client, err := NewClient(socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	l := newLayout(t)
	return l, client
}

func TestIntegrationNginxPromoteHealthRollback(t *testing.T) {
	l, client := integrationBackend(t)
	name := "proxycore-it-nginx-" + strings.ReplaceAll(filepath.Base(l.root), "/", "")
	dockerCLI(t, "run", "-d", "--name", name, "-v", l.candidates+":"+l.candidates, "nginx:alpine")
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", name).Run() })
	b := New(client, Options{CandidateRoot: l.candidates, ZonesRoot: l.zones, ConfigRoot: l.config,
		Containers: Containers{CoreDNS: "unused", Nginx: name}})

	managed := "events {}\nhttp {\n    map $http_upgrade $connection_upgrade { default upgrade; }\n    server { listen 8081; location / { return 200 \"ok\"; } }\n}\n"
	dir := l.candidate(t, "rev-1", map[string]string{"nginx.conf": managed})
	rq := func(op control.Operation) control.Request { return request(control.ServiceNginx, op, dir) }

	// Before promote the baked default must be reported as drift.
	if out := mustRun(t, b, rq(control.OperationHealth)); !strings.Contains(out, `"status":"drift"`) {
		t.Fatalf("pre-promote health = %s", out)
	}
	if out := mustRun(t, b, rq(control.OperationValidate)); out != `{"status":"validated","checksum":"sum-1"}` {
		t.Fatal(out)
	}
	mustRun(t, b, rq(control.OperationPromote))
	mustRun(t, b, rq(control.OperationReload))
	if out := mustRun(t, b, rq(control.OperationHealth)); out != `{"status":"healthy","revisionId":"rev-1"}` {
		t.Fatalf("post-promote health = %s", out)
	}
	if got := dockerCLI(t, "exec", name, "cat", "/etc/nginx/nginx.conf"); got != strings.TrimSpace(managed) {
		t.Fatalf("container config = %q", got)
	}
	mustRun(t, b, rq(control.OperationRollback))
	if out := mustRun(t, b, rq(control.OperationHealth)); !strings.Contains(out, `"status":"drift"`) {
		t.Fatalf("post-rollback health = %s", out)
	}
	if _, err := run(t, b, request(control.ServiceNginx, control.OperationValidate, l.candidates+"/rev-1/../x")); err == nil {
		t.Fatal("path outside root accepted")
	}
}

func TestIntegrationCoreDNSPromoteHealthRollback(t *testing.T) {
	l, client := integrationBackend(t)
	name := "proxycore-it-coredns-" + strings.ReplaceAll(filepath.Base(l.root), "/", "")
	write(t, filepath.Join(l.config, "Corefile"), ".:53 {\n    forward . 1.1.1.1\n}\n", 0o644)
	dockerCLI(t, "run", "-d", "--name", name, "-v", l.config+":/etc/coredns-config", "coredns/coredns", "-conf", "/etc/coredns-config/Corefile")
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", name).Run() })
	b := New(client, Options{CandidateRoot: l.candidates, ZonesRoot: l.zones, ConfigRoot: l.config,
		Containers: Containers{CoreDNS: name, Nginx: "unused"}})

	dir := l.candidate(t, "rev-1", map[string]string{"Corefile": ".:53 {\n    forward . 8.8.8.8\n}\n", "zones/a.zone": "zone\n"})
	rq := func(op control.Operation) control.Request { return request(control.ServiceCoreDNS, op, dir) }
	mustRun(t, b, rq(control.OperationPromote))
	if read(t, filepath.Join(l.config, "Corefile")) != ".:53 {\n    forward . 8.8.8.8\n}\n" {
		t.Fatal("live Corefile not persisted")
	}
	got, err := client.GetArchive(context.Background(), name, "/etc/coredns-config/Corefile")
	if err != nil {
		t.Fatal(err)
	}
	if file, err := extractFirstFile(got); err != nil || !strings.Contains(string(file), "8.8.8.8") {
		t.Fatalf("container Corefile = %q, %v", file, err)
	}
	mustRun(t, b, rq(control.OperationReload))
	if out := mustRun(t, b, rq(control.OperationHealth)); out != `{"status":"healthy"}` {
		t.Fatal(out)
	}
	mustRun(t, b, rq(control.OperationRollback))
	if read(t, filepath.Join(l.config, "Corefile")) != ".:53 {\n    forward . 1.1.1.1\n}\n" {
		t.Fatal("rollback did not restore the previous Corefile")
	}
}
