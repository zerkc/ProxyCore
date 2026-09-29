package zipwriter

import (
	"archive/zip"
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/zerkc/ProxyCore/apps/api/internal/backupcore/zipextract"
)

func TestWriterRejectsUnknownCompression(t *testing.T) {
	writer := Open(io.Discard, WithCompression(99))
	opened := false
	err := writer.Add("file.txt", 1, func() (io.ReadCloser, error) {
		opened = true
		return io.NopCloser(bytes.NewReader([]byte("x"))), nil
	})
	if !errors.Is(err, zipextract.ErrUnknownCompression) {
		t.Fatalf("Add unknown compression = %v, want %v", err, zipextract.ErrUnknownCompression)
	}
	if opened {
		t.Fatal("unknown compression opened its body")
	}
}

func TestWriterOptionsRecordCompressionAndComment(t *testing.T) {
	var output bytes.Buffer
	writer := Open(&output, WithCompression(zip.Store), WithComment("proxycore backup"), WithAggregateLimit(16))
	if err := writer.Add("file.txt", 3, func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader([]byte("abc"))), nil
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	archive, err := zip.NewReader(bytes.NewReader(output.Bytes()), int64(output.Len()))
	if err != nil {
		t.Fatalf("zip.NewReader: %v", err)
	}
	if archive.Comment != "proxycore backup" {
		t.Fatalf("archive comment = %q, want %q", archive.Comment, "proxycore backup")
	}
	if len(archive.File) != 1 || archive.File[0].Method != zip.Store {
		t.Fatalf("archive method = %d, want zip.Store", archive.File[0].Method)
	}
}
