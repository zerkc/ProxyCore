package httpexport

import (
	"errors"
	"testing"
)

func TestErrBodyTooLargeIsStableSentinel(t *testing.T) {
	if !errors.Is(ErrBodyTooLarge, ErrBodyTooLarge) {
		t.Fatal("ErrBodyTooLarge is not self-identifying")
	}
	if ErrBodyTooLarge.Error() != "httpexport: body exceeds configured limit" {
		t.Fatalf("ErrBodyTooLarge = %q", ErrBodyTooLarge)
	}
}
