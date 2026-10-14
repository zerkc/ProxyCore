package updater

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type recordingLogger struct {
	mu    sync.Mutex
	lines []string
}

func (l *recordingLogger) Printf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, format)
}

// fakeCommander records every invocation and lets the test program responses.
type fakeCommander struct {
	mu           sync.Mutex
	calls        []call
	result       map[string]fakeResult
	ExecOverride func(ctx context.Context, name string, args ...string) (string, error)
}

type call struct {
	Name string
	Args []string
}

type fakeResult struct {
	Output string
	Err    error
}

// exec is the default implementation; handles docker compose calls using the
// programmed result map.
func (f *fakeCommander) exec(ctx context.Context, name string, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call{Name: name, Args: append([]string(nil), args...)})
	// The docker compose subcommand (pull, run, up, ...) is buried somewhere
	// after the -f / -p flags we pass. Scan for the first occurrence of any
	// known subcommand — that's the key tests program results by.
	subcommands := []string{"pull", "build", "run", "up", "down", "ps", "logs"}
	key := ""
	for _, a := range args {
		for _, sc := range subcommands {
			if a == sc {
				key = sc
				break
			}
		}
		if key != "" {
			break
		}
	}
	r, ok := f.result[key]
	if !ok {
		// default to success so unprogrammed commands don't fail the test
		return "", nil
	}
	return r.Output, r.Err
}

// Exec implements the updater.Options.Exec contract. When ExecOverride is set it
// is called; otherwise it falls through to the default exec handler.
func (f *fakeCommander) Exec(ctx context.Context, name string, args ...string) (string, error) {
	if f.ExecOverride != nil {
		return f.ExecOverride(ctx, name, args...)
	}
	return f.exec(ctx, name, args...)
}

func (f *fakeCommander) CallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func TestApplyHappyPath(t *testing.T) {
	tmp := t.TempDir()
	requestFile := filepath.Join(tmp, "request.json")

	fc := &fakeCommander{
		result: map[string]fakeResult{
			"pull":   {Output: "pulled api: ok"},
			"run":    {Output: "preflight ok"},
			"up":     {Output: "api up"},
			"health": {Output: ""}, // not used via docker; HTTPGet handles it
		},
	}
	hits := 0
	u := New(Options{
		ProjectName:          "proxycore",
		Services:             []string{"api", "worker"},
		HealthURL:            "http://api:3000/api/health",
		HealthTimeout:        2 * time.Second,
		PollInterval:         50 * time.Millisecond,
		BootstrapRequestFile: requestFile,
		Exec:                 fc.Exec,
		HTTPGet: func(ctx context.Context, url string) (int, error) {
			hits++
			return 200, nil
		},
		Logger: &recordingLogger{},
	})

	status, err := u.Apply(context.Background(), "0.1.5")
	if err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}
	if status.Status != "ok" {
		t.Fatalf("status=%q want ok; steps=%+v", status.Status, status.Steps)
	}
	if len(status.Steps) != 5 {
		t.Fatalf("expected 5 steps (pull, preflight, restart, verify, bootstrap), got %d: %+v",
			len(status.Steps), stepNames(status.Steps))
	}
	want := []string{"pull", "preflight", "restart", "verify", "bootstrap"}
	for i, n := range want {
		if status.Steps[i].Name != n {
			t.Errorf("step %d name=%q want %q", i, status.Steps[i].Name, n)
		}
		if status.Steps[i].Status != "ok" {
			t.Errorf("step %d status=%q want ok; err=%q", i, status.Steps[i].Status, status.Steps[i].Error)
		}
	}
	if hits != 1 {
		t.Errorf("expected 1 health probe, got %d", hits)
	}
	if fc.CallCount() != 4 {
		t.Errorf("expected 4 docker calls (pull, volume inspect, run, up), got %d", fc.CallCount())
	}

	// Verify the atomic bootstrap request JSON was written.
	data, err := os.ReadFile(requestFile)
	if err != nil {
		t.Fatalf("bootstrap request file not written: %v", err)
	}
	var req bootstrapRequest
	if err := json.Unmarshal(data, &req); err != nil {
		t.Fatalf("bootstrap request is not valid JSON: %v; body=%q", err, data)
	}
	if req.TargetVersion != "0.1.5" {
		t.Errorf("bootstrap request targetVersion=%q want %q", req.TargetVersion, "0.1.5")
	}
	if req.RequestedAt == "" {
		t.Error("bootstrap request requestedAt is empty")
	}
}

func TestApplyDefaultsIncludeControlAndNginxRecoveryOwners(t *testing.T) {
	fc := &fakeCommander{}
	u := New(Options{
		Exec: fc.Exec,
		HTTPGet: func(ctx context.Context, url string) (int, error) {
			return 200, nil
		},
		Logger: &recordingLogger{},
	})

	status, err := u.Apply(context.Background(), "0.1.5")
	if err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}
	if status.Status != "ok" {
		t.Fatalf("status=%q want ok", status.Status)
	}

	fc.mu.Lock()
	calls := append([]call(nil), fc.calls...)
	fc.mu.Unlock()
	for _, subcommand := range []string{"pull", "up"} {
		var args []string
		for _, c := range calls {
			if c.Name == "docker" && containsArg(c.Args, subcommand) {
				args = c.Args
				break
			}
		}
		if args == nil {
			t.Fatalf("did not record docker compose %s", subcommand)
		}
		joined := strings.Join(args, " ")
		for _, service := range []string{"api", "worker", "control", "nginx"} {
			if !strings.Contains(joined, service) {
				t.Errorf("compose %s args %q do not include service %q", subcommand, joined, service)
			}
		}
	}
}

// bootstrapRequest mirrors the JSON written by the updater after a successful
// apply. Defined here so the test can parse the request file without
// importing the internal package layout.
type bootstrapRequest struct {
	TargetVersion string `json:"targetVersion"`
	RequestedAt   string `json:"requestedAt"`
}

func TestApplyRejectsInvalidTargetVersion(t *testing.T) {
	fc := &fakeCommander{}
	u := New(Options{Exec: fc.Exec})

	_, err := u.Apply(context.Background(), "latest")
	if !errors.Is(err, ErrInvalidTargetVersion) {
		t.Fatalf("err=%v, want ErrInvalidTargetVersion", err)
	}
	if fc.CallCount() != 0 {
		t.Fatalf("expected no commands for invalid target, got %d", fc.CallCount())
	}
}

func TestApplyPullFailureStopsEarly(t *testing.T) {
	fc := &fakeCommander{
		result: map[string]fakeResult{
			"pull": {Output: "connection refused", Err: errFake("exit 1")},
		},
	}
	u := New(Options{
		ProjectName:   "proxycore",
		Services:      []string{"api", "worker"},
		HealthURL:     "http://api:3000/api/health",
		HealthTimeout: 2 * time.Second,
		PollInterval:  50 * time.Millisecond,
		Exec:          fc.Exec,
		HTTPGet:       func(ctx context.Context, url string) (int, error) { return 200, nil },
		Logger:        &recordingLogger{},
	})

	_, err := u.Apply(context.Background(), "0.1.5")
	if err == nil {
		t.Fatal("expected error after pull failure")
	}
	if !strings.Contains(err.Error(), "step failed") {
		t.Errorf("err=%q; want it to mention step failure", err.Error())
	}
	last := u.Last()
	if last == nil || last.Status != "failed" {
		t.Fatalf("Last().Status=%v want failed", last)
	}
	if len(last.Steps) != 1 || last.Steps[0].Name != "pull" {
		t.Fatalf("expected only pull step, got %+v", stepNames(last.Steps))
	}
}

func TestApplyRestartFailureKeepsVerifyPending(t *testing.T) {
	fc := &fakeCommander{
		result: map[string]fakeResult{
			"pull": {Output: ""},
			"run":  {Output: ""},
			"up":   {Output: "no such image", Err: errFake("exit 1")},
		},
	}
	u := New(Options{
		ProjectName:   "proxycore",
		Services:      []string{"api", "worker"},
		HealthURL:     "http://api:3000/api/health",
		HealthTimeout: 2 * time.Second,
		PollInterval:  50 * time.Millisecond,
		Exec:          fc.Exec,
		HTTPGet:       func(ctx context.Context, url string) (int, error) { return 200, nil },
		Logger:        &recordingLogger{},
	})

	_, err := u.Apply(context.Background(), "0.1.5")
	if err == nil {
		t.Fatal("expected error after restart failure")
	}
	last := u.Last()
	if len(last.Steps) != 3 {
		t.Fatalf("expected 3 steps (pull, preflight, restart), got %d: %+v",
			len(last.Steps), stepNames(last.Steps))
	}
	if last.Steps[2].Name != "restart" || last.Steps[2].Status != "failed" {
		t.Errorf("expected step 3 = restart/failed, got %+v", last.Steps[2])
	}
}

func TestApplyBootstrapWriteFailure(t *testing.T) {
	// Use a path that does not exist so the atomic write fails.
	requestFile := "/nonexistent/path/request.json"

	fc := &fakeCommander{
		result: map[string]fakeResult{
			"pull":   {Output: ""},
			"run":    {Output: ""},
			"up":     {Output: "api up"},
			"health": {Output: ""},
		},
	}
	hits := 0
	u := New(Options{
		ProjectName:          "proxycore",
		Services:             []string{"api"},
		HealthURL:            "http://api:3000/api/health",
		HealthTimeout:        2 * time.Second,
		PollInterval:         50 * time.Millisecond,
		BootstrapRequestFile: requestFile,
		Exec:                 fc.Exec,
		HTTPGet: func(ctx context.Context, url string) (int, error) {
			hits++
			return 200, nil
		},
		Logger: &recordingLogger{},
	})

	_, err := u.Apply(context.Background(), "0.1.5")
	if err == nil {
		t.Fatal("expected error after bootstrap write failure")
	}
	if err != ErrBootstrapRequestFailed {
		t.Errorf("err=%v want ErrBootstrapRequestFailed", err)
	}
	last := u.Last()
	if last == nil || last.Status != "failed" {
		t.Fatalf("Last().Status=%v want failed", last)
	}
	// Should have all 5 steps: the first 4 succeeded, bootstrap failed.
	if len(last.Steps) != 5 {
		t.Fatalf("expected 5 steps, got %d: %+v", len(last.Steps), stepNames(last.Steps))
	}
	if last.Steps[4].Name != "bootstrap" || last.Steps[4].Status != "failed" {
		t.Errorf("expected step 5 = bootstrap/failed, got %+v", last.Steps[4])
	}
	if hits != 1 {
		t.Errorf("expected 1 health probe, got %d", hits)
	}
}

func TestApplyBootstrapSkippedWhenNotConfigured(t *testing.T) {
	fc := &fakeCommander{
		result: map[string]fakeResult{
			"pull":   {Output: ""},
			"run":    {Output: ""},
			"up":     {Output: "api up"},
			"health": {Output: ""},
		},
	}
	u := New(Options{
		ProjectName:   "proxycore",
		Services:      []string{"api"},
		HealthURL:     "http://api:3000/api/health",
		HealthTimeout: 2 * time.Second,
		PollInterval:  50 * time.Millisecond,
		// BootstrapRequestFile left empty — step should be skipped.
		Exec: fc.Exec,
		HTTPGet: func(ctx context.Context, url string) (int, error) {
			return 200, nil
		},
		Logger: &recordingLogger{},
	})

	status, err := u.Apply(context.Background(), "0.1.5")
	if err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}
	if status.Status != "ok" {
		t.Fatalf("status=%q want ok", status.Status)
	}
	if len(status.Steps) != 5 {
		t.Fatalf("expected 5 steps (bootstrap step is present but skips), got %d: %+v",
			len(status.Steps), stepNames(status.Steps))
	}
	boot := status.Steps[4]
	if boot.Name != "bootstrap" {
		t.Errorf("step 5 name=%q want bootstrap", boot.Name)
	}
	if boot.Status != "ok" {
		t.Errorf("bootstrap step status=%q want ok when not configured; err=%q",
			boot.Status, boot.Error)
	}
	if !strings.Contains(boot.Output, "not configured") {
		t.Errorf("bootstrap output=%q; want it to mention 'not configured'", boot.Output)
	}
}

func TestApplyConcurrentRunRejected(t *testing.T) {
	fc := &fakeCommander{}
	// Block the first Apply in the verify (HTTPGet) step so the test main
	// goroutine has time to observe inFlight=true and start a second Apply.
	block := make(chan struct{})
	release := make(chan struct{})
	u := New(Options{
		ProjectName:   "proxycore",
		Services:      []string{"api"},
		HealthURL:     "http://api:3000/api/health",
		HealthTimeout: 2 * time.Second,
		PollInterval:  20 * time.Millisecond,
		Exec:          fc.Exec,
		HTTPGet: func(ctx context.Context, url string) (int, error) {
			select {
			case <-release:
				return 200, nil
			case <-block:
				return 200, nil
			}
		},
		Logger: &recordingLogger{},
	})

	firstCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		_, _ = u.Apply(firstCtx, "0.1.5")
		close(done)
	}()

	// Wait until the first Apply marks inFlight (which it does before the
	// first HTTPGet call) and is parked in the verify step.
	deadline := time.Now().Add(time.Second)
	for !u.InFlight() {
		if time.Now().After(deadline) {
			close(release)
			cancel()
			t.Fatal("first Apply never entered inFlight state")
		}
		time.Sleep(2 * time.Millisecond)
	}

	_, err := u.Apply(context.Background(), "0.1.6")
	if err != ErrAlreadyRunning {
		t.Errorf("second Apply err=%v want ErrAlreadyRunning", err)
	}

	close(release)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("first Apply did not return after release")
	}
}

func containsArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

func stepNames(steps []StepResult) []string {
	out := make([]string, len(steps))
	for i, s := range steps {
		out[i] = s.Name + ":" + s.Status
	}
	return out
}

type errFake string

func (e errFake) Error() string { return string(e) }

// preflightFixture builds an Updater whose Exec reports a programmed legacy
// volume / sqlite database state and records every command.
func preflightFixture(t *testing.T, legacyVolume, sqliteDB bool) (*Updater, *[]string) {
	t.Helper()
	var cmds []string
	exec := func(ctx context.Context, name string, args ...string) (string, error) {
		line := name + " " + strings.Join(args, " ")
		cmds = append(cmds, line)
		switch {
		case name == "docker" && len(args) >= 3 && args[0] == "volume" && args[1] == "inspect":
			if args[2] != "proxycore_postgres-data" {
				t.Errorf("unexpected volume inspected: %q", args[2])
			}
			if legacyVolume {
				return "[{}]", nil
			}
			return "", errFake("no such volume")
		case containsArg(args, "run") && containsArg(args, "--entrypoint"):
			if sqliteDB {
				return "", nil
			}
			return "", errFake("exit 1")
		}
		return "", nil
	}
	u := New(Options{
		ProjectName:   "proxycore",
		Services:      []string{"api", "worker"},
		HealthTimeout: time.Second,
		PollInterval:  10 * time.Millisecond,
		Exec:          exec,
		HTTPGet:       func(ctx context.Context, url string) (int, error) { return 200, nil },
		Logger:        &recordingLogger{},
	})
	return u, &cmds
}

func TestPreflightRefusesLegacyPostgresWithoutSQLite(t *testing.T) {
	u, cmds := preflightFixture(t, true, false)

	_, err := u.Apply(context.Background(), "0.2.0")
	if err == nil {
		t.Fatal("expected preflight failure")
	}
	last := u.Last()
	if got := stepNames(last.Steps); len(got) != 2 || got[1] != "preflight:failed" {
		t.Fatalf("steps=%v want [pull:ok preflight:failed]", got)
	}
	msg := last.Steps[1].Error
	for _, want := range []string{"PostgreSQL", "proxycore-convert", "docs"} {
		if !strings.Contains(msg, want) {
			t.Errorf("preflight error %q does not mention %q", msg, want)
		}
	}
	for _, c := range *cmds {
		if strings.Contains(c, " up ") {
			t.Errorf("services must not be restarted after a refused preflight: %q", c)
		}
	}
}

func TestPreflightProceedsOnFreshInstall(t *testing.T) {
	u, cmds := preflightFixture(t, false, false)

	status, err := u.Apply(context.Background(), "0.2.0")
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := stepNames(status.Steps)[1]; got != "preflight:ok" {
		t.Errorf("step 1=%q want preflight:ok", got)
	}
	for _, c := range *cmds {
		if strings.Contains(c, "--entrypoint") {
			t.Errorf("no sqlite probe expected without a legacy volume: %q", c)
		}
	}
}

func TestPreflightProceedsWhenSQLiteAlreadyExists(t *testing.T) {
	u, cmds := preflightFixture(t, true, true)

	status, err := u.Apply(context.Background(), "0.2.0")
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := stepNames(status.Steps)[1]; got != "preflight:ok" {
		t.Errorf("step 1=%q want preflight:ok", got)
	}
	probed := false
	for _, c := range *cmds {
		if strings.Contains(c, "--entrypoint test") && strings.Contains(c, "/var/lib/proxycore-db/proxycore.db") {
			probed = true
		}
	}
	if !probed {
		t.Errorf("expected sqlite probe via compose run; commands=%v", *cmds)
	}
}

// Updates always pull: a build-looking PROXYCORE_UPDATE_MODE in the process
// environment must not change the command sequence.
func TestApplyAlwaysPullsIgnoringBuildModeEnv(t *testing.T) {
	t.Setenv("PROXYCORE_UPDATE_MODE", "build")
	fc := &fakeCommander{}
	u := New(Options{
		ProjectName:          "proxycore",
		Services:             []string{"api", "worker"},
		BootstrapRequestFile: filepath.Join(t.TempDir(), "request.json"),
		HealthURL:            "http://api:3000/api/health",
		HealthTimeout:        2 * time.Second,
		PollInterval:         50 * time.Millisecond,
		Exec:                 fc.Exec,
		HTTPGet:              func(ctx context.Context, url string) (int, error) { return 200, nil },
		Logger:               &recordingLogger{},
	})
	status, err := u.Apply(context.Background(), "0.1.5")
	if err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}
	if status.Steps[0].Name != "pull" {
		t.Fatalf("first step=%q want pull", status.Steps[0].Name)
	}
	fc.mu.Lock()
	defer fc.mu.Unlock()
	for _, c := range fc.calls {
		if c.Name == "git" {
			t.Errorf("unexpected git call: %v", c.Args)
		}
		for _, a := range c.Args {
			if a == "build" {
				t.Errorf("unexpected build call: %v", c.Args)
			}
		}
	}
}
