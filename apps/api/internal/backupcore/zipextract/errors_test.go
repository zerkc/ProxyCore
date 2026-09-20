package zipextract

import (
	"errors"
	"fmt"
	"testing"
)

func TestSentinelsSupportErrorsIs(t *testing.T) {
	sentinels := []error{
		ErrCorruptArchive,
		ErrUnsupportedVersion,
		ErrUnknownCompression,
		ErrPassphraseRequired,
		ErrPassphraseMismatch,
		ErrPathEscape,
		ErrFileTooLarge,
		ErrDuplicatePath,
		ErrEmptyPath,
		ErrAbsolutePath,
		ErrDrivePath,
		ErrBackslashPath,
		ErrAggregateLimitExceeded,
		ErrTrailerMismatch,
	}

	for _, sentinel := range sentinels {
		t.Run(sentinel.Error(), func(t *testing.T) {
			wrapped := fmt.Errorf("outer: %w", sentinel)
			if !errors.Is(wrapped, sentinel) {
				t.Fatalf("errors.Is(%v, %v) = false", wrapped, sentinel)
			}
		})
	}
}
