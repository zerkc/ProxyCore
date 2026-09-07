package updater

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
			"run":    {Output: "migrations applied"},
			"up":     {Output: "api up"},
			"health": {Output: ""}, // not used via docker; HTTPGet handles it
		},
	}
	hits := 0
	u := New(Options{
		ProjectName:          "proxycore",
		Services:             []string{"api", "worker"},
		MigrateService:       "migrate",
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
		t.Fatalf("expected 5 steps (pull, migrate, restart, verify, bootstrap), got %d: %+v",
			len(status.Steps), stepNames(status.Steps))
	}
	want := []string{"pull", "migrate", "restart", "verify", "bootstrap"}
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
	if fc.CallCount() != 3 {
		t.Errorf("expected 3 docker calls (pull, run, up), got %d", fc.CallCount())
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

func TestApplyBuildModeFetchesSource(t *testing.T) {
	tmp := t.TempDir()
	requestFile := filepath.Join(tmp, "request.json")

	gitCalls := 0
	fc := &fakeCommander{
		result: map[string]fakeResult{
			"build": {Output: "built api worker migrate"},
			"run":   {Output: "migrations applied"},
			"up":    {Output: "api worker up"},
		},
	}
	origExec := fc.exec
	fc.ExecOverride = func(ctx context.Context, name string, args ...string) (string, error) {
		// gitArgs prepends [-C, dir] to subcommand args: args=[-C, dir, subcmd, ...]
		if name == "git" && len(args) >= 3 && args[0] == "-C" {
			gitCalls++
			subcmd := args[2]
			switch subcmd {
			case "rev-parse":
				return "abc123def456", nil
			case "status":
				return "", nil // clean
			case "fetch":
				return "", nil
			case "checkout":
				joined := strings.Join(args, " ")
				if !strings.Contains(joined, "--detach") || !strings.Contains(joined, "--force") || !strings.Contains(joined, "refs/tags/v0.1.6") {
					return "", fmt.Errorf("expected forced detached checkout of v0.1.6, got %s", joined)
				}
				return "", nil
			default:
				return "", fmt.Errorf("unexpected git subcommand: %s", subcmd)
			}
		}
		return origExec(ctx, name, args...)
	}

	u := New(Options{
		UpdateMode:           UpdateModeBuild,
		SourceDir:            tmp,
		ProjectName:          "proxycore",
		Services:             []string{"api", "worker"},
		MigrateService:       "migrate",
		BootstrapRequestFile: requestFile,
		HealthURL:            "http://api:3000/api/health",
		HealthTimeout:        2 * time.Second,
		PollInterval:         50 * time.Millisecond,
		Exec:                 fc.Exec,
		HTTPGet:              func(ctx context.Context, url string) (int, error) { return 200, nil },
		Logger:               &recordingLogger{},
	})

	status, err := u.Apply(context.Background(), "0.1.6")
	if err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}
	if status.Status != "ok" {
		t.Fatalf("status=%q want ok; steps=%+v", status.Status, status.Steps)
	}
	// fetch-source is now the first step.
	if len(status.Steps) != 6 {
		t.Fatalf("expected 6 steps (fetch-source, build, migrate, restart, verify, bootstrap), got %d: %+v",
			len(status.Steps), stepNames(status.Steps))
	}
	if status.Steps[0].Name != "fetch-source" {
		t.Errorf("first step=%q want fetch-source", status.Steps[0].Name)
	}
	if status.Steps[0].Status != "ok" {
		t.Errorf("fetch-source status=%q want ok; err=%q", status.Steps[0].Status, status.Steps[0].Error)
	}
	if gitCalls < 4 {
		t.Errorf("expected ≥4 git calls (rev-parse, status, fetch, checkout), got %d", gitCalls)
	}
}

func TestApplyBuildModeSkipsFetchSourceWhenSourceDirEmpty(t *testing.T) {
	tmp := t.TempDir()
	requestFile := filepath.Join(tmp, "request.json")

	gitCalls := 0
	fc := &fakeCommander{
		result: map[string]fakeResult{
			"build": {Output: "built"},
			"run":   {Output: "migrated"},
			"up":    {Output: "up"},
		},
	}
	origExec := fc.exec
	fc.ExecOverride = func(ctx context.Context, name string, args ...string) (string, error) {
		if name == "git" && len(args) >= 3 && args[0] == "-C" {
			gitCalls++
		}
		return origExec(ctx, name, args...)
	}

	u := New(Options{
		UpdateMode:           UpdateModeBuild,
		SourceDir:            "", // not configured
		ProjectName:          "proxycore",
		Services:             []string{"api"},
		MigrateService:       "migrate",
		BootstrapRequestFile: requestFile,
		HealthURL:            "http://api:3000/api/health",
		HealthTimeout:        2 * time.Second,
		PollInterval:         50 * time.Millisecond,
		Exec:                 fc.Exec,
		HTTPGet:              func(ctx context.Context, url string) (int, error) { return 200, nil },
		Logger:               &recordingLogger{},
	})

	status, err := u.Apply(context.Background(), "0.1.6")
	if err != nil {
		t.Fatalf("Apply returned error: %v", err)
	}
	if status.Status != "ok" {
		t.Fatalf("status=%q want ok", status.Status)
	}
	// No fetch-source step when SourceDir is empty.
	if len(status.Steps) != 5 {
		t.Fatalf("expected 5 steps (build, migrate, restart, verify, bootstrap), got %d: %+v",
			len(status.Steps), stepNames(status.Steps))
	}
	if status.Steps[0].Name != UpdateModeBuild {
		t.Errorf("first step=%q want %s", status.Steps[0].Name, UpdateModeBuild)
	}
	if gitCalls != 0 {
		t.Errorf("expected 0 git calls when SourceDir is empty, got %d", gitCalls)
	}
}

func TestApplyBuildModeRejectsDirtySource(t *testing.T) {
	tmp := t.TempDir()
	requestFile := filepath.Join(tmp, "request.json")

	fc := &fakeCommander{
		result: map[string]fakeResult{
			"build": {Output: "built"},
			"run":   {Output: "migrated"},
			"up":    {Output: "up"},
		},
	}
	origExec := fc.exec
	fc.ExecOverride = func(ctx context.Context, name string, args ...string) (string, error) {
		// args = [-C, dir, subcmd, ...]
		if name == "git" && len(args) >= 3 && args[0] == "-C" {
			subcmd := args[2]
			switch subcmd {
			case "rev-parse":
				return "abc123", nil
			case "status":
				return "M  some/file.go\n", nil // dirty
			default:
				return "", nil
			}
		}
		return origExec(ctx, name, args...)
	}

	u := New(Options{
		UpdateMode:           UpdateModeBuild,
		SourceDir:            tmp,
		ProjectName:          "proxycore",
		Services:             []string{"api"},
		MigrateService:       "migrate",
		BootstrapRequestFile: requestFile,
		HealthURL:            "http://api:3000/api/health",
		HealthTimeout:        2 * time.Second,
		PollInterval:         50 * time.Millisecond,
		Exec:                 fc.Exec,
		HTTPGet:              func(ctx context.Context, url string) (int, error) { return 200, nil },
		Logger:               &recordingLogger{},
	})

	_, err := u.Apply(context.Background(), "0.1.6")
	if err == nil {
		t.Fatal("expected error for dirty source")
	}
	last := u.Last()
	if last == nil || last.Status != "failed" {
		t.Fatalf("Last().Status=%v want failed", last)
	}
	if len(last.Steps) < 1 || last.Steps[0].Name != "fetch-source" {
		t.Errorf("expected first step=fetch-source, got %+v", stepNames(last.Steps))
	}
	if !strings.Contains(last.Steps[0].Error, ErrSourceDirty.Error()) {
		t.Errorf("expected error to mention ErrSourceDirty; got %q", last.Steps[0].Error)
	}
}

func TestApplyBuildModeRejectsGitFailure(t *testing.T) {
	tmp := t.TempDir()
	requestFile := filepath.Join(tmp, "request.json")

	fc := &fakeCommander{
		result: map[string]fakeResult{
			"build": {Output: "built"},
			"run":   {Output: "migrated"},
			"up":    {Output: "up"},
		},
	}
	origExec := fc.exec
	fc.ExecOverride = func(ctx context.Context, name string, args ...string) (string, error) {
		if name == "git" && len(args) >= 3 && args[0] == "-C" {
			subcmd := args[2]
			switch subcmd {
			case "rev-parse":
				return "abc123", nil
			case "status":
				return "", nil
			case "fetch":
				return "", fmt.Errorf("exit 1: repository not found")
			default:
				return "", nil
			}
		}
		return origExec(ctx, name, args...)
	}

	u := New(Options{
		UpdateMode:           UpdateModeBuild,
		SourceDir:            tmp,
		ProjectName:          "proxycore",
		Services:             []string{"api"},
		MigrateService:       "migrate",
		BootstrapRequestFile: requestFile,
		HealthURL:            "http://api:3000/api/health",
		HealthTimeout:        2 * time.Second,
		PollInterval:         50 * time.Millisecond,
		Exec:                 fc.Exec,
		HTTPGet:              func(ctx context.Context, url string) (int, error) { return 200, nil },
		Logger:               &recordingLogger{},
	})

	_, err := u.Apply(context.Background(), "0.1.6")
	if err == nil {
		t.Fatal("expected error after git fetch failure")
	}
	last := u.Last()
	if last == nil || last.Status != "failed" {
		t.Fatalf("Last().Status=%v want failed", last)
	}
	if !strings.Contains(last.Steps[0].Error, "git fetch") {
		t.Errorf("expected error to mention git fetch; got %q", last.Steps[0].Error)
	}
}

func TestApplyBuildModeRestoresSourceOnFailure(t *testing.T) {
	tmp := t.TempDir()
	requestFile := filepath.Join(tmp, "request.json")

	originalHead := "abc000original"
	restoredTo := ""

	fc := &fakeCommander{
		result: map[string]fakeResult{
			"build": {Output: "built"},
			"run":   {Output: "migrations applied"},
			"up":    {Output: "no such image", Err: errFake("exit 1")},
		},
	}
	origExec := fc.exec
	fc.ExecOverride = func(ctx context.Context, name string, args ...string) (string, error) {
		// args = [-C, dir, subcmd, ...]
		if name == "git" && len(args) >= 3 && args[0] == "-C" {
			subcmd := args[2]
			switch subcmd {
			case "rev-parse":
				return originalHead, nil
			case "status":
				return "", nil
			case "fetch":
				return "", nil
			case "checkout":
				checkoutTarget := args[len(args)-1]
				if checkoutTarget == originalHead {
					// Restore call after restart failure.
					restoredTo = originalHead
					return "", nil
				}
				// Checkout the target tag.
				return "checked out refs/tags/v0.1.6", nil
			default:
				return "", nil
			}
		}
		return origExec(ctx, name, args...)
	}

	u := New(Options{
		UpdateMode:           UpdateModeBuild,
		SourceDir:            tmp,
		ProjectName:          "proxycore",
		Services:             []string{"api"},
		MigrateService:       "migrate",
		BootstrapRequestFile: requestFile,
		HealthURL:            "http://api:3000/api/health",
		HealthTimeout:        2 * time.Second,
		PollInterval:         50 * time.Millisecond,
		Exec:                 fc.Exec,
		HTTPGet:              func(ctx context.Context, url string) (int, error) { return 200, nil },
		Logger:               &recordingLogger{},
	})

	_, err := u.Apply(context.Background(), "0.1.6")
	if err == nil {
		t.Fatal("expected error after restart failure")
	}
	if restoredTo != originalHead {
		t.Errorf("expected restore to checkout %q, got %q", originalHead, restoredTo)
	}
}

func TestApplyBuildMode(t *testing.T) {
	tmp := t.TempDir()
	requestFile := filepath.Join(tmp, "request.json")
	fc := &fakeCommander{
		result: map[string]fakeResult{
			"build": {Output: "built api worker migrate"},
			"run":   {Output: "migrations applied"},
			"up":    {Output: "api worker up"},
		},
	}
	u := New(Options{
		UpdateMode:           UpdateModeBuild,
		ProjectName:          "proxycore",
		Services:             []string{"api", "worker"},
		MigrateService:       "migrate",
		BootstrapRequestFile: requestFile,
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
	if status.Steps[0].Name != UpdateModeBuild {
		t.Fatalf("first step=%q want %q", status.Steps[0].Name, UpdateModeBuild)
	}
	fc.mu.Lock()
	firstArgs := append([]string(nil), fc.calls[0].Args...)
	fc.mu.Unlock()
	joined := strings.Join(firstArgs, " ")
	for _, want := range []string{"--profile", "tools", "build", "api", "worker", "migrate"} {
		if !strings.Contains(joined, want) {
			t.Errorf("build command %q does not contain %q", joined, want)
		}
	}
}

func TestApplyPullFailureStopsEarly(t *testing.T) {
	fc := &fakeCommander{
		result: map[string]fakeResult{
			"pull": {Output: "connection refused", Err: errFake("exit 1")},
		},
	}
	u := New(Options{
		ProjectName:    "proxycore",
		Services:       []string{"api", "worker"},
		MigrateService: "migrate",
		HealthURL:      "http://api:3000/api/health",
		HealthTimeout:  2 * time.Second,
		PollInterval:   50 * time.Millisecond,
		Exec:           fc.Exec,
		HTTPGet:        func(ctx context.Context, url string) (int, error) { return 200, nil },
		Logger:         &recordingLogger{},
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
		ProjectName:    "proxycore",
		Services:       []string{"api", "worker"},
		MigrateService: "migrate",
		HealthURL:      "http://api:3000/api/health",
		HealthTimeout:  2 * time.Second,
		PollInterval:   50 * time.Millisecond,
		Exec:           fc.Exec,
		HTTPGet:        func(ctx context.Context, url string) (int, error) { return 200, nil },
		Logger:         &recordingLogger{},
	})

	_, err := u.Apply(context.Background(), "0.1.5")
	if err == nil {
		t.Fatal("expected error after restart failure")
	}
	last := u.Last()
	if len(last.Steps) != 3 {
		t.Fatalf("expected 3 steps (pull, migrate, restart), got %d: %+v",
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
		MigrateService:       "migrate",
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
		ProjectName:    "proxycore",
		Services:       []string{"api"},
		MigrateService: "migrate",
		HealthURL:      "http://api:3000/api/health",
		HealthTimeout:  2 * time.Second,
		PollInterval:   50 * time.Millisecond,
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

func stepNames(steps []StepResult) []string {
	out := make([]string, len(steps))
	for i, s := range steps {
		out[i] = s.Name + ":" + s.Status
	}
	return out
}

type errFake string

func (e errFake) Error() string { return string(e) }
