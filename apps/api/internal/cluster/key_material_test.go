package cluster

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/google/uuid"
)

func TestKeyMaterialRedactsAndDestroys(t *testing.T) {
	canary := bytes.Repeat([]byte{0x7a}, KEKLength)
	defer zeroClusterTestBytes(canary)
	material, err := NewKeyMaterial(uuid.MustParse("00112233-4455-4667-8899-aabbccddeeff"), canary)
	if err != nil {
		t.Fatalf("NewKeyMaterial: %v", err)
	}
	if got := material.CopyBytes(); !bytes.Equal(got, canary) {
		t.Fatalf("key copy mismatch: %x", got)
	}
	if rendered := fmt.Sprint(material); rendered == string(canary) || rendered == fmt.Sprintf("%x", canary) {
		t.Fatalf("key material rendered plaintext: %q", rendered)
	}
	material.Destroy()
	if got := material.CopyBytes(); got != nil {
		t.Fatalf("destroyed key copied %x", got)
	}
}
