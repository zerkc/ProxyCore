package backupcore

import (
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
)

func namedStringFile(path, value string) NamedFile {
	return NamedFile{
		Path: path,
		Size: int64(len(value)),
		Open: func() (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader(value)), nil
		},
	}
}

func TestComputeEntryChecksumsKnownAnswer(t *testing.T) {
	checksums, err := ComputeEntryChecksums([]NamedFile{namedStringFile("db/users.json", "hello world")})
	if err != nil {
		t.Fatalf("ComputeEntryChecksums: %v", err)
	}
	if got, want := checksums["db/users.json"], "b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9"; got != want {
		t.Fatalf("checksum = %q, want %q", got, want)
	}
}

func TestComputeEntryChecksumsIsIndependentOfInputOrder(t *testing.T) {
	first, err := ComputeEntryChecksums([]NamedFile{
		namedStringFile("z.txt", "z"),
		namedStringFile("a.txt", "a"),
	})
	if err != nil {
		t.Fatalf("ComputeEntryChecksums(first): %v", err)
	}
	second, err := ComputeEntryChecksums([]NamedFile{
		namedStringFile("a.txt", "a"),
		namedStringFile("z.txt", "z"),
	})
	if err != nil {
		t.Fatalf("ComputeEntryChecksums(second): %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("checksums depend on input order:\nfirst %#v\nsecond %#v", first, second)
	}
}

func TestComputeEntryChecksumsReturnsOpenError(t *testing.T) {
	wantErr := errors.New("file is unreadable")
	_, err := ComputeEntryChecksums([]NamedFile{{
		Path: "db/users.json",
		Open: func() (io.ReadCloser, error) { return nil, wantErr },
	}})
	if err == nil {
		t.Fatal("ComputeEntryChecksums accepted an unreadable file")
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want wrapped %v", err, wantErr)
	}
}

type closeTrackingReader struct {
	io.Reader
	closed *bool
}

func (r *closeTrackingReader) Close() error {
	*r.closed = true
	return nil
}

func TestComputeEntryChecksumsClosesReaderWhenOpenReturnsReaderAndError(t *testing.T) {
	wantErr := errors.New("open failed after allocating reader")
	closed := false
	_, err := ComputeEntryChecksums([]NamedFile{{
		Path: "db/users.json",
		Open: func() (io.ReadCloser, error) {
			return &closeTrackingReader{Reader: strings.NewReader("unused"), closed: &closed}, wantErr
		},
	}})
	if err == nil {
		t.Fatal("ComputeEntryChecksums accepted an opener error")
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want wrapped %v", err, wantErr)
	}
	if !closed {
		t.Fatal("ComputeEntryChecksums did not close the reader returned with an opener error")
	}
}
