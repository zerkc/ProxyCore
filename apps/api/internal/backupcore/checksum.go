package backupcore

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
)

// NamedFile is a bundle entry source. Open must return a fresh reader for the
// named file; Size is metadata for callers and is intentionally not used to
// buffer the file in memory.
type NamedFile struct {
	Path string
	Open func() (io.ReadCloser, error)
	Size int64
}

// ComputeEntryChecksums streams every named file through SHA-256 and returns
// the resulting lowercase hexadecimal digest keyed by its relative path.
func ComputeEntryChecksums(files []NamedFile) (map[string]string, error) {
	checksums := make(map[string]string, len(files))
	for _, file := range files {
		if _, exists := checksums[file.Path]; exists {
			return nil, fmt.Errorf("duplicate file path %q", file.Path)
		}
		if file.Open == nil {
			return nil, fmt.Errorf("open %q: nil opener", file.Path)
		}
		reader, err := file.Open()
		if err != nil {
			// Open may return both a partially allocated reader and an error;
			// release that reader before propagating the opener error.
			if reader != nil {
				_ = reader.Close()
			}
			return nil, fmt.Errorf("open %q: %w", file.Path, err)
		}
		if reader == nil {
			return nil, fmt.Errorf("open %q: returned nil reader", file.Path)
		}

		hasher := sha256.New()
		_, copyErr := io.Copy(hasher, reader)
		closeErr := reader.Close()
		if copyErr != nil {
			return nil, fmt.Errorf("read %q: %w", file.Path, copyErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("close %q: %w", file.Path, closeErr)
		}
		checksums[file.Path] = hex.EncodeToString(hasher.Sum(nil))
	}
	return checksums, nil
}
