package configuration

import (
	"fmt"
	"strings"
	"testing"
)

func TestPhase2AcknowledgmentRequestRedactsPresentedCredential(t *testing.T) {
	request := SnapshotAcknowledgementRequest{PresentedCredential: "pcnode1_secret-material"}
	if strings.Contains(fmt.Sprintf("%+v", request), "secret-material") {
		t.Fatal("acknowledgement request formatter exposed presented credential")
	}
}
