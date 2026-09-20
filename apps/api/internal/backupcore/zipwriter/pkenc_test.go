package zipwriter

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/zerkc/ProxyCore/apps/api/internal/backupcore/zipextract"
)

func TestEncryptedBytesDoNotContainPlaintextSample(t *testing.T) {
	const secret = "known-secret-sample-that-must-not-appear"
	var output bytes.Buffer
	writer, err := OpenEncrypted(&output, []byte("correct horse battery staple"))
	if err != nil {
		t.Fatalf("OpenEncrypted: %v", err)
	}
	if err := writer.Add("private/secret.txt", int64(len(secret)), func() (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(secret)), nil
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !bytes.HasPrefix(output.Bytes(), []byte("BX01")) {
		t.Fatal("encrypted archive does not have BX01 envelope magic")
	}
	if bytes.Contains(output.Bytes(), []byte(secret)) || bytes.Contains(output.Bytes(), []byte("private/secret.txt")) {
		t.Fatal("encrypted archive contains a plaintext payload sample")
	}
}

func TestEncryptedArchiveRoundTripThroughExtractor(t *testing.T) {
	const passphrase = "correct horse battery staple"
	entries := map[string][]byte{
		"manifest.json": []byte(`{"formatVersion":"1"}`),
		"db/blob.bin":   randomBytes(t, 4096),
	}
	var output bytes.Buffer
	writer, err := OpenEncrypted(&output, []byte(passphrase))
	if err != nil {
		t.Fatalf("OpenEncrypted: %v", err)
	}
	for path, body := range entries {
		data := append([]byte(nil), body...)
		if err := writer.Add(path, int64(len(data)), func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(data)), nil
		}); err != nil {
			t.Fatalf("Add(%q): %v", path, err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	archive, err := zipextract.Open(context.Background(), bytes.NewReader(output.Bytes()), []byte(passphrase))
	if err != nil {
		t.Fatalf("zipextract.Open: %v", err)
	}
	defer archive.Close()
	files, err := archive.Files(context.Background())
	if err != nil {
		t.Fatalf("Files: %v", err)
	}
	if len(files) != len(entries) {
		t.Fatalf("Files returned %d entries, want %d", len(files), len(entries))
	}
	for _, file := range files {
		reader, err := file.Open()
		if err != nil {
			t.Fatalf("Open(%q): %v", file.Path, err)
		}
		got, readErr := io.ReadAll(reader)
		closeErr := reader.Close()
		if readErr != nil || closeErr != nil {
			t.Fatalf("read/close %q: read=%v close=%v", file.Path, readErr, closeErr)
		}
		if !bytes.Equal(got, entries[file.Path]) {
			t.Fatalf("content mismatch for %q", file.Path)
		}
	}
}

func TestEncryptedArchiveRoundTripsAcrossChunkBoundary(t *testing.T) {
	const passphrase = "correct horse battery staple"
	body := make([]byte, 64*1024+17)
	for index := range body {
		body[index] = byte((index*29 + 7) % 251)
	}
	var output bytes.Buffer
	writer, err := OpenEncrypted(&output, []byte(passphrase))
	if err != nil {
		t.Fatalf("OpenEncrypted: %v", err)
	}
	if err := writer.Add("chunked.bin", int64(len(body)), func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	archive, err := zipextract.Open(context.Background(), bytes.NewReader(output.Bytes()), []byte(passphrase))
	if err != nil {
		t.Fatalf("zipextract.Open: %v", err)
	}
	defer archive.Close()
	files, err := archive.Files(context.Background())
	if err != nil {
		t.Fatalf("Files: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("Files returned %d entries, want 1", len(files))
	}
	reader, err := files[0].Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	got, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("read/close: read=%v close=%v", readErr, closeErr)
	}
	if !bytes.Equal(got, body) {
		t.Fatal("chunk-boundary content mismatch")
	}
}

func TestWrongPassphraseDoesNotLeakPayload(t *testing.T) {
	const passphrase = "correct horse battery staple"
	const path = "private/secret.txt"
	const body = "top-secret backup body"
	var output bytes.Buffer
	writer, err := OpenEncrypted(&output, []byte(passphrase))
	if err != nil {
		t.Fatalf("OpenEncrypted: %v", err)
	}
	if err := writer.Add(path, int64(len(body)), func() (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(body)), nil
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	_, err = zipextract.Open(context.Background(), bytes.NewReader(output.Bytes()), []byte("wrong passphrase"))
	if !errors.Is(err, zipextract.ErrPassphraseMismatch) {
		t.Fatalf("wrong passphrase = %v, want %v", err, zipextract.ErrPassphraseMismatch)
	}
	if strings.Contains(err.Error(), path) || strings.Contains(err.Error(), body) {
		t.Fatalf("wrong-passphrase error leaked payload: %q", err)
	}
}

func TestEmptyPassphraseProducesRawArchive(t *testing.T) {
	var output bytes.Buffer
	writer, err := OpenEncrypted(&output, nil)
	if err != nil {
		t.Fatalf("OpenEncrypted: %v", err)
	}
	if err := writer.Add("file.txt", 4, func() (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("body")), nil
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if bytes.HasPrefix(output.Bytes(), []byte("BX01")) {
		t.Fatal("empty passphrase produced an envelope")
	}
	archive, err := zip.NewReader(bytes.NewReader(output.Bytes()), int64(output.Len()))
	if err != nil {
		t.Fatalf("raw zip NewReader: %v", err)
	}
	if len(archive.File) != 1 || archive.File[0].Name != "file.txt" {
		t.Fatalf("raw archive entries = %v", archive.File)
	}
}

func TestEncryptedArchiveRejectsMalformedEnvelopeVariants(t *testing.T) {
	original := encryptedFixture(t)
	tests := []struct {
		name   string
		mutate func([]byte) []byte
		want   error
	}{
		{name: "truncated ciphertext", mutate: func(data []byte) []byte { return data[:len(data)-1] }, want: zipextract.ErrCorruptArchive},
		{name: "flipped bit", mutate: func(data []byte) []byte {
			copy := append([]byte(nil), data...)
			copy[len(copy)-5] ^= 0x80
			return copy
		}, want: zipextract.ErrCorruptArchive},
		{name: "wrong magic", mutate: func(data []byte) []byte {
			copy := append([]byte(nil), data...)
			copy[0] = 'Z'
			return copy
		}, want: zipextract.ErrCorruptArchive},
		{name: "wrong version", mutate: func(data []byte) []byte {
			copy := append([]byte(nil), data...)
			copy[4] = 0x02
			return copy
		}, want: zipextract.ErrUnsupportedVersion},
		{name: "wrong kdf id", mutate: func(data []byte) []byte {
			copy := append([]byte(nil), data...)
			copy[5] = 0x02
			return copy
		}, want: zipextract.ErrCorruptArchive},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			passphrase := []byte("correct horse battery staple")
			if tt.name == "wrong magic" {
				passphrase = nil
			}
			_, err := zipextract.Open(context.Background(), bytes.NewReader(tt.mutate(original)), passphrase)
			if !errors.Is(err, tt.want) {
				t.Fatalf("Open = %v, want errors.Is(..., %v)", err, tt.want)
			}
		})
	}
}

func TestPKEncUsesConstantTimeCompare(t *testing.T) {
	source, err := os.ReadFile("pkenc.go")
	if err != nil {
		t.Fatalf("ReadFile(pkenc.go): %v", err)
	}
	if !bytes.Contains(source, []byte("ConstantTimeCompare")) {
		t.Fatal("pkenc.go does not use crypto/subtle.ConstantTimeCompare")
	}
}

func encryptedFixture(t *testing.T) []byte {
	t.Helper()
	var output bytes.Buffer
	writer, err := OpenEncrypted(&output, []byte("correct horse battery staple"))
	if err != nil {
		t.Fatalf("OpenEncrypted: %v", err)
	}
	if err := writer.Add("file.txt", 4, func() (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("body")), nil
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return output.Bytes()
}

func randomBytes(t *testing.T, size int) []byte {
	t.Helper()
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}
	return data
}
