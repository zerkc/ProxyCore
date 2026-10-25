package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/zerkc/ProxyCore/apps/api/internal/worker/control"
	"github.com/zerkc/ProxyCore/apps/api/internal/worker/controldocker"
)

type recordingDocker struct {
	mu         sync.Mutex
	restarts   []string
	restartErr error
	running    bool
}

func (d *recordingDocker) Running(context.Context, string) (bool, error) { return d.running, nil }
func (d *recordingDocker) Restart(_ context.Context, name string, seconds int) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.restarts = append(d.restarts, name+":"+string(rune('0'+seconds)))
	return d.restartErr
}
func (d *recordingDocker) Exec(context.Context, string, []string) (int, error) { return 0, nil }
func (d *recordingDocker) PutArchive(context.Context, string, string, []byte) error {
	return nil
}
func (d *recordingDocker) GetArchive(context.Context, string, string) ([]byte, error) {
	return nil, errors.New("no such file")
}

type dockerEnv struct {
	socket, candidates, config string
	env                        map[string]string
}

func newDockerEnv(t *testing.T) dockerEnv {
	t.Helper()
	root := t.TempDir()
	e := dockerEnv{socket: socketIn(t), candidates: filepath.Join(root, "candidates"), config: filepath.Join(root, "config")}
	e.env = map[string]string{
		"WORKER_SOCKET_PATH": e.socket, "CONTROL_BACKEND": "docker",
		"CANDIDATE_ROOT": e.candidates, "COREDNS_CONFIG_ROOT": e.config, "COREDNS_ZONES_ROOT": filepath.Join(root, "zones"),
	}
	if err := os.MkdirAll(e.candidates, 0o755); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e dockerEnv) seedable(t *testing.T) string {
	t.Helper()
	p := filepath.Join(e.candidates, "rev-1", "coredns", "Corefile")
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte("a.test:53 {\n    file /etc/coredns/zones/a.test.zone\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func startDocker(t *testing.T, e dockerEnv, d *recordingDocker, dialed *string) (stdout, stderr *safeBuffer, stop func() error) {
	t.Helper()
	stdout, stderr = &safeBuffer{}, &safeBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	dial := func(path string) (controldocker.Docker, error) {
		if dialed != nil {
			*dialed = path
		}
		return d, nil
	}
	go func() { done <- runWith(ctx, lookup(e.env), stdout, stderr, dial) }()
	waitFor(t, "startup line", func() bool { return strings.Contains(stdout.String(), "listening on") })
	return stdout, stderr, func() error { cancel(); return <-done }
}

func TestDockerBackendServesOperationsOverTheSocket(t *testing.T) {
	e := newDockerEnv(t)
	d := &recordingDocker{running: true}
	var dialed string
	_, _, stop := startDocker(t, e, d, &dialed)
	dir := filepath.Join(e.candidates, "rev-1")
	os.MkdirAll(dir, 0o755)
	client := control.NewClient(e.socket)
	base := control.Request{RequestID: "r1", Service: control.ServiceCoreDNS, RevisionID: "rev-1", Checksum: "sum", CandidatePath: dir}
	for op, want := range map[control.Operation]string{
		control.OperationStage:  `{"status":"staged","candidatePath":"` + dir + `"}`,
		control.OperationHealth: `{"status":"healthy"}`,
	} {
		r := base
		r.Operation = op
		got, err := client.Execute(context.Background(), r)
		if err != nil || !got.OK || string(got.Output) != want {
			t.Fatalf("%s = %+v, %v", op, got, err)
		}
	}
	r := base
	r.Operation, r.CandidatePath = control.OperationStage, "/etc/passwd"
	got, err := client.Execute(context.Background(), r)
	if err != nil || got.OK || got.Error != "Candidate path is outside the fixed worker root" {
		t.Fatalf("outside root = %+v, %v", got, err)
	}
	if dialed != "/var/run/docker.sock" {
		t.Fatalf("dialed %q, want the default socket", dialed)
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}
}

func TestDockerSocketPathComesFromTheEnvironment(t *testing.T) {
	e := newDockerEnv(t)
	e.env["DOCKER_SOCKET_PATH"] = "/custom/docker.sock"
	var dialed string
	_, _, stop := startDocker(t, e, &recordingDocker{}, &dialed)
	stop()
	if dialed != "/custom/docker.sock" {
		t.Fatalf("dialed %q", dialed)
	}
}

func TestDockerStartupSeedsAndRestartsCoreDNSBeforeListening(t *testing.T) {
	e := newDockerEnv(t)
	e.env["COREDNS_CONTAINER_NAME"] = "dns-x"
	seeded := e.seedable(t)
	d := &recordingDocker{}
	stdout, stderr, stop := startDocker(t, e, d, nil)
	defer stop()
	want := "proxycore-control restored persisted CoreDNS Corefile from " + seeded + "\n" +
		"proxycore-control restarted dns-x to load Corefile\n" +
		"proxycore-control listening on " + e.socket + "\n"
	if stdout.String() != want || stderr.String() != "" {
		t.Fatalf("stdout %q stderr %q", stdout.String(), stderr.String())
	}
	if len(d.restarts) != 1 || d.restarts[0] != "dns-x:5" {
		t.Fatalf("restarts = %v", d.restarts)
	}
}

func TestDockerStartupRestartFailureIsReportedAndServingContinues(t *testing.T) {
	e := newDockerEnv(t)
	e.seedable(t)
	d := &recordingDocker{restartErr: errors.New("daemon down")}
	stdout, stderr, stop := startDocker(t, e, d, nil)
	defer stop()
	if stderr.String() != "proxycore-control could not restart CoreDNS after Corefile seed: daemon down\n" {
		t.Fatalf("stderr %q", stderr.String())
	}
	if strings.Contains(stdout.String(), "restarted") || !strings.Contains(stdout.String(), "listening on") {
		t.Fatalf("stdout %q", stdout.String())
	}
}

func TestDockerStartupWithNothingToSeedDoesNotRestart(t *testing.T) {
	e := newDockerEnv(t)
	d := &recordingDocker{}
	stdout, _, stop := startDocker(t, e, d, nil)
	defer stop()
	if stdout.String() != "proxycore-control listening on "+e.socket+"\n" || len(d.restarts) != 0 {
		t.Fatalf("stdout %q restarts %v", stdout.String(), d.restarts)
	}
}

func TestDockerBackendFailuresPreventListening(t *testing.T) {
	e := newDockerEnv(t)
	os.WriteFile(e.config, []byte("a file, not a directory"), 0o600) // seed cannot mkdir the config root
	var stdout, stderr safeBuffer
	err := runWith(context.Background(), lookup(e.env), &stdout, &stderr, func(string) (controldocker.Docker, error) {
		return &recordingDocker{}, nil
	})
	if err == nil {
		t.Fatal("seed failure must fail startup")
	}
	if _, statErr := os.Stat(e.socket); statErr == nil || stdout.String() != "" {
		t.Fatalf("socket created or output written: %q", stdout.String())
	}

	err = runWith(context.Background(), lookup(e.env), &stdout, &stderr, func(string) (controldocker.Docker, error) {
		return nil, errors.New("dial failed")
	})
	if err == nil || err.Error() != "dial failed" {
		t.Fatalf("dial error = %v", err)
	}
}

func TestDockerOptionsKeepEmptyValuesLikeNullishCoalescing(t *testing.T) {
	got := dockerOptions(lookup(map[string]string{"CANDIDATE_ROOT": "", "NGINX_CONTAINER_NAME": "web"}))
	defaults := controldocker.DefaultOptions()
	if got.CandidateRoot != "" || got.Containers.Nginx != "web" ||
		got.ZonesRoot != defaults.ZonesRoot || got.ConfigRoot != defaults.ConfigRoot || got.Containers.CoreDNS != defaults.Containers.CoreDNS {
		t.Fatalf("options = %+v", got)
	}
}

func TestNonDockerBackendNeverDials(t *testing.T) {
	backend, err := selectBackend(context.Background(), lookup(map[string]string{"CONTROL_BACKEND": "other"}), nil, nil,
		func(string) (controldocker.Docker, error) { t.Fatal("dialed"); return nil, nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := backend.(fixedBackend); !ok {
		t.Fatalf("backend = %T", backend)
	}
}
