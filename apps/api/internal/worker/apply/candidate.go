package apply

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/zerkc/ProxyCore/apps/api/internal/worker/control"
)

// DefaultCandidateRoot is where candidates live when no root is configured.
const DefaultCandidateRoot = "/var/lib/proxycore/candidates"

// Candidate is one rendered service tree. Files may be nil (nothing to write)
// or empty (the directory is created but stays empty), like the TS optional map.
type Candidate struct {
	Service       control.Service
	CandidatePath string
	Checksum      string
	Files         map[string]string
}

// FS is the filesystem the orchestrator writes candidates through.
type FS interface {
	MkdirAll(path string, perm os.FileMode) error
	WriteFile(name string, data []byte, perm os.FileMode) error
}

// OSFS writes to the real filesystem.
type OSFS struct{}

// MkdirAll implements FS.
func (OSFS) MkdirAll(path string, perm os.FileMode) error { return os.MkdirAll(path, perm) }

// WriteFile implements FS.
func (OSFS) WriteFile(name string, data []byte, perm os.FileMode) error {
	return os.WriteFile(name, data, perm)
}

// writeCandidate writes the candidate files under its path. The path must sit
// inside the candidate root and every relative file path must stay inside the
// path; all paths are checked before anything is written.
func writeCandidate(fs FS, candidate Candidate, candidateRoot string) error {
	if candidate.Files == nil {
		return nil
	}
	root := strings.TrimRight(candidateRoot, "/")
	if !strings.HasPrefix(candidate.CandidatePath, root+"/") || strings.Contains(candidate.CandidatePath, "..") {
		return errors.New("Candidate path is outside the worker candidate root")
	}
	names := make([]string, 0, len(candidate.Files))
	for name := range candidate.Files {
		if strings.HasPrefix(name, "/") || strings.Contains(name, "..") || strings.ContainsRune(name, 0) {
			return errors.New("Candidate file path is invalid")
		}
		names = append(names, name)
	}
	sort.Strings(names)
	if err := fs.MkdirAll(candidate.CandidatePath, 0o755); err != nil {
		return err
	}
	for _, name := range names {
		target := filepath.Join(candidate.CandidatePath, name)
		if err := fs.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		// Nginx workers must read auth/cert candidate files after reload.
		if err := fs.WriteFile(target, []byte(candidate.Files[name]), 0o644); err != nil {
			return err
		}
	}
	return nil
}
