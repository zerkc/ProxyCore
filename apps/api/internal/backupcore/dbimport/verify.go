package dbimport

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/zerkc/ProxyCore/apps/api/internal/backupcore"
	"github.com/zerkc/ProxyCore/apps/api/internal/backupcore/zipextract"
)

func readManifest(ctx context.Context, files map[string]zipextract.File) (backupcore.Manifest, []byte, error) {
	file, ok := files["manifest.json"]
	if !ok {
		return backupcore.Manifest{}, nil, zipextract.ErrCorruptArchive
	}
	data, err := readArchiveFile(ctx, file)
	if err != nil {
		return backupcore.Manifest{}, nil, zipextract.ErrCorruptArchive
	}
	var manifest backupcore.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return backupcore.Manifest{}, nil, zipextract.ErrCorruptArchive
	}
	if err := backupcore.Validate(manifest); err != nil {
		return backupcore.Manifest{}, nil, zipextract.ErrCorruptArchive
	}
	return manifest, data, nil
}

func readArchiveFile(ctx context.Context, file zipextract.File) ([]byte, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	if file.Open == nil {
		return nil, errors.New("archive file has no opener")
	}
	reader, err := file.Open()
	if err != nil {
		return nil, err
	}
	if reader == nil {
		return nil, errors.New("archive file opener returned nil reader")
	}
	data, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	return data, errors.Join(readErr, closeErr)
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}

func verifyArchiveChecksums(ctx context.Context, files map[string]zipextract.File, manifest backupcore.Manifest) error {
	named := make([]backupcore.NamedFile, 0, len(files))
	for path, file := range files {
		if path == "manifest.json" {
			continue
		}
		path, file := path, file
		named = append(named, backupcore.NamedFile{
			Path: path,
			Size: file.Size,
			Open: file.Open,
		})
	}
	computed, err := backupcore.ComputeEntryChecksums(named)
	if err != nil {
		return fmt.Errorf("%w: compute entry checksums: %v", ErrChecksumMismatch, err)
	}
	if len(computed) != len(manifest.EntryChecksums) {
		return fmt.Errorf("%w: archive and manifest entry counts differ", ErrChecksumMismatch)
	}
	for path, want := range manifest.EntryChecksums {
		got, ok := computed[path]
		if !ok || !strings.EqualFold(got, want) {
			return fmt.Errorf("%w: entry %q", ErrChecksumMismatch, path)
		}
	}
	for path := range computed {
		if _, ok := manifest.EntryChecksums[path]; !ok {
			return fmt.Errorf("%w: unlisted entry %q", ErrChecksumMismatch, path)
		}
	}
	return nil
}

func manifestDigest(manifestBytes []byte) string {
	digest := sha256.Sum256(manifestBytes)
	return hex.EncodeToString(digest[:])
}
