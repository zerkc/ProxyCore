package dbimport

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/zerkc/ProxyCore/apps/api/internal/backupcore/zipextract"
	"github.com/zerkc/ProxyCore/apps/api/internal/httpserver"
	"github.com/zerkc/ProxyCore/apps/api/internal/secrets"
)

func TestVerifyMasterKeyUsesSentinelCiphertext(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, 32)
	masterKey := base64.StdEncoding.EncodeToString(key)
	wrongKey := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x24}, 32))
	distractor, err := secrets.EncryptSecret("secret-value", wrongKey)
	if err != nil {
		t.Fatalf("EncryptSecret(distractor): %v", err)
	}
	marker, err := secrets.EncryptSecret("proxycore-backup-master-key", masterKey)
	if err != nil {
		t.Fatalf("EncryptSecret(marker): %v", err)
	}

	for _, test := range []struct {
		name      string
		payload   string
		masterKey string
		want      error
	}{
		{
			name: "marker is not the first row",
			payload: `[{"id":"11111111-1111-4111-8111-111111111111","purpose":"ordinary","ciphertext":"` + distractor + `"},` +
				`{"id":"00000000-0000-0000-0000-000000000000","purpose":"__backup_master_key_marker__","ciphertext":"` + marker + `"}]`,
			masterKey: masterKey,
		},
		{
			name:      "wrong key maps to mismatch",
			payload:   `[{"id":"00000000-0000-0000-0000-000000000000","purpose":"__backup_master_key_marker__","ciphertext":"` + marker + `"}]`,
			masterKey: wrongKey,
			want:      httpserver.ErrMasterKeyMismatch,
		},
		{
			name:      "missing marker returns sentinel",
			payload:   `[{"purpose":"ordinary","ciphertext":"` + marker + `"}]`,
			masterKey: masterKey,
			want:      ErrNoSecretsInBundle,
		},
		{
			name:      "multiple markers fail closed",
			payload:   `[{"purpose":"__backup_master_key_marker__","ciphertext":"` + marker + `"},{"purpose":"__backup_master_key_marker__","ciphertext":"` + marker + `"}]`,
			masterKey: masterKey,
			want:      httpserver.ErrMasterKeyMismatch,
		},
		{
			name:      "malformed marker ciphertext maps to mismatch",
			payload:   `[{"purpose":"__backup_master_key_marker__","ciphertext":"not-a-ciphertext"}]`,
			masterKey: masterKey,
			want:      httpserver.ErrMasterKeyMismatch,
		},
		{
			name:      "empty secrets returns sentinel",
			payload:   `[]`,
			masterKey: masterKey,
			want:      ErrNoSecretsInBundle,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := verifyMasterKey(context.Background(), map[string]zipextract.File{
				"db/secrets.json": archiveFile("db/secrets.json", []byte(test.payload)),
			}, test.masterKey)
			if test.want == nil {
				if err != nil {
					t.Fatalf("verifyMasterKey() = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, test.want) {
				t.Fatalf("verifyMasterKey() = %v, want %v", err, test.want)
			}
		})
	}
}

func TestVerifyMasterKeyMissingSecretsFile(t *testing.T) {
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x42}, 32))
	if err := verifyMasterKey(context.Background(), map[string]zipextract.File{}, key); !errors.Is(err, ErrNoSecretsInBundle) {
		t.Fatalf("verifyMasterKey(missing file) = %v, want ErrNoSecretsInBundle", err)
	}
}
