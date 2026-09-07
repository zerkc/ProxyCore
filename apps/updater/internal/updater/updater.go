// Package updater orchestrates the self-update of a ProxyCore installation.
//
// The updater is intentionally not in charge of restarting itself — that is
// handled by an external bootstrap (see scripts/updater-bootstrap.sh) because
// a process cannot reliably replace its own running image. The updater only
// owns the lifecycle of the api and worker services. After a successful
// build/pull→migrate→restart→verify sequence it atomically writes a bootstrap
// request to a shared volume so the sidecar can build/pull the new image and
// recreate the updater container without a circular dependency.
package updater

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// UpdateModePull uses registry images already referenced by compose.yaml.
	UpdateModePull = "pull"
	// UpdateModeBuild rebuilds local compose services from the checked-out source.
	UpdateModeBuild = "build"
)

// Options configures an Updater. Defaults are applied in New.
type Options struct {
	// UpdateMode selects whether api/worker images are pulled or built locally.
	// Pull is the default to preserve registry-backed deployments.
	UpdateMode string
	// ComposeFile is the absolute path to compose.yaml inside the container.
	ComposeFile string
	// ProjectName is the docker compose project name (matches
	// COMPOSE_PROJECT_NAME in install.sh).
	ProjectName string
	// EnvFile, when non-empty, is passed as `--env-file` to every `docker
	// compose` invocation so ${VAR} interpolation works inside the container.
	EnvFile string
	// Services is the list of services to build/pull and recreate during an
	// update. The updater itself MUST NOT be in this list.
	Services []string
	// MigrateService is the name of the one-shot migration service defined
	// in compose.yaml under profiles: [tools].
	MigrateService string
	// HealthURL is polled after restart to confirm the new api is live.
	HealthURL string
	// HealthTimeout bounds how long Apply waits for HealthURL to return 2xx.
	HealthTimeout time.Duration
	// PollInterval is how often to re-check HealthURL while waiting.
	PollInterval time.Duration
	// ComposeExtraArgs are passed to every `docker compose` invocation, after
	// `-f <file> -p <project> [--env-file <file>]`.
	ComposeExtraArgs []string

	// BootstrapRequestFile, when non-empty, is the absolute path on the shared
	// volume where the updater writes an atomic bootstrap request JSON after a
	// successful update. The updater-bootstrap sidecar watches this file and
	// replaces the updater's own container with the newly-pulled image.
	// Optional; when empty the bootstrap step is skipped silently.
	BootstrapRequestFile string

	// Exec runs a command and returns combined output. Override for tests.
	Exec func(ctx context.Context, name string, args ...string) (string, error)
	// HTTPGet issues GET and returns the response status code.
	HTTPGet func(ctx context.Context, url string) (int, error)
	// Logger receives structured progress messages.
	Logger Logger
}

// Logger is the minimal logging surface the updater needs.
type Logger interface {
	Printf(format string, args ...any)
}

type nopLogger struct{}

func (nopLogger) Printf(string, ...any) {}

// Status is the public record of an Apply run.
type Status struct {
	Status        string       `json:"status"` // running | ok | failed
	TargetVersion string       `json:"targetVersion"`
	StartedAt     time.Time    `json:"startedAt"`
	FinishedAt    *time.Time   `json:"finishedAt,omitempty"`
	Steps         []StepResult `json:"steps"`
}

// StepResult records the outcome of one logical step of the update.
type StepResult struct {
	Name       string        `json:"name"`
	Status     string        `json:"status"` // ok | failed | skipped
	Error      string        `json:"error,omitempty"`
	Duration   time.Duration `json:"-"`
	DurationMs int64         `json:"durationMs"`
	Output     string        `json:"output,omitempty"` // truncated command output
	StartedAt  time.Time     `json:"startedAt"`
}

// Updater orchestrates build/pull → migrate → restart → verify → bootstrap.
type Updater struct {
	opts Options

	mu     sync.Mutex
	last   *Status
	inFlgt bool
}

// New returns an Updater with defaults applied.
func New(opts Options) *Updater {
	opts.UpdateMode = normalizeUpdateMode(opts.UpdateMode)
	if opts.ComposeFile == "" {
		opts.ComposeFile = "/app/compose.yaml"
	}
	if opts.ProjectName == "" {
		opts.ProjectName = "proxycore"
	}
	if len(opts.Services) == 0 {
		opts.Services = []string{"api", "worker"}
	}
	if opts.MigrateService == "" {
		opts.MigrateService = "migrate"
	}
	if opts.HealthURL == "" {
		opts.HealthURL = "http://api:3000/api/health"
	}
	if opts.HealthTimeout <= 0 {
		opts.HealthTimeout = 90 * time.Second
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = 2 * time.Second
	}
	if opts.Exec == nil {
		opts.Exec = defaultExec
	}
	if opts.HTTPGet == nil {
		opts.HTTPGet = defaultHTTPGet
	}
	if opts.Logger == nil {
		opts.Logger = nopLogger{}
	}
	return &Updater{opts: opts}
}

// Last returns the most recent Apply result. Safe to call concurrently.
func (u *Updater) Last() *Status {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.last == nil {
		return nil
	}
	clone := *u.last
	return &clone
}

// InFlight reports whether an Apply is currently running.
func (u *Updater) InFlight() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.inFlgt
}

// Apply runs the full update sequence for the given target version.
//
// The returned Status is also retained and exposed via Last(). The caller
// (typically an HTTP handler) should treat Apply as long-running and return
// quickly to the client with 202; the actual work continues here.
func (u *Updater) Apply(ctx context.Context, targetVersion string) (*Status, error) {
	u.mu.Lock()
	if u.inFlgt {
		u.mu.Unlock()
		return nil, ErrAlreadyRunning
	}
	u.inFlgt = true
	status := &Status{
		Status:        "running",
		TargetVersion: targetVersion,
		StartedAt:     time.Now().UTC(),
	}
	u.last = status
	u.mu.Unlock()

	defer func() {
		u.mu.Lock()
		u.inFlgt = false
		finished := time.Now().UTC()
		status.FinishedAt = &finished
		u.mu.Unlock()
	}()

	steps := []StepResult{}

	runStep := func(name string, fn func() (string, error)) {
		step := StepResult{Name: name, Status: "ok", StartedAt: time.Now().UTC()}
		out, err := fn()
		step.Duration = time.Since(step.StartedAt)
		step.DurationMs = step.Duration.Milliseconds()
		if err != nil {
			step.Status = "failed"
			step.Error = err.Error()
		}
		step.Output = truncate(out, 1024)
		steps = append(steps, step)
		u.opts.Logger.Printf("updater: %s status=%s duration=%s err=%q",
			name, step.Status, step.Duration, step.Error)
		if err != nil {
			// capture current state before bailing
			u.mu.Lock()
			status.Steps = append([]StepResult(nil), steps...)
			status.Status = "failed"
			u.mu.Unlock()
		}
	}

	updateStep := UpdateModePull
	updateArgs := []string{"pull"}
	if u.opts.UpdateMode == UpdateModeBuild {
		updateStep = UpdateModeBuild
		// The migration image is built too; otherwise local source changes can
		// leave migrations stale even when api/worker were rebuilt.
		updateArgs = []string{"--profile", "tools", "build"}
	}
	updateArgs = append(updateArgs, u.opts.Services...)
	if u.opts.UpdateMode == UpdateModeBuild {
		updateArgs = append(updateArgs, u.opts.MigrateService)
	}
	runStep(updateStep, func() (string, error) {
		args := append([]string{"compose"}, u.composeArgs()...)
		args = append(args, updateArgs...)
		return u.opts.Exec(ctx, "docker", args...)
	})
	if lastStepFailed(steps) {
		return u.fail(status, steps), ErrStepFailed
	}

	runStep("migrate", func() (string, error) {
		args := append([]string{"compose"}, u.composeArgs()...)
		args = append(args, "run", "--rm", u.opts.MigrateService)
		return u.opts.Exec(ctx, "docker", args...)
	})
	if lastStepFailed(steps) {
		return u.fail(status, steps), ErrStepFailed
	}

	runStep("restart", func() (string, error) {
		// --no-deps avoids recreating postgres / control / coredns.
		// --force-recreate guarantees the container is replaced even if the
		// image tag is identical (defensive; pull above should have changed it).
		args := append([]string{"compose"}, u.composeArgs()...)
		args = append(args, "up", "-d", "--no-deps", "--force-recreate")
		args = append(args, u.opts.Services...)
		return u.opts.Exec(ctx, "docker", args...)
	})
	if lastStepFailed(steps) {
		return u.fail(status, steps), ErrStepFailed
	}

	runStep("verify", func() (string, error) {
		if err := u.waitHealthy(ctx); err != nil {
			return "", err
		}
		return u.opts.HealthURL + " → 2xx", nil
	})
	if lastStepFailed(steps) {
		return u.fail(status, steps), ErrStepFailed
	}

	// Bootstrap: atomically write a request to the shared volume so the
	// updater-bootstrap sidecar can build/pull the new image and replace this
	// container. We do NOT wait for the sidecar — it will recreate us.
	runStep("bootstrap", func() (string, error) {
		if u.opts.BootstrapRequestFile == "" {
			return "bootstrap request file not configured; skipping", nil
		}
		jsonReq := fmt.Sprintf(`{"targetVersion":%q,"requestedAt":%q}`,
			targetVersion, time.Now().UTC().Format(time.RFC3339))
		dir := filepath.Dir(u.opts.BootstrapRequestFile)
		tmp, err := atomicWriteFile(dir, "bootstrap-", []byte(jsonReq))
		if err != nil {
			return "", fmt.Errorf("atomicWriteFile: %w", err)
		}
		if err := os.Rename(tmp, u.opts.BootstrapRequestFile); err != nil {
			_ = os.Remove(tmp)
			return "", fmt.Errorf("rename tmp→request: %w", err)
		}
		return fmt.Sprintf("bootstrap request written to %s", u.opts.BootstrapRequestFile), nil
	})
	if lastStepFailed(steps) {
		return u.fail(status, steps), ErrBootstrapRequestFailed
	}

	u.mu.Lock()
	status.Steps = append([]StepResult(nil), steps...)
	status.Status = "ok"
	u.mu.Unlock()
	return status, nil
}

func (u *Updater) fail(status *Status, steps []StepResult) *Status {
	u.mu.Lock()
	defer u.mu.Unlock()
	status.Steps = append([]StepResult(nil), steps...)
	status.Status = "failed"
	return status
}

func normalizeUpdateMode(mode string) string {
	if strings.EqualFold(strings.TrimSpace(mode), UpdateModeBuild) {
		return UpdateModeBuild
	}
	return UpdateModePull
}

func lastStepFailed(steps []StepResult) bool {
	if len(steps) == 0 {
		return false
	}
	return steps[len(steps)-1].Status == "failed"
}

// composeArgs returns the shared `-f file -p project [--env-file file] [extra...]` prefix.
func (u *Updater) composeArgs() []string {
	args := []string{"-f", u.opts.ComposeFile, "-p", u.opts.ProjectName}
	if u.opts.EnvFile != "" {
		args = append(args, "--env-file", u.opts.EnvFile)
	}
	args = append(args, u.opts.ComposeExtraArgs...)
	return args
}

func (u *Updater) waitHealthy(ctx context.Context) error {
	deadline := time.Now().Add(u.opts.HealthTimeout)
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("health check did not return 2xx within %s", u.opts.HealthTimeout)
		}
		status, err := u.opts.HTTPGet(ctx, u.opts.HealthURL)
		if err == nil && status >= 200 && status < 300 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(u.opts.PollInterval):
		}
	}
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// defaultExec shells out to the host's binary, inheriting the parent process
// environment. Returns combined stdout+stderr.
func defaultExec(ctx context.Context, name string, args ...string) (string, error) {
	// #nosec G204 -- caller controls name+args; this runs inside a container
	// with tightly-scoped env and the docker socket mounted read-write.
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // see comment above
	stdout, err := cmd.CombinedOutput()
	if err != nil {
		return string(stdout), fmt.Errorf("%s: %w", name, err)
	}
	return string(stdout), nil
}

func defaultHTTPGet(ctx context.Context, url string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}

// ErrAlreadyRunning is returned when Apply is called while a previous run is
// still in progress.
var ErrAlreadyRunning = errors.New("updater: an update is already in progress")

// ErrStepFailed indicates that one of the orchestrated steps failed.
var ErrStepFailed = errors.New("updater: a step failed; see Status.Steps")

// ErrBootstrapRequestFailed indicates the bootstrap request could not be
// written; the update succeeded but the sidecar cannot self-replace.
var ErrBootstrapRequestFailed = errors.New("updater: bootstrap request write failed")

// atomicWriteFile writes data to a new file inside dir with the given prefix
// and returns the absolute path. The caller is responsible for renaming or
// removing it. The file is created with 0o600 permissions.
func atomicWriteFile(dir, prefix string, data []byte) (string, error) {
	f, err := os.CreateTemp(dir, prefix)
	if err != nil {
		return "", fmt.Errorf("CreateTemp: %w", err)
	}
	path := f.Name()
	_, err = f.Write(data)
	_ = f.Close()
	if err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("write: %w", err)
	}
	return path, nil
}
