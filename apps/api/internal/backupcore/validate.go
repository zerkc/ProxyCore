package backupcore

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"time"
)

func Validate(m Manifest) error {
	if len(m.unknownTopLevelKeys) > 0 {
		return fmt.Errorf("unknown top-level manifest key %q", m.unknownTopLevelKeys[0])
	}
	if len(m.duplicateTopLevelKeys) > 0 {
		return fmt.Errorf("duplicate top-level manifest key %q", m.duplicateTopLevelKeys[0])
	}
	if len(m.invalidTopLevelFields) > 0 {
		return fmt.Errorf("invalid top-level manifest field %q", m.invalidTopLevelFields[0])
	}
	if m.envPresentMissing {
		return fmt.Errorf("missing required manifest key %q", "envPresent")
	}
	if m.FormatVersion == "" {
		return fmt.Errorf("missing required manifest key %q", "formatVersion")
	}
	if m.FormatVersion != FormatVersion {
		return fmt.Errorf("unsupported manifest format version %q", m.FormatVersion)
	}
	if m.CreatedAt == "" {
		return fmt.Errorf("missing required manifest key %q", "createdAt")
	}
	createdAt, err := time.Parse(time.RFC3339, m.CreatedAt)
	if err != nil || createdAt.Location() != time.UTC {
		return fmt.Errorf("createdAt must be an RFC 3339 UTC timestamp")
	}
	if m.ExporterVersion == "" {
		return fmt.Errorf("missing required manifest key %q", "exporterVersion")
	}
	if m.InstallationID == "" {
		return fmt.Errorf("missing required manifest key %q", "installationId")
	}
	if m.NodeID == "" {
		return fmt.Errorf("missing required manifest key %q", "nodeId")
	}
	if m.Role == "" {
		return fmt.Errorf("missing required manifest key %q", "role")
	}
	if m.EntryChecksums == nil {
		return fmt.Errorf("missing required manifest key %q", "entryChecksums")
	}
	if m.DBTables == nil {
		return fmt.Errorf("missing required manifest key %q", "dbTables")
	}
	if m.Encryption != nil {
		if err := validateEncryption(m.Encryption); err != nil {
			return err
		}
	}
	for _, tablePath := range m.DBTables {
		if EscapesBundleRoot(tablePath) {
			return fmt.Errorf("db table path escapes bundle root: %q", tablePath)
		}
	}
	if len(m.duplicateChecksumPaths) > 0 {
		return fmt.Errorf("duplicate entry checksum path %q", m.duplicateChecksumPaths[0])
	}
	for entryPath, checksum := range m.EntryChecksums {
		if EscapesBundleRoot(entryPath) {
			return fmt.Errorf("entry checksum path escapes bundle root: %q", entryPath)
		}
		if len(checksum) != 64 {
			return fmt.Errorf("entry checksum for %q is not a SHA-256 hex digest", entryPath)
		}
		if _, err := hex.DecodeString(checksum); err != nil {
			return fmt.Errorf("entry checksum for %q is not a SHA-256 hex digest", entryPath)
		}
	}
	for _, certPath := range m.Certs {
		if EscapesBundleRoot(certPath) {
			return fmt.Errorf("certificate path escapes bundle root: %q", certPath)
		}
	}
	return nil
}

func validateEncryption(encryption *Encryption) error {
	if encryption == nil {
		return nil
	}
	if encryption.Cipher != CipherAES256GCM {
		return fmt.Errorf("encryption.cipher: must be %q", CipherAES256GCM)
	}
	if encryption.Kdf != KDFPBKDF2HMACSHA256 {
		return fmt.Errorf("encryption.kdf: must be %q", KDFPBKDF2HMACSHA256)
	}
	if encryption.Params.N <= 0 {
		return fmt.Errorf("encryption.params.n: must be positive")
	}
	if encryption.Params.R <= 0 {
		return fmt.Errorf("encryption.params.r: must be positive")
	}
	if encryption.Params.P <= 0 {
		return fmt.Errorf("encryption.params.p: must be positive")
	}
	if salt, err := decodeBase64URL(encryption.Salt); err != nil {
		return fmt.Errorf("encryption.salt: must be base64url-decodable: %w", err)
	} else if len(salt) != 16 {
		return fmt.Errorf("encryption.salt: must decode to 16 bytes")
	}
	if tag, err := decodeBase64URL(encryption.VerificationTag); err != nil {
		return fmt.Errorf("encryption.verificationTag: must be base64url-decodable: %w", err)
	} else if len(tag) != 16 {
		return fmt.Errorf("encryption.verificationTag: must decode to 16 bytes")
	}
	return nil
}

func decodeBase64URL(value string) ([]byte, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err == nil {
		return decoded, nil
	}
	return base64.URLEncoding.DecodeString(value)
}
