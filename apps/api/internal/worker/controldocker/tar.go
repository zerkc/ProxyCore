package controldocker

import (
	"errors"
	"fmt"
	"strings"
)

const tarBlock = 512

type tarEntry struct {
	Name     string
	Contents []byte
}

// createTar builds a ustar archive byte for byte like createTar in
// docker-control.ts: mode 0644, uid/gid/mtime 0, typeflag '0', owner root. The
// standard library writer is not used because it also fills the device fields
// and may choose other header encodings, which would change the bytes Docker
// receives.
func createTar(entries []tarEntry) []byte {
	var out []byte
	for _, entry := range entries {
		header := make([]byte, tarBlock)
		copy(header[0:100], entry.Name)
		writeTarOctal(header, 100, 8, 0o644)
		writeTarOctal(header, 108, 8, 0)
		writeTarOctal(header, 116, 8, 0)
		writeTarOctal(header, 124, 12, uint64(len(entry.Contents)))
		writeTarOctal(header, 136, 12, 0)
		header[156] = '0'
		copy(header[257:263], "ustar")
		copy(header[263:265], "00")
		copy(header[265:297], "root")
		copy(header[297:329], "root")
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
		out = append(out, header...)
		out = append(out, entry.Contents...)
		out = append(out, make([]byte, (tarBlock-len(entry.Contents)%tarBlock)%tarBlock)...)
	}
	return append(out, make([]byte, 2*tarBlock)...)
}

func writeTarOctal(header []byte, offset, length int, value uint64) {
	text := fmt.Sprintf("%o", value)
	if pad := length - 1 - len(text); pad > 0 {
		text = strings.Repeat("0", pad) + text
	}
	copy(header[offset:offset+length], text+"\x00")
}

var errNoTarFile = errors.New("CoreDNS Corefile archive did not contain a file")

// extractFirstFile returns the payload of the first regular file of a ustar
// archive, mirroring extractFirstFileFromTar. The result is never nil.
func extractFirstFile(archive []byte) ([]byte, error) {
	offset := 0
	for offset+tarBlock <= len(archive) {
		header := archive[offset : offset+tarBlock]
		offset += tarBlock
		if allZero(header) {
			break
		}
		size, ok := parseTarSize(header[124:136])
		if !ok {
			break
		}
		typeFlag := header[156]
		end := offset + size
		if end > len(archive) {
			end = len(archive)
		}
		payload := archive[offset:end]
		offset += size + (tarBlock-size%tarBlock)%tarBlock
		if typeFlag == 0 || typeFlag == '0' {
			return append([]byte{}, payload...), nil
		}
	}
	return nil, errNoTarFile
}

func allZero(block []byte) bool {
	for _, b := range block {
		if b != 0 {
			return false
		}
	}
	return true
}

// parseTarSize mirrors Number.parseInt(field-up-to-NUL.trim(), 8) || 0: the
// leading octal digits count, anything else yields 0. A size that cannot fit an
// int, or a negative one, reports !ok (the TS loop would not terminate sanely).
func parseTarSize(field []byte) (int, bool) {
	text := string(field)
	if i := strings.IndexByte(text, 0); i >= 0 {
		text = text[:i]
	}
	text = strings.TrimSpace(text)
	size := 0
	for _, c := range []byte(text) {
		if c < '0' || c > '7' {
			break
		}
		size = size*8 + int(c-'0')
		if size > 1<<40 {
			return 0, false
		}
	}
	return size, true
}
