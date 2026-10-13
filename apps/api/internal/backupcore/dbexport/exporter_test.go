package dbexport

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/secrets"
)

func TestMasterKeyMarkerRoundTripsThroughSecretCipher(t *testing.T) {
	masterKey := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x42}, 32))
	fixed := func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) }
	row, err := masterKeyMarker(fixed, masterKey)
	if err != nil {
		t.Fatalf("masterKeyMarker: %v", err)
	}
	raw, err := json.Marshal(row)
	if err != nil {
		t.Fatalf("marshal marker: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode marker: %v", err)
	}
	if decoded["id"] != "00000000-0000-0000-0000-000000000000" {
		t.Fatalf("marker id = %v", decoded["id"])
	}
	if decoded["purpose"] != "__backup_master_key_marker__" {
		t.Fatalf("marker purpose = %v", decoded["purpose"])
	}
	ciphertext, ok := decoded["ciphertext"].(string)
	if !ok || ciphertext == "" {
		t.Fatalf("marker ciphertext = %v", decoded["ciphertext"])
	}
	plaintext, err := secrets.DecryptSecret(ciphertext, masterKey)
	if err != nil {
		t.Fatalf("decrypt marker: %v", err)
	}
	if plaintext != "proxycore-backup-master-key" {
		t.Fatalf("marker plaintext = %q", plaintext)
	}
}
