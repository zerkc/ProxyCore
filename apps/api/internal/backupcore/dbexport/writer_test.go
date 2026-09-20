package dbexport

import (
	"bytes"
	"io"
	"testing"
)

type memWriter struct {
	files map[string][]byte
	bytes int64
}

func newMemWriter() *memWriter {
	return &memWriter{files: make(map[string][]byte)}
}

func (w *memWriter) Add(path string, _ int64, open func() (io.ReadCloser, error)) error {
	reader, err := open()
	if err != nil {
		return err
	}
	defer reader.Close()
	data, err := io.ReadAll(reader)
	if err != nil {
		return err
	}
	w.files[path] = append([]byte(nil), data...)
	w.bytes += int64(len(data))
	return nil
}

func TestMemWriterStoresJSONEntryAndCountsBytes(t *testing.T) {
	writer := newMemWriter()
	payload := []byte(`[{}]`)
	if err := writer.Add("db/example.json", int64(len(payload)), func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(payload)), nil
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if got := string(writer.files["db/example.json"]); got != string(payload) {
		t.Fatalf("stored payload = %q, want %q", got, payload)
	}
	if writer.bytes != int64(len(payload)) {
		t.Fatalf("bytes = %d, want %d", writer.bytes, len(payload))
	}
}

var _ ZipWriter = (*memWriter)(nil)
