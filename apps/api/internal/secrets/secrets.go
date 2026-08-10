package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"regexp"
	"strings"
)

const masterKeyLength = 32

// DecodeMasterKey validates a base64 master key decodes to 32 bytes.
func DecodeMasterKey(masterKeyBase64 string) ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(masterKeyBase64))
	if err != nil {
		return nil, errors.New("PROXYCORE_MASTER_KEY_BASE64 must decode to 32 bytes")
	}
	if len(key) != masterKeyLength {
		return nil, errors.New("PROXYCORE_MASTER_KEY_BASE64 must decode to 32 bytes")
	}
	return key, nil
}

// EncryptSecret mirrors the Node crypto.encryptSecret AES-256-GCM format:
// v1:base64url(iv):base64url(tag):base64url(ciphertext).
func EncryptSecret(plaintext, masterKeyBase64 string) (string, error) {
	key, err := DecodeMasterKey(masterKeyBase64)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	iv := make([]byte, 12)
	if _, err := rand.Read(iv); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nil, iv, []byte(plaintext), nil)
	tagStart := len(sealed) - gcm.Overhead()
	ciphertext := sealed[:tagStart]
	tag := sealed[tagStart:]
	return strings.Join([]string{
		"v1",
		base64.RawURLEncoding.EncodeToString(iv),
		base64.RawURLEncoding.EncodeToString(tag),
		base64.RawURLEncoding.EncodeToString(ciphertext),
	}, ":"), nil
}

// DecryptSecret reverses EncryptSecret.
func DecryptSecret(encoded, masterKeyBase64 string) (string, error) {
	parts := strings.Split(encoded, ":")
	if len(parts) != 4 || parts[0] != "v1" || parts[1] == "" || parts[2] == "" || parts[3] == "" {
		return "", errors.New("Unsupported encrypted secret format")
	}
	key, err := DecodeMasterKey(masterKeyBase64)
	if err != nil {
		return "", err
	}
	iv, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", err
	}
	tag, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return "", err
	}
	ciphertext, err := base64.RawURLEncoding.DecodeString(parts[3])
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	plaintext, err := gcm.Open(nil, iv, append(ciphertext, tag...), nil)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}

// HashBasicAuthPassword produces an Nginx {SHA} htpasswd hash.
func HashBasicAuthPassword(password string) (string, error) {
	if len(password) < 8 {
		return "", errors.New("Basic Auth password must contain at least 8 characters")
	}
	if strings.ContainsAny(password, "\r\n") {
		return "", errors.New("Basic Auth password cannot contain newlines")
	}
	sum := sha1.Sum([]byte(password))
	return "{SHA}" + base64.StdEncoding.EncodeToString(sum[:]), nil
}

var redactPattern = regexp.MustCompile(`(?i)pass(word)?|secret|token|private.?key|ciphertext|authorization`)

// RedactSecrets replaces sensitive-looking keys with [REDACTED].
func RedactSecrets(value any) any {
	switch v := value.(type) {
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = RedactSecrets(item)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			if redactPattern.MatchString(key) {
				out[key] = "[REDACTED]"
			} else {
				out[key] = RedactSecrets(item)
			}
		}
		return out
	default:
		return value
	}
}
