package zipwriter

import (
	"archive/zip"
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/zerkc/ProxyCore/apps/api/internal/backupcore/zipextract"
)

func TestWriterRoundTripWithZeroFiles(t *testing.T) {
	var output bytes.Buffer
	writer := Open(&output)
	if err := writer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	archive, err := zip.NewReader(bytes.NewReader(output.Bytes()), int64(output.Len()))
	if err != nil {
		t.Fatalf("zip.NewReader: %v", err)
	}
	if len(archive.File) != 0 {
		t.Fatalf("archive has %d files, want zero", len(archive.File))
	}
}

func TestWriterRoundTripStreamsTextAndBinaryFiles(t *testing.T) {
	text := []byte("hello from the backup writer\n")
	binary := make([]byte, 4*1024)
	for index := range binary {
		binary[index] = byte((index*37 + 11) % 251)
	}

	var output bytes.Buffer
	writer := Open(&output)
	for _, file := range []struct {
		path string
		data []byte
	}{
		{path: "notes/readme.txt", data: text},
		{path: "db/blob.bin", data: binary},
	} {
		data := append([]byte(nil), file.data...)
		if err := writer.Add(file.path, int64(len(data)), func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(data)), nil
		}); err != nil {
			t.Fatalf("Add(%q): %v", file.path, err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	archive, err := zip.NewReader(bytes.NewReader(output.Bytes()), int64(output.Len()))
	if err != nil {
		t.Fatalf("zip.NewReader: %v", err)
	}
	want := map[string][]byte{"notes/readme.txt": text, "db/blob.bin": binary}
	if len(archive.File) != len(want) {
		t.Fatalf("archive has %d files, want %d", len(archive.File), len(want))
	}
	for _, entry := range archive.File {
		reader, err := entry.Open()
		if err != nil {
			t.Fatalf("Open(%q): %v", entry.Name, err)
		}
		got, readErr := io.ReadAll(reader)
		closeErr := reader.Close()
		if readErr != nil {
			t.Fatalf("read %q: %v", entry.Name, readErr)
		}
		if closeErr != nil {
			t.Fatalf("close %q: %v", entry.Name, closeErr)
		}
		if !bytes.Equal(got, want[entry.Name]) {
			t.Fatalf("content mismatch for %q", entry.Name)
		}
	}
}

func TestWriterRejectsUnsafeAndDuplicatePaths(t *testing.T) {
	tests := []struct {
		path string
		want error
	}{
		{path: "/etc/passwd", want: zipextract.ErrAbsolutePath},
		{path: "../oops", want: zipextract.ErrPathEscape},
		{path: "C:/x", want: zipextract.ErrDrivePath},
		{path: `\foo`, want: zipextract.ErrBackslashPath},
		{path: "", want: zipextract.ErrEmptyPath},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			writer := Open(io.Discard)
			opened := false
			err := writer.Add(tt.path, 1, func() (io.ReadCloser, error) {
				opened = true
				return io.NopCloser(bytes.NewReader([]byte("x"))), nil
			})
			if !errors.Is(err, tt.want) {
				t.Fatalf("Add(%q) = %v, want errors.Is(..., %v)", tt.path, err, tt.want)
			}
			if opened {
				t.Fatal("unsafe path opened its body")
			}
		})
	}

	writer := Open(io.Discard)
	if err := writer.Add("duplicate.txt", 1, func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader([]byte("x"))), nil
	}); err != nil {
		t.Fatalf("first Add: %v", err)
	}
	if err := writer.Add("duplicate.txt", 1, func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader([]byte("y"))), nil
	}); !errors.Is(err, zipextract.ErrDuplicatePath) {
		t.Fatalf("duplicate Add = %v, want %v", err, zipextract.ErrDuplicatePath)
	}
}

func TestWriterRejectsClaimedFileLargerThanTwoGiBWithoutOpening(t *testing.T) {
	writer := Open(io.Discard)
	opened := false
	err := writer.Add("large.bin", 2<<30+1, func() (io.ReadCloser, error) {
		opened = true
		return io.NopCloser(bytes.NewReader(nil)), nil
	})
	if !errors.Is(err, zipextract.ErrFileTooLarge) {
		t.Fatalf("Add oversized file = %v, want %v", err, zipextract.ErrFileTooLarge)
	}
	if opened {
		t.Fatal("oversized file opener was called")
	}
}

func TestWriterAggregateLimitAllowsBoundaryAndRejectsOnlyCrossing(t *testing.T) {
	var output bytes.Buffer
	writer := Open(&output, WithAggregateLimit(5))
	if err := writer.Add("first", 5, func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader([]byte("12345"))), nil
	}); err != nil {
		t.Fatalf("Add boundary-sized file: %v", err)
	}

	opened := false
	err := writer.Add("crossing", 1, func() (io.ReadCloser, error) {
		opened = true
		return io.NopCloser(bytes.NewReader([]byte("6"))), nil
	})
	if !errors.Is(err, zipextract.ErrAggregateLimitExceeded) {
		t.Fatalf("Add crossing file = %v, want %v", err, zipextract.ErrAggregateLimitExceeded)
	}
	if opened {
		t.Fatal("aggregate-limit rejection opened its body")
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestWriterCloseIsIdempotent(t *testing.T) {
	var output bytes.Buffer
	writer := Open(&output)
	first := writer.Close()
	second := writer.Close()
	if first != nil || second != nil {
		t.Fatalf("Close results = (%v, %v), want nil, nil", first, second)
	}
}
