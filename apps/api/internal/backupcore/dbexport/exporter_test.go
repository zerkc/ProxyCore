package dbexport

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/zerkc/ProxyCore/apps/api/internal/secrets"
)

func TestExporterPrependsMasterKeyMarker(t *testing.T) {
	pool := openExporterTestPool(t)
	masterKey := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x42}, 32))
	writer := newMemWriter()

	_, err := New(pool, ExporterOptions{
		Tables:          []string{"secrets"},
		MasterKeyBase64: masterKey,
	}).Export(context.Background(), writer)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}

	var rows []map[string]any
	if err := json.Unmarshal(writer.files["db/secrets.json"], &rows); err != nil {
		t.Fatalf("decode secrets export: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("secrets export is empty")
	}
	marker := rows[0]
	if marker["id"] != "00000000-0000-0000-0000-000000000000" {
		t.Fatalf("marker id = %v", marker["id"])
	}
	if marker["purpose"] != "__backup_master_key_marker__" {
		t.Fatalf("marker purpose = %v", marker["purpose"])
	}
	ciphertext, ok := marker["ciphertext"].(string)
	if !ok || ciphertext == "" {
		t.Fatalf("marker ciphertext = %v", marker["ciphertext"])
	}
	plaintext, err := secrets.DecryptSecret(ciphertext, masterKey)
	if err != nil {
		t.Fatalf("decrypt marker: %v", err)
	}
	if plaintext != "proxycore-backup-master-key" {
		t.Fatalf("marker plaintext = %q", plaintext)
	}
}
