package zipextract_test

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/zerkc/ProxyCore/apps/api/internal/backupcore/zipextract"
	"github.com/zerkc/ProxyCore/apps/api/internal/backupcore/zipwriter"
)

func TestOpenRawArchiveListsAndOpensFiles(t *testing.T) {
	raw := makeRawArchive(t, map[string][]byte{
		"manifest.json": []byte(`{"formatVersion":"1"}`),
		"db/users.json": []byte(`["alice"]`),
	})

	archive, err := zipextract.Open(context.Background(), bytes.NewReader(raw), nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer archive.Close()

	files, err := archive.Files(context.Background())
	if err != nil {
		t.Fatalf("Files: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("Files returned %d entries, want 2", len(files))
	}
	want := map[string][]byte{
		"manifest.json": []byte(`{"formatVersion":"1"}`),
		"db/users.json": []byte(`["alice"]`),
	}
	for _, file := range files {
		reader, err := file.Open()
		if err != nil {
			t.Fatalf("Open(%q): %v", file.Path, err)
		}
		got, readErr := io.ReadAll(reader)
		closeErr := reader.Close()
		if readErr != nil {
			t.Fatalf("read %q: %v", file.Path, readErr)
		}
		if closeErr != nil {
			t.Fatalf("close %q: %v", file.Path, closeErr)
		}
		if !bytes.Equal(got, want[file.Path]) {
			t.Fatalf("content mismatch for %q", file.Path)
		}
		if file.Size != int64(len(want[file.Path])) {
			t.Fatalf("size for %q = %d, want %d", file.Path, file.Size, len(want[file.Path]))
		}
	}
}

func TestOpenEncryptedArchiveWithRightPassphrase(t *testing.T) {
	const passphrase = "correct horse battery staple"
	encrypted := makeEncryptedArchive(t, passphrase, map[string][]byte{
		"manifest.json": []byte(`{"formatVersion":"1"}`),
		"db/users.json": []byte(`["alice"]`),
	})

	archive, err := zipextract.Open(context.Background(), bytes.NewReader(encrypted), []byte(passphrase))
	if err != nil {
		t.Fatalf("Open encrypted: %v", err)
	}
	defer archive.Close()
	files, err := archive.Files(context.Background())
	if err != nil {
		t.Fatalf("Files: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("Files returned %d entries, want 2", len(files))
	}
}

func TestOpenEncryptedArchiveWrongPassphraseDoesNotLeakPayload(t *testing.T) {
	const passphrase = "correct horse battery staple"
	const path = "private/secret.txt"
	const body = "top-secret backup body"
	encrypted := makeEncryptedArchive(t, passphrase, map[string][]byte{path: []byte(body)})

	_, err := zipextract.Open(context.Background(), bytes.NewReader(encrypted), []byte("wrong passphrase"))
	if !errors.Is(err, zipextract.ErrPassphraseMismatch) {
		t.Fatalf("Open wrong passphrase = %v, want %v", err, zipextract.ErrPassphraseMismatch)
	}
	if strings.Contains(err.Error(), path) || strings.Contains(err.Error(), body) {
		t.Fatalf("wrong-passphrase error leaked payload: %q", err)
	}
}

func TestOpenRawArchiveWithPassphraseRequiresEnvelope(t *testing.T) {
	raw := makeRawArchive(t, map[string][]byte{"file.txt": []byte("body")})
	_, err := zipextract.Open(context.Background(), bytes.NewReader(raw), []byte("unexpected"))
	if !errors.Is(err, zipextract.ErrPassphraseRequired) {
		t.Fatalf("Open raw archive with passphrase = %v, want %v", err, zipextract.ErrPassphraseRequired)
	}
}

func TestOpenRejectsEscapingCentralDirectoryEntryBeforeReturningFiles(t *testing.T) {
	var output bytes.Buffer
	writer := zip.NewWriter(&output)
	entry, err := writer.CreateHeader(&zip.FileHeader{Name: "../oops", Method: zip.Store})
	if err != nil {
		t.Fatalf("CreateHeader: %v", err)
	}
	if _, err := entry.Write([]byte("must not be exposed")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("zip Close: %v", err)
	}

	_, err = zipextract.Open(context.Background(), bytes.NewReader(output.Bytes()), nil)
	if !errors.Is(err, zipextract.ErrPathEscape) {
		t.Fatalf("Open escaping archive = %v, want %v", err, zipextract.ErrPathEscape)
	}
}

func makeRawArchive(t *testing.T, entries map[string][]byte) []byte {
	t.Helper()
	var output bytes.Buffer
	writer := zip.NewWriter(&output)
	for path, body := range entries {
		entry, err := writer.Create(path)
		if err != nil {
			t.Fatalf("Create(%q): %v", path, err)
		}
		if _, err := entry.Write(body); err != nil {
			t.Fatalf("Write(%q): %v", path, err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("zip Close: %v", err)
	}
	return output.Bytes()
}

func makeEncryptedArchive(t *testing.T, passphrase string, entries map[string][]byte) []byte {
	t.Helper()
	var output bytes.Buffer
	writer, err := zipwriter.OpenEncrypted(&output, []byte(passphrase))
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
		t.Fatalf("encrypted Close: %v", err)
	}
	return output.Bytes()
}
