package controldocker

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"strings"
	"testing"
)

// minimalTar is createMinimalTar from docker-control.test.ts.
func minimalTar(name string, contents []byte) []byte {
	header := make([]byte, 512)
	copy(header, name)
	copy(header[124:136], fmt.Sprintf("%011o\x00", len(contents)))
	header[156] = '0'
	copy(header[257:], "ustar")
	copy(header[263:], "00")
	for i := 148; i < 156; i++ {
		header[i] = ' '
	}
	sum := 0
	for _, b := range header {
		sum += int(b)
	}
	copy(header[148:154], fmt.Sprintf("%06o", sum))
	header[154] = 0
	header[155] = ' '
	out := append(header, contents...)
	out = append(out, make([]byte, (512-len(contents)%512)%512)...)
	return append(out, make([]byte, 1024)...)
}

func TestExtractFirstFileReadsAUstarArchive(t *testing.T) {
	payload := []byte("home.arpa:53 {\n    file /etc/coredns/zones/home.arpa.zone\n}\n")
	got, err := extractFirstFile(minimalTar("Corefile", payload))
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("extract = %q, %v", got, err)
	}
}

func TestExtractFirstFileSkipsNonRegularEntries(t *testing.T) {
	var buf bytes.Buffer
	w := tar.NewWriter(&buf)
	w.WriteHeader(&tar.Header{Name: "dir/", Typeflag: tar.TypeDir, Mode: 0o755})
	w.WriteHeader(&tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "x"})
	w.WriteHeader(&tar.Header{Name: "dir/real", Typeflag: tar.TypeReg, Mode: 0o644, Size: 4})
	w.Write([]byte("data"))
	w.Close()
	got, err := extractFirstFile(buf.Bytes())
	if err != nil || string(got) != "data" {
		t.Fatalf("extract = %q, %v", got, err)
	}
}

func TestExtractFirstFileAcceptsNulTypeFlag(t *testing.T) {
	archive := minimalTar("a", []byte("x"))
	archive[156] = 0
	got, err := extractFirstFile(archive)
	if err != nil || string(got) != "x" {
		t.Fatalf("extract = %q, %v", got, err)
	}
}

func TestExtractFirstFileReturnsNonNilEmptyPayload(t *testing.T) {
	got, err := extractFirstFile(minimalTar("empty", nil))
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("extract = %#v, %v", got, err)
	}
}

func TestExtractFirstFileTruncatedPayloadIsClamped(t *testing.T) {
	archive := minimalTar("Corefile", bytes.Repeat([]byte("a"), 100))[:512+40]
	got, err := extractFirstFile(archive)
	if err != nil || len(got) != 40 {
		t.Fatalf("extract = %d bytes, %v", len(got), err)
	}
}

func TestExtractFirstFileErrorsWithoutAFile(t *testing.T) {
	for name, archive := range map[string][]byte{
		"empty":    nil,
		"trailer":  make([]byte, 1024),
		"short":    make([]byte, 100),
		"dir only": dirOnlyTar(),
	} {
		_, err := extractFirstFile(archive)
		if err == nil || err.Error() != "CoreDNS Corefile archive did not contain a file" {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
}

func dirOnlyTar() []byte {
	var buf bytes.Buffer
	w := tar.NewWriter(&buf)
	w.WriteHeader(&tar.Header{Name: "d/", Typeflag: tar.TypeDir, Mode: 0o755})
	w.Close()
	return buf.Bytes()
}

func TestCreateTarProducesAStandardArchive(t *testing.T) {
	archive := createTar([]tarEntry{
		{Name: "Corefile", Contents: []byte("hello")},
		{Name: "big", Contents: bytes.Repeat([]byte("z"), 700)},
	})
	if len(archive) != 512+512+512+1024+1024 {
		t.Fatalf("archive length = %d", len(archive))
	}
	r := tar.NewReader(bytes.NewReader(archive))
	for _, want := range []struct {
		name string
		size int
	}{{"Corefile", 5}, {"big", 700}} {
		h, err := r.Next()
		if err != nil {
			t.Fatal(err)
		}
		if h.Name != want.name || h.Size != int64(want.size) || h.Mode != 0o644 || h.Uid != 0 || h.Gid != 0 ||
			h.Uname != "root" || h.Gname != "root" || h.Typeflag != tar.TypeReg || h.ModTime.Unix() != 0 {
			t.Fatalf("header = %+v", h)
		}
		if body, _ := io.ReadAll(r); len(body) != want.size {
			t.Fatalf("%s body = %d bytes", want.name, len(body))
		}
	}
	if _, err := r.Next(); err != io.EOF {
		t.Fatalf("expected end of archive, got %v", err)
	}
}

func TestCreateTarHeaderBytesMatchTheTypeScriptLayout(t *testing.T) {
	header := createTar([]tarEntry{{Name: "Corefile", Contents: []byte("hello")}})[:512]
	checks := map[string]string{
		"mode":     string(header[100:108]),
		"uid":      string(header[108:116]),
		"size":     string(header[124:136]),
		"mtime":    string(header[136:148]),
		"magic":    string(header[257:263]),
		"version":  string(header[263:265]),
		"uname":    strings.TrimRight(string(header[265:297]), "\x00"),
		"devmajor": string(header[329:337]),
	}
	want := map[string]string{
		"mode": "0000644\x00", "uid": "0000000\x00", "size": "00000000005\x00", "mtime": "00000000000\x00",
		"magic": "ustar\x00", "version": "00", "uname": "root", "devmajor": strings.Repeat("\x00", 8),
	}
	for k, v := range want {
		if checks[k] != v {
			t.Errorf("%s = %q, want %q", k, checks[k], v)
		}
	}
	if header[154] != 0 || header[155] != ' ' || header[156] != '0' {
		t.Errorf("checksum terminator or typeflag wrong: %v %v %v", header[154], header[155], header[156])
	}
}

func TestCreateTarRoundTripsThroughExtract(t *testing.T) {
	payload := bytes.Repeat([]byte("line\n"), 300)
	got, err := extractFirstFile(createTar([]tarEntry{{Name: "Corefile", Contents: payload}}))
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("round trip failed: %d bytes, %v", len(got), err)
	}
}
