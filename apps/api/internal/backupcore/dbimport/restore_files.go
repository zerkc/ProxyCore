package dbimport

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/zerkc/ProxyCore/apps/api/internal/backupcore"
	"github.com/zerkc/ProxyCore/apps/api/internal/backupcore/zipextract"
)

func restoreFiles(ctx context.Context, files map[string]zipextract.File, manifest backupcore.Manifest, envPath, candidateRoot, revisionID string, envMode os.FileMode) error {
	if manifest.EnvPresent {
		if envPath == "" {
			return fmt.Errorf("restore env: empty destination path")
		}
		envFile, ok := files["env/env"]
		if !ok {
			return fmt.Errorf("restore env: archive entry %q is missing", "env/env")
		}
		data, err := readArchiveFile(ctx, envFile)
		if err != nil {
			return fmt.Errorf("read env entry: %w", err)
		}
		if err := writeRestoredFile(envPath, data, envMode); err != nil {
			return fmt.Errorf("write env file: %w", err)
		}
	}

	certs := certificatePaths(files, manifest.Certs)
	for _, certPath := range certs {
		if err := contextError(ctx); err != nil {
			return err
		}
		if !strings.HasPrefix(certPath, "certs/") || certPath == "certs/" || backupcore.EscapesBundleRoot(certPath) {
			return fmt.Errorf("invalid certificate path %q", certPath)
		}
		file, ok := files[certPath]
		if !ok {
			return fmt.Errorf("restore certificate: archive entry %q is missing", certPath)
		}
		data, err := readArchiveFile(ctx, file)
		if err != nil {
			return fmt.Errorf("read certificate %q: %w", certPath, err)
		}
		relative := filepath.FromSlash(strings.TrimPrefix(certPath, "certs/"))
		destination := filepath.Join(candidateRoot, revisionID, "nginx", "certs", relative)
		if err := ensureWithin(filepath.Join(candidateRoot, revisionID, "nginx", "certs"), destination); err != nil {
			return fmt.Errorf("restore certificate %q: %w", certPath, err)
		}
		if err := writeRestoredFile(destination, data, 0600); err != nil {
			return fmt.Errorf("write certificate %q: %w", certPath, err)
		}
	}
	return nil
}

func certificatePaths(files map[string]zipextract.File, declared []string) []string {
	result := append([]string(nil), declared...)
	seen := make(map[string]struct{}, len(result))
	for _, path := range result {
		seen[path] = struct{}{}
	}
	var extra []string
	for path := range files {
		if !strings.HasPrefix(path, "certs/") || path == "certs/" {
			continue
		}
		if _, ok := seen[path]; ok {
			continue
		}
		extra = append(extra, path)
	}
	sort.Strings(extra)
	return append(result, extra...)
}

func writeRestoredFile(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	if err := os.WriteFile(path, data, mode); err != nil {
		return err
	}
	return os.Chmod(path, mode)
}

func ensureWithin(root, candidate string) error {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	candidateAbs, err := filepath.Abs(candidate)
	if err != nil {
		return err
	}
	relative, err := filepath.Rel(rootAbs, candidateAbs)
	if err != nil {
		return err
	}
	if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("path escapes destination root")
	}
	return nil
}
