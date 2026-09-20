package zipextract

import (
	"errors"
	"path"
	"strings"

	"github.com/zerkc/ProxyCore/apps/api/internal/backupcore"
)

var (
	ErrCorruptArchive         = errors.New("zipwriter: corrupt archive")
	ErrUnsupportedVersion     = errors.New("zipwriter: unsupported version")
	ErrUnknownCompression     = errors.New("zipwriter: unknown compression")
	ErrPassphraseRequired     = errors.New("zipwriter: passphrase required")
	ErrPassphraseMismatch     = errors.New("zipwriter: passphrase mismatch")
	ErrPathEscape             = errors.New("zipwriter: path escapes archive root")
	ErrFileTooLarge           = errors.New("zipwriter: file too large")
	ErrDuplicatePath          = errors.New("zipwriter: duplicate path")
	ErrEmptyPath              = errors.New("zipwriter: empty path")
	ErrAbsolutePath           = errors.New("zipwriter: absolute path")
	ErrDrivePath              = errors.New("zipwriter: drive-qualified path")
	ErrBackslashPath          = errors.New("zipwriter: backslash path")
	ErrAggregateLimitExceeded = errors.New("zipwriter: aggregate limit exceeded")
	ErrTrailerMismatch        = errors.New("zipwriter: trailing chunk mismatch")
)

// ValidatePath applies the one bundle-root policy shared by archive writers
// and readers, while preserving the more specific public sentinel for callers.
func ValidatePath(value string) error {
	if !backupcore.EscapesBundleRoot(value) {
		return nil
	}
	if value == "" {
		return ErrEmptyPath
	}
	if path.IsAbs(value) {
		return ErrAbsolutePath
	}
	if containsWindowsDrivePath(value) {
		return ErrDrivePath
	}
	if strings.Contains(value, `\`) {
		return ErrBackslashPath
	}
	return ErrPathEscape
}

func containsWindowsDrivePath(value string) bool {
	for index := 0; index+2 < len(value); index++ {
		if index > 0 && value[index-1] != '/' {
			continue
		}
		letter := value[index]
		if ((letter >= 'a' && letter <= 'z') || (letter >= 'A' && letter <= 'Z')) &&
			value[index+1] == ':' && (value[index+2] == '/' || value[index+2] == '\\') {
			return true
		}
	}
	return false
}
