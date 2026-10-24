package apply_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/zerkc/ProxyCore/apps/api/internal/worker/apply"
	"github.com/zerkc/ProxyCore/apps/api/internal/worker/jobs"
	"github.com/zerkc/ProxyCore/apps/api/internal/worker/store"
)

func TestApplyWritesCandidateFilesReadableByNginx(t *testing.T) {
	e := newEnv(t)
	job := e.enqueue(store.TargetNginx, 1)
	candidate := e.candidate("nginx", job)
	candidate.Files = map[string]string{
		"nginx.conf":        "events {}\n",
		"certs/app.key":     "key\n",
		"basic-auth/secret": "operator:{SHA}x\n",
	}
	result, err := e.orch.Apply(context.Background(), job.ID, empty(), fixed(candidate))
	if err != nil || result.Status != jobs.Applied {
		t.Fatalf("result = %+v, %v", result, err)
	}
	for name, want := range candidate.Files {
		path := filepath.Join(candidate.CandidatePath, name)
		got, err := os.ReadFile(path)
		if err != nil || string(got) != want {
			t.Fatalf("%s = %q, %v", name, got, err)
		}
		info, _ := os.Stat(path)
		// Group/other must be able to read it (the umask can only remove write bits here).
		if info.Mode().Perm()&0o044 != 0o044 {
			t.Fatalf("%s mode = %v, want 0644", name, info.Mode().Perm())
		}
	}
}

func TestApplyCreatesTheCandidateDirectoryForAnEmptyFileSet(t *testing.T) {
	e := newEnv(t)
	job := e.enqueue(store.TargetNginx, 1)
	candidate := e.candidate("nginx", job)
	candidate.Files = map[string]string{}
	if _, err := e.orch.Apply(context.Background(), job.ID, empty(), fixed(candidate)); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(candidate.CandidatePath); err != nil || !info.IsDir() {
		t.Fatalf("candidate dir = %v, %v", info, err)
	}
}

func TestApplyWritesNothingWhenTheCandidateHasNoFiles(t *testing.T) {
	e := newEnv(t)
	job := e.enqueue(store.TargetNginx, 1)
	candidate := e.candidate("nginx", job)
	if _, err := e.orch.Apply(context.Background(), job.ID, empty(), fixed(candidate)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(candidate.CandidatePath); !os.IsNotExist(err) {
		t.Fatalf("stat err = %v", err)
	}
}

func TestApplyRejectsUnsafeCandidatePathsBeforeAnyControlCall(t *testing.T) {
	tests := []struct {
		name    string
		path    func(root, revision string) string
		files   map[string]string
		message string
	}{
		{"outside the root", func(root, rev string) string { return "/tmp/elsewhere/" + rev }, map[string]string{"a": "x"}, "Candidate path is outside the worker candidate root"},
		{"root itself", func(root, rev string) string { return root }, map[string]string{"a": "x"}, "Candidate path is outside the worker candidate root"},
		{"sibling prefix", func(root, rev string) string { return root + "-evil/" + rev }, map[string]string{"a": "x"}, "Candidate path is outside the worker candidate root"},
		{"dot dot in the path", func(root, rev string) string { return root + "/" + rev + "/../x" }, map[string]string{"a": "x"}, "Candidate path is outside the worker candidate root"},
		{"absolute file", func(root, rev string) string { return root + "/" + rev }, map[string]string{"/etc/passwd": "x"}, "Candidate file path is invalid"},
		{"traversing file", func(root, rev string) string { return root + "/" + rev }, map[string]string{"../escape": "x"}, "Candidate file path is invalid"},
		{"NUL in file", func(root, rev string) string { return root + "/" + rev }, map[string]string{"a\x00b": "x"}, "Candidate file path is invalid"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			job := e.enqueue(store.TargetNginx, 1)
			candidate := e.candidate("nginx", job)
			candidate.CandidatePath = tt.path(e.root, job.RevisionID)
			candidate.Files = tt.files
			result, err := e.orch.Apply(context.Background(), job.ID, empty(), fixed(candidate))
			if err != nil {
				t.Fatal(err)
			}
			if result.Status != jobs.Failed || *result.ErrorMessage != tt.message {
				t.Fatalf("result = %+v", result)
			}
			if len(e.control.operations()) != 0 {
				t.Fatalf("operations = %v", e.control.operations())
			}
			contains(t, e.logs[0], "step=write_candidate service=nginx")
			if _, err := os.Stat(filepath.Join(e.root, "..", "escape")); err == nil {
				t.Fatal("a file escaped the candidate root")
			}
		})
	}
}

type failingFS struct{ apply.OSFS }

func (failingFS) WriteFile(string, []byte, os.FileMode) error { return errors.New("disk full") }

func TestApplyFailsTheJobWhenTheFilesystemWriteFails(t *testing.T) {
	e := newEnv(t)
	e.orch.FS = failingFS{}
	job := e.enqueue(store.TargetNginx, 1)
	candidate := e.candidate("nginx", job)
	candidate.Files = map[string]string{"nginx.conf": "x"}
	result, err := e.orch.Apply(context.Background(), job.ID, empty(), fixed(candidate))
	if err != nil || result.Status != jobs.Failed || *result.ErrorMessage != "disk full" {
		t.Fatalf("result = %+v, %v", result, err)
	}
}
