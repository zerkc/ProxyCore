package configuration

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/google/uuid"
)

func TestClusterKeyMaterialOwnsAndRedactsPlaintext(t *testing.T) {
	key := bytes.Repeat([]byte{0x6c}, 32)
	material, err := NewClusterKeyMaterial("00112233-4455-4667-8899-aabbccddeeff", key)
	if err != nil {
		t.Fatalf("NewClusterKeyMaterial: %v", err)
	}
	if material.ID() != uuid.MustParse("00112233-4455-4667-8899-aabbccddeeff").String() {
		t.Fatalf("material ID = %q", material.ID())
	}
	if !bytes.Equal(material.CopyBytes(), key) {
		t.Fatal("material did not retain an independent key copy")
	}
	if rendered := fmt.Sprint(material); rendered == fmt.Sprintf("%x", key) {
		t.Fatalf("material rendered key plaintext: %q", rendered)
	}
	material.Destroy()
	if material.CopyBytes() != nil {
		t.Fatal("destroyed material returned key bytes")
	}
}
