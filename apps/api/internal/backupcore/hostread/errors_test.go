package hostread

import (
	"errors"
	"fmt"
	"testing"
)

func TestErrNoAppliedRevisionRoundTrip(t *testing.T) {
	wrapped := fmt.Errorf("lookup applied revision: %w", ErrNoAppliedRevision)
	if !errors.Is(wrapped, ErrNoAppliedRevision) {
		t.Fatalf("errors.Is(%v, ErrNoAppliedRevision) = false", wrapped)
	}
	if errors.Is(errors.New("hostread: no applied revision"), ErrNoAppliedRevision) {
		t.Fatal("a separately-created error unexpectedly matched ErrNoAppliedRevision")
	}
}
