package controldocker

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

// Fixed stable-config file names below the candidate root.
const (
	NginxStableConfig         = "nginx-live.conf"
	NginxPreviousStableConfig = "nginx-previous-live.conf"

	nginxManagedMarker       = "map $http_upgrade $connection_upgrade"
	nginxBakedFallbackMarker = "return 503;"
)

// Observation is the result of classifying the active Nginx config.
type Observation struct {
	Status string // "healthy" or "drift"
	Reason string // set when Status is "drift"
}

func drift(reason string) Observation { return Observation{Status: "drift", Reason: reason} }

func NginxLiveConfigPath(candidateRoot string) string {
	return filepath.Join(candidateRoot, NginxStableConfig)
}

func NginxPreviousStableConfigPath(candidateRoot string) string {
	return filepath.Join(candidateRoot, NginxPreviousStableConfig)
}

// ClassifyNginxActiveConfig compares the active config with the persisted
// promoted one. The optional candidate selects the explicit 3-argument form of
// the TS function (arguments.length >= 3): passing it, even as nil, makes the
// candidate the expected contents; omitting it expects the stable config.
func ClassifyNginxActiveConfig(active, stable []byte, candidate ...[]byte) Observation {
	if len(active) == 0 {
		return drift("missing-active-config")
	}
	text := string(active)
	if strings.Contains(text, nginxBakedFallbackMarker) && !strings.Contains(text, nginxManagedMarker) {
		return drift("baked-fallback")
	}
	if len(stable) == 0 {
		return drift("missing-stable-config")
	}
	expected := stable
	if len(candidate) >= 1 {
		expected = candidate[0]
	}
	if len(expected) == 0 {
		return drift("missing-candidate-config")
	}
	if !bytes.Equal(active, stable) {
		return drift("mismatched-config")
	}
	if !bytes.Equal(active, expected) {
		return drift("mismatched-candidate-config")
	}
	if !strings.Contains(text, nginxManagedMarker) {
		return drift("unmanaged-config")
	}
	return Observation{Status: "healthy"}
}

func (b *Backend) probeNginxActiveConfig(ctx context.Context, name, candidatePath string) (Observation, error) {
	active, err := b.readArchiveFile(ctx, name, "/etc/nginx/nginx.conf")
	if err != nil {
		active = nil
	}
	stable, err := readFileIfPresent(NginxLiveConfigPath(b.opts.CandidateRoot))
	if err != nil {
		return Observation{}, err
	}
	candidate, err := readFileIfPresent(filepath.Join(candidatePath, "nginx.conf"))
	if err != nil {
		return Observation{}, err
	}
	observation := ClassifyNginxActiveConfig(active, stable, candidate)
	if observation.Status != "healthy" {
		return observation, nil
	}
	if err := b.exec(ctx, name, "nginx", "-t", "-c", "/etc/nginx/nginx.conf"); err != nil {
		return drift("invalid-active-config"), nil
	}
	return observation, nil
}

// FindNewestLegacyNginxCandidate returns the newest <root>/<rev>/nginx/nginx.conf
// or "" when none exists.
func FindNewestLegacyNginxCandidate(candidateRoot string) (string, error) {
	return findNewest(candidateRoot, "nginx.conf", "/nginx/nginx.conf", "nginx/nginx.conf")
}

// findNewest walks root and returns the file named base whose path contains
// infix or ends with suffix and has the newest mtime (the last wins on a tie).
// Unreadable directories are skipped, like the TS visitors.
func findNewest(root, base, infix, suffix string) (string, error) {
	best, bestMtime := "", int64(0)
	var visit func(dir string) error
	visit = func(dir string) error {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil
		}
		for _, entry := range entries {
			p := filepath.Join(dir, entry.Name())
			if entry.IsDir() {
				if err := visit(p); err != nil {
					return err
				}
				continue
			}
			if !entry.Type().IsRegular() || entry.Name() != base {
				continue
			}
			if !strings.Contains(p, infix) && !strings.HasSuffix(p, suffix) {
				continue
			}
			info, err := os.Stat(p)
			if err != nil {
				return err
			}
			if mtime := info.ModTime().UnixNano(); mtime >= bestMtime {
				best, bestMtime = p, mtime
			}
		}
		return nil
	}
	if err := visit(root); err != nil {
		return "", err
	}
	return best, nil
}

// WriteStableNginxConfig atomically writes the stable config (temp file +
// rename) keeping previous for rollback; an empty previous removes the old one.
func WriteStableNginxConfig(candidateRoot string, contents, previous []byte) error {
	stable, previousPath := NginxLiveConfigPath(candidateRoot), NginxPreviousStableConfigPath(candidateRoot)
	if err := os.MkdirAll(filepath.Dir(stable), 0o755); err != nil {
		return err
	}
	if len(previous) > 0 {
		if err := writeFileAtomically(previousPath, previous); err != nil {
			return err
		}
	} else if err := removeIfPresent(previousPath); err != nil {
		return err
	}
	return writeFileAtomically(stable, contents)
}

// readFileIfPresent returns nil (not an empty slice) for a missing file; an
// existing empty file yields a non-nil empty slice, as an empty Buffer is
// truthy in JS.
func readFileIfPresent(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if data == nil {
		data = []byte{}
	}
	return data, nil
}

func writeFileAtomically(path string, contents []byte) error {
	tmp := path + ".tmp-" + strconv.Itoa(os.Getpid()) + "-" + uuid.NewString()
	defer os.RemoveAll(tmp)
	if err := os.WriteFile(tmp, contents, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (b *Backend) readArchiveFile(ctx context.Context, name, path string) ([]byte, error) {
	archive, err := b.docker.GetArchive(ctx, name, path)
	if err != nil {
		return nil, err
	}
	return extractFirstFile(archive)
}

func (b *Backend) putFile(ctx context.Context, name, dir, file string, contents []byte) error {
	return b.docker.PutArchive(ctx, name, dir, createTar([]tarEntry{{Name: file, Contents: contents}}))
}

// promoteNginx copies the candidate into the container and persists it as the
// stable config only after the copy succeeded. On failure it restores the
// previous container config best effort and returns the original error.
func (b *Backend) promoteNginx(ctx context.Context, name, candidatePath string) error {
	candidateConfig := filepath.Join(candidatePath, "nginx.conf")
	contents, err := os.ReadFile(candidateConfig)
	if err != nil {
		return err
	}
	previous, perr := b.readArchiveFile(ctx, name, "/etc/nginx/nginx.conf")
	if perr != nil {
		previous = nil
	}
	// First apply or a recreated container may have no live config to back up.
	_ = b.exec(ctx, name, "cp", "/etc/nginx/nginx.conf", "/etc/nginx/.proxycore.previous.conf")

	err = b.exec(ctx, name, "cp", candidateConfig, "/etc/nginx/nginx.conf")
	if err == nil {
		err = WriteStableNginxConfig(b.opts.CandidateRoot, contents, previous)
	}
	if err != nil {
		if previous != nil {
			_ = b.putFile(ctx, name, "/etc/nginx", "nginx.conf", previous)
		} else {
			_ = b.exec(ctx, name, "cp", "/etc/nginx/.proxycore.previous.conf", "/etc/nginx/nginx.conf")
		}
		return err
	}
	return nil
}

// rollbackNginx restores the previous stable config, or the in-container backup
// of the first promotion when no previous stable config exists.
func (b *Backend) rollbackNginx(ctx context.Context, name string) error {
	stablePath := NginxLiveConfigPath(b.opts.CandidateRoot)
	previous, err := readFileIfPresent(NginxPreviousStableConfigPath(b.opts.CandidateRoot))
	if err != nil {
		return err
	}
	if previous != nil {
		current, err := readFileIfPresent(stablePath)
		if err != nil {
			return err
		}
		if err := writeFileAtomically(stablePath, previous); err != nil {
			return err
		}
		if err := b.putFile(ctx, name, "/etc/nginx", "nginx.conf", previous); err != nil {
			if current != nil {
				_ = writeFileAtomically(stablePath, current)
			}
			return err
		}
		return nil
	}
	if err := b.exec(ctx, name, "cp", "/etc/nginx/.proxycore.previous.conf", "/etc/nginx/nginx.conf"); err != nil {
		return err
	}
	return removeIfPresent(stablePath)
}

// removeIfPresent is rm(path, { force: true }): a missing file is fine, a
// non-empty directory is an error.
func removeIfPresent(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
