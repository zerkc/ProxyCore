package dbimport

import (
	"bytes"
	"context"

	"github.com/zerkc/ProxyCore/apps/api/internal/backupcore/zipextract"
	"github.com/zerkc/ProxyCore/apps/api/internal/httpserver"
	"github.com/zerkc/ProxyCore/apps/api/internal/secrets"
)

// verifyMasterKey intentionally performs only a decryption check. It never
// returns the plaintext and never includes ciphertext or a passphrase in an
// error, log, report, or audit value.
func verifyMasterKey(ctx context.Context, files map[string]zipextract.File, masterKeyBase64 string) error {
	file, ok := files["db/secrets.json"]
	if !ok {
		return ErrNoSecretsInBundle
	}
	data, err := readArchiveFile(ctx, file)
	if err != nil {
		return httpserver.ErrMasterKeyMismatch
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return ErrNoSecretsInBundle
	}
	rows, err := decodeTableRows(data)
	if err != nil {
		return httpserver.ErrMasterKeyMismatch
	}
	if len(rows) == 0 {
		return ErrNoSecretsInBundle
	}
	ciphertext, ok := rows[0]["ciphertext"].(string)
	if !ok || ciphertext == "" {
		return httpserver.ErrMasterKeyMismatch
	}
	if _, err := secrets.DecryptSecret(ciphertext, masterKeyBase64); err != nil {
		return httpserver.ErrMasterKeyMismatch
	}
	return nil
}
