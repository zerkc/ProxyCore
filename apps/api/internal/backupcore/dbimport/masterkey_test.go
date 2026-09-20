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

func TestVerifyMasterKeyDirectCases(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, 32)
	masterKey := base64.StdEncoding.EncodeToString(key)
	ciphertext, err := secrets.EncryptSecret("secret-value", masterKey)
	if err != nil {
		t.Fatalf("EncryptSecret: %v", err)
	}
	wrongKey := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x24}, 32))

	for _, test := range []struct {
		name      string
		files     map[string]zipextract.File
		masterKey string
		want      error
	}{
		{
			name: "known key decrypts first row",
			files: map[string]zipextract.File{
				"db/secrets.json": archiveFile("db/secrets.json", []byte(`[{"ciphertext":"`+ciphertext+`"}]`)),
			},
			masterKey: masterKey,
		},
		{
			name: "wrong key maps to mismatch",
			files: map[string]zipextract.File{
				"db/secrets.json": archiveFile("db/secrets.json", []byte(`[{"ciphertext":"`+ciphertext+`"}]`)),
			},
			masterKey: wrongKey,
			want:      httpserver.ErrMasterKeyMismatch,
		},
		{
			name: "malformed ciphertext maps to mismatch",
			files: map[string]zipextract.File{
				"db/secrets.json": archiveFile("db/secrets.json", []byte(`[{"ciphertext":"not-a-ciphertext"}]`)),
			},
			masterKey: masterKey,
			want:      httpserver.ErrMasterKeyMismatch,
		},
		{
			name:      "missing secrets returns sentinel",
			files:     map[string]zipextract.File{},
			masterKey: masterKey,
			want:      ErrNoSecretsInBundle,
		},
		{
			name: "empty secrets returns sentinel",
			files: map[string]zipextract.File{
				"db/secrets.json": archiveFile("db/secrets.json", []byte("[]")),
			},
			masterKey: masterKey,
			want:      ErrNoSecretsInBundle,
		},
		{
			name: "blank secrets payload returns sentinel",
			files: map[string]zipextract.File{
				"db/secrets.json": archiveFile("db/secrets.json", nil),
			},
			masterKey: masterKey,
			want:      ErrNoSecretsInBundle,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := verifyMasterKey(context.Background(), test.files, test.masterKey)
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
