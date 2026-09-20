package dbimport

import (
	"errors"
	"fmt"
	"testing"
)

func TestSentinelErrorsRoundTripThroughWrapping(t *testing.T) {
	for _, sentinel := range []error{
		ErrChecksumMismatch,
		ErrImportAlreadyInProgress,
		ErrInvalidEnvMode,
	} {
		t.Run(sentinel.Error(), func(t *testing.T) {
			wrapped := fmt.Errorf("import failed: %w", sentinel)
			if !errors.Is(wrapped, sentinel) {
				t.Fatalf("errors.Is(%v, %v) = false", wrapped, sentinel)
			}
		})
	}
}
