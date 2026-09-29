package hostread

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func TestEnvFileUsesConfiguredPathAndReportsMissingPath(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	contents := []byte("PROXYCORE_MASTER_KEY_BASE64=test-only\n")
	if err := os.WriteFile(envPath, contents, 0o600); err != nil {
		t.Fatalf("write .env fixture: %v", err)
	}

	file, err := New(nil, Config{EnvPath: envPath}).EnvFile(context.Background())
	if err != nil {
		t.Fatalf("EnvFile: %v", err)
	}
	if file.Path != "env/env" {
		t.Fatalf("Path = %q, want %q", file.Path, "env/env")
	}
	if file.Size != int64(len(contents)) {
		t.Fatalf("Size = %d, want %d", file.Size, len(contents))
	}
	reader, err := file.Open()
	if err != nil {
		t.Fatalf("open NamedFile: %v", err)
	}
	got, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil {
		t.Fatalf("read NamedFile: %v", readErr)
	}
	if closeErr != nil {
		t.Fatalf("close NamedFile: %v", closeErr)
	}
	if string(got) != string(contents) {
		t.Fatalf("contents = %q, want %q", got, contents)
	}

	missing := filepath.Join(dir, "missing.env")
	_, err = New(nil, Config{EnvPath: missing}).EnvFile(context.Background())
	var pathErr *os.PathError
	if !errors.As(err, &pathErr) {
		t.Fatalf("missing EnvFile error = %v, want *os.PathError", err)
	}
}

func TestAppliedCertFilesListsDirectCaseInsensitiveFiles(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "candidates")
	revision := "revision-1"
	certDir := filepath.Join(root, revision, "nginx", "certs")
	if err := os.MkdirAll(filepath.Join(certDir, "nested"), 0o755); err != nil {
		t.Fatalf("create cert directories: %v", err)
	}

	fixtures := map[string]string{
		"a.crt":              "crt-a",
		"b.CRT":              "crt-b",
		"a.key":              "key-a",
		"b.KEY":              "key-b",
		"nested/ignored.crt": "ignored",
	}
	for name, contents := range fixtures {
		if err := os.WriteFile(filepath.Join(certDir, name), []byte(contents), 0o600); err != nil {
			t.Fatalf("write cert fixture %q: %v", name, err)
		}
	}

	files, err := New(nil, Config{CandidateRoot: root}).AppliedCertFiles(context.Background(), revision)
	if err != nil {
		t.Fatalf("AppliedCertFiles: %v", err)
	}
	got := make(map[string]int64, len(files))
	for _, file := range files {
		got[file.Path] = file.Size
	}
	want := map[string]int64{
		"certs/a.crt": int64(len("crt-a")),
		"certs/b.CRT": int64(len("crt-b")),
		"certs/a.key": int64(len("key-a")),
		"certs/b.KEY": int64(len("key-b")),
	}
	if len(files) != len(want) {
		t.Fatalf("file count = %d, want %d (%v)", len(files), len(want), got)
	}
	for name, size := range want {
		if got[name] != size {
			t.Fatalf("file %q size = %d, want %d", name, got[name], size)
		}
	}

	var names []string
	for _, file := range files {
		names = append(names, file.Path)
	}
	if !sort.StringsAreSorted(names) {
		t.Fatalf("file names = %v, want sorted output", names)
	}

	missing, err := New(nil, Config{CandidateRoot: root}).AppliedCertFiles(context.Background(), "missing-revision")
	if err != nil {
		t.Fatalf("missing cert directory: %v", err)
	}
	if len(missing) != 0 {
		t.Fatalf("missing cert directory returned %d files, want zero", len(missing))
	}
}

func TestAppliedCertFilesRejectsEscapingRevisionOrEntry(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "candidates")
	reader := New(nil, Config{CandidateRoot: root})

	tests := []struct {
		name       string
		revisionID string
	}{
		{name: "parent", revisionID: filepath.Join("..", "outside")},
		{name: "absolute", revisionID: filepath.Join(string(filepath.Separator), "outside")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := reader.AppliedCertFiles(context.Background(), tt.revisionID); err == nil {
				t.Fatalf("AppliedCertFiles(%q) returned nil error", tt.revisionID)
			}
		})
	}

	revision := "revision-symlink"
	certDir := filepath.Join(root, revision, "nginx", "certs")
	if err := os.MkdirAll(certDir, 0o755); err != nil {
		t.Fatalf("create symlink cert directory: %v", err)
	}
	outside := filepath.Join(dir, "outside.crt")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatalf("write outside fixture: %v", err)
	}
	link := filepath.Join(certDir, "escaped.crt")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := reader.AppliedCertFiles(context.Background(), revision); err == nil {
		t.Fatal("AppliedCertFiles accepted a certificate symlink outside CandidateRoot")
	}
}

func TestAppliedCertFilesRejectsExistingNonDirectory(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "candidates")
	revision := "revision-file"
	certPath := filepath.Join(root, revision, "nginx", "certs")
	if err := os.MkdirAll(filepath.Dir(certPath), 0o755); err != nil {
		t.Fatalf("create non-directory fixture parent: %v", err)
	}
	if err := os.WriteFile(certPath, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("write non-directory fixture: %v", err)
	}
	if _, err := New(nil, Config{CandidateRoot: root}).AppliedCertFiles(context.Background(), revision); err == nil {
		t.Fatal("AppliedCertFiles accepted a non-directory cert path")
	}
}
