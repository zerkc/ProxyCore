package dbexport

import (
	"fmt"
	"io"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/secrets"
)

const (
	// BackupMasterKeyMarkerID is reserved for the bundle-only master-key marker.
	BackupMasterKeyMarkerID = "00000000-0000-0000-0000-000000000000"
	// BackupMasterKeyMarkerPurpose identifies the row used for deterministic
	// master-key verification during import.
	BackupMasterKeyMarkerPurpose = "__backup_master_key_marker__"
	// BackupMasterKeyMarkerPlaintext is the known plaintext authenticated by
	// the marker ciphertext.
	BackupMasterKeyMarkerPlaintext = "proxycore-backup-master-key"
)

// ExporterOptions controls the database tables selected by an Exporter.
type ExporterOptions struct {
	Tables          []string
	Now             func() time.Time
	MasterKeyBase64 string
}

// masterKeyMarker returns the bundle-only row that lets import verify the
// master key before any table is replaced.
func masterKeyMarker(now func() time.Time, masterKeyBase64 string) (map[string]any, error) {
	ciphertext, err := secrets.EncryptSecret(BackupMasterKeyMarkerPlaintext, masterKeyBase64)
	if err != nil {
		return nil, fmt.Errorf("encrypt backup master-key marker: %w", err)
	}
	if now == nil {
		now = time.Now
	}
	timestamp := now().UTC().Format(time.RFC3339Nano)
	return map[string]any{
		"id":         BackupMasterKeyMarkerID,
		"purpose":    BackupMasterKeyMarkerPurpose,
		"ciphertext": ciphertext,
		"created_at": timestamp,
		"updated_at": timestamp,
	}, nil
}

// countingReadCloser records the bytes consumed by the ZipWriter without
// buffering the entry or requiring a second pass over the database rows.
type countingReadCloser struct {
	io.Reader
	closer io.Closer
	count  *int64
}

func (r *countingReadCloser) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	*r.count += int64(n)
	return n, err
}

func (r *countingReadCloser) Close() error {
	if r.closer != nil {
		return r.closer.Close()
	}
	if closer, ok := r.Reader.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}
