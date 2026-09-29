package hostread

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const appliedRevisionQuery = `
SELECT id::text
FROM config_revisions
WHERE applied_at IS NOT NULL
ORDER BY applied_at DESC, id DESC
LIMIT 1`

type pgReader struct {
	pool *pgxpool.Pool
	cfg  Config
}

var _ HostSnapshotReader = (*pgReader)(nil)

// New constructs a host snapshot reader backed by the configuration database.
func New(pool *pgxpool.Pool, cfg Config) *pgReader {
	cfg.ApplyDefaults(nil)
	return &pgReader{pool: pool, cfg: cfg}
}

func (r *pgReader) EnvFile(context.Context) (NamedFile, error) {
	if r == nil {
		return NamedFile{}, errors.New("hostread: nil reader")
	}
	cfg := r.cfg
	cfg.ApplyDefaults(nil)
	envPath := cfg.EnvPath

	info, err := os.Stat(envPath)
	if err != nil {
		return NamedFile{}, err
	}
	return NamedFile{
		Path: "env/env",
		Size: info.Size(),
		Open: func() (io.ReadCloser, error) {
			return os.Open(envPath)
		},
	}, nil
}

func (r *pgReader) AppliedRevision(ctx context.Context) (string, error) {
	if r == nil {
		return "", errors.New("hostread: nil reader")
	}
	if r.pool == nil {
		return "", errors.New("hostread: nil database pool")
	}
	if ctx == nil {
		return "", errors.New("hostread: nil context")
	}

	var revisionID string
	if err := r.pool.QueryRow(ctx, appliedRevisionQuery).Scan(&revisionID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrNoAppliedRevision
		}
		return "", fmt.Errorf("hostread: query applied revision: %w", err)
	}
	return revisionID, nil
}

func (r *pgReader) AppliedCertFiles(_ context.Context, revisionID string) ([]NamedFile, error) {
	if r == nil {
		return nil, errors.New("hostread: nil reader")
	}
	cfg := r.cfg
	cfg.ApplyDefaults(nil)
	return appliedCertFiles(cfg.CandidateRoot, revisionID)
}

func appliedCertFiles(candidateRoot, revisionID string) ([]NamedFile, error) {
	if strings.ContainsAny(revisionID, `/\`) || filepath.IsAbs(revisionID) {
		return nil, fmt.Errorf("hostread: revision %q escapes candidate root", revisionID)
	}

	certRoot := filepath.Join(candidateRoot, revisionID, "nginx", "certs")
	if err := ensurePathWithin(candidateRoot, certRoot); err != nil {
		return nil, err
	}

	rootInfo, err := os.Lstat(certRoot)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []NamedFile{}, nil
		}
		return nil, fmt.Errorf("hostread: stat certificate directory: %w", err)
	}
	if err := ensureResolvedPathWithin(candidateRoot, certRoot); err != nil {
		return nil, err
	}
	if !rootInfo.IsDir() {
		return nil, fmt.Errorf("hostread: certificate path %q is not a directory", certRoot)
	}

	entries, err := os.ReadDir(certRoot)
	if err != nil {
		return nil, fmt.Errorf("hostread: read certificate directory: %w", err)
	}
	files := make([]NamedFile, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			// Certificates are direct children. Nested staging directories are
			// intentionally not part of the applied snapshot.
			continue
		}
		name := entry.Name()
		ext := filepath.Ext(name)
		if !strings.EqualFold(ext, ".crt") && !strings.EqualFold(ext, ".key") {
			continue
		}
		if strings.ContainsAny(name, `/\`) {
			return nil, fmt.Errorf("hostread: certificate entry %q escapes candidate root", name)
		}

		filePath := filepath.Join(certRoot, name)
		if err := ensurePathWithin(candidateRoot, filePath); err != nil {
			return nil, err
		}
		if err := ensureResolvedPathWithin(candidateRoot, filePath); err != nil {
			return nil, err
		}
		info, err := os.Stat(filePath)
		if err != nil {
			return nil, fmt.Errorf("hostread: stat certificate %q: %w", name, err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("hostread: certificate %q is not a regular file", name)
		}

		certificateID := strings.TrimSuffix(name, ext)
		files = append(files, NamedFile{
			Path: "certs/" + certificateID + ext,
			Size: info.Size(),
			Open: func() (io.ReadCloser, error) {
				return os.Open(filePath)
			},
		})
	}
	return files, nil
}

func ensurePathWithin(root, path string) error {
	within, err := pathWithin(root, path)
	if err != nil {
		return fmt.Errorf("hostread: resolve candidate path: %w", err)
	}
	if !within {
		return fmt.Errorf("hostread: path %q escapes candidate root %q", path, root)
	}
	return nil
}

func ensureResolvedPathWithin(root, path string) error {
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("hostread: resolve candidate root: %w", err)
	}
	resolvedPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("hostread: resolve candidate path: %w", err)
	}
	within, err := pathWithin(resolvedRoot, resolvedPath)
	if err != nil {
		return fmt.Errorf("hostread: resolve candidate path: %w", err)
	}
	if !within {
		return fmt.Errorf("hostread: resolved path %q escapes candidate root %q", path, root)
	}
	return nil
}

func pathWithin(root, path string) (bool, error) {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return false, err
	}
	pathAbs, err := filepath.Abs(path)
	if err != nil {
		return false, err
	}
	relative, err := filepath.Rel(filepath.Clean(rootAbs), filepath.Clean(pathAbs))
	if err != nil {
		return false, err
	}
	if relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) || filepath.IsAbs(relative) {
		return false, nil
	}
	return true, nil
}
