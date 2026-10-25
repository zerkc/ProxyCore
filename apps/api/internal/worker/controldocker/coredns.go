package controldocker

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// CorednsLiveCorefilePath is the persisted Corefile below the config root.
func CorednsLiveCorefilePath(configRoot string) string {
	return filepath.Join(configRoot, "Corefile")
}

func corefileLooksApplied(contents []byte) bool {
	return strings.Contains(string(contents), "file /etc/coredns/zones/")
}

// SeedLiveCorefileFromCandidates restores the newest applied Corefile when the
// persisted one is still the image default. It returns the source path, or "" if
// nothing was seeded.
func SeedLiveCorefileFromCandidates(candidateRoot, configRoot string) (string, error) {
	if err := os.MkdirAll(configRoot, 0o755); err != nil {
		return "", err
	}
	live := CorednsLiveCorefilePath(configRoot)
	if current, err := os.ReadFile(live); err == nil && corefileLooksApplied(current) {
		return "", nil
	}
	latest, err := findNewest(candidateRoot, "Corefile", "/coredns/Corefile", "coredns/Corefile")
	if err != nil || latest == "" {
		return "", err
	}
	contents, err := os.ReadFile(latest)
	if err != nil {
		return "", err
	}
	if !corefileLooksApplied(contents) {
		return "", nil
	}
	if err := os.WriteFile(live, contents, 0o644); err != nil {
		return "", err
	}
	return latest, nil
}

func (b *Backend) previousCorednsPaths() (corefile, zones string) {
	return filepath.Join(b.opts.CandidateRoot, ".proxycore-previous-coredns-corefile"),
		filepath.Join(b.opts.CandidateRoot, ".proxycore-previous-coredns-zones")
}

func (b *Backend) promoteCoreDNS(ctx context.Context, name, candidatePath string) error {
	live := CorednsLiveCorefilePath(b.opts.ConfigRoot)
	previousCorefilePath, previousZonesPath := b.previousCorednsPaths()

	if err := os.MkdirAll(b.opts.ConfigRoot, 0o755); err != nil {
		return err
	}
	previous, err := os.ReadFile(live)
	if err != nil {
		previous, err = b.readArchiveFile(ctx, name, "/etc/coredns-config/Corefile")
		if err != nil {
			previous, err = b.readArchiveFile(ctx, name, "/etc/coredns/Corefile")
			if err != nil {
				return err
			}
		}
	}
	if err := os.WriteFile(previousCorefilePath, previous, 0o600); err != nil {
		return err
	}
	if err := os.RemoveAll(previousZonesPath); err != nil {
		return err
	}
	if err := copyDirectory(b.opts.ZonesRoot, previousZonesPath); err != nil {
		return err
	}

	err = func() error {
		if err := replaceDirectory(b.opts.ZonesRoot, filepath.Join(candidatePath, "zones")); err != nil {
			return err
		}
		corefile, err := os.ReadFile(filepath.Join(candidatePath, "Corefile"))
		if err != nil {
			return err
		}
		// Persist on the shared volume so a container recreate keeps the config.
		if err := os.WriteFile(live, corefile, 0o644); err != nil {
			return err
		}
		return b.putFile(ctx, name, "/etc/coredns-config", "Corefile", corefile)
	}()
	if err != nil {
		_ = os.WriteFile(live, previous, 0o644)
		_ = replaceDirectory(b.opts.ZonesRoot, previousZonesPath)
		return err
	}
	return nil
}

func (b *Backend) rollbackCoreDNS(ctx context.Context, name string) error {
	live := CorednsLiveCorefilePath(b.opts.ConfigRoot)
	previousCorefilePath, previousZonesPath := b.previousCorednsPaths()
	previous, err := os.ReadFile(previousCorefilePath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(b.opts.ConfigRoot, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(live, previous, 0o644); err != nil {
		return err
	}
	if err := b.putFile(ctx, name, "/etc/coredns-config", "Corefile", previous); err != nil {
		return err
	}
	return replaceDirectory(b.opts.ZonesRoot, previousZonesPath)
}

func replaceDirectory(target, source string) error {
	if err := clearDirectory(target); err != nil {
		return err
	}
	return copyDirectory(source, target)
}

func clearDirectory(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(dir, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

// copyDirectory copies regular files and directories (mode 0644 for files). It
// creates the source directory when missing, like the TS helper.
func copyDirectory(source, target string) error {
	if err := os.MkdirAll(source, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(source)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		from, to := filepath.Join(source, entry.Name()), filepath.Join(target, entry.Name())
		if entry.IsDir() {
			if err := copyDirectory(from, to); err != nil {
				return err
			}
			continue
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("Unsupported file type in CoreDNS zone directory: %s", entry.Name())
		}
		if err := copyFile(from, to); err != nil {
			return err
		}
		if err := os.Chmod(to, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(from, to string) (err error) {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := out.Close(); err == nil {
			err = cerr
		}
	}()
	_, err = io.Copy(out, in)
	return err
}
