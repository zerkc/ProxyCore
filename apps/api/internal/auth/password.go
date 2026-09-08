package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/scrypt"
)

const (
	passwordN          = 16_384
	passwordR          = 8
	passwordP          = 1
	passwordKeyLength  = 64
	passwordSaltLength = 16
	minPasswordLength  = 5
	opaqueTokenBytes   = 32
	temporaryPasswordBytes = 24
)

func HashPassword(password string) (string, error) {
	if len(password) < minPasswordLength {
		return "", errors.New("password must contain at least 5 characters")
	}
	salt := make([]byte, passwordSaltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}
	derived, err := scrypt.Key([]byte(password), salt, passwordN, passwordR, passwordP, passwordKeyLength)
	if err != nil {
		return "", fmt.Errorf("derive password hash: %w", err)
	}
	return strings.Join([]string{
		"scrypt",
		strconv.Itoa(passwordN),
		strconv.Itoa(passwordR),
		strconv.Itoa(passwordP),
		base64.RawURLEncoding.EncodeToString(salt),
		base64.RawURLEncoding.EncodeToString(derived),
	}, "$"), nil
}

func VerifyPassword(password, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "scrypt" {
		return false
	}
	n, err := strconv.Atoi(parts[1])
	if err != nil {
		return false
	}
	r, err := strconv.Atoi(parts[2])
	if err != nil {
		return false
	}
	p, err := strconv.Atoi(parts[3])
	if err != nil {
		return false
	}
	salt, err := base64.RawURLEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	expected, err := base64.RawURLEncoding.DecodeString(parts[5])
	if err != nil {
		return false
	}
	derived, err := scrypt.Key([]byte(password), salt, n, r, p, len(expected))
	if err != nil {
		return false
	}
	return len(derived) == len(expected) && subtle.ConstantTimeCompare(derived, expected) == 1
}

// GenerateTemporaryPassword returns a high-entropy, URL-safe password suitable
// for copying from a terminal without transformation.
func GenerateTemporaryPassword() (string, error) {
	password := make([]byte, temporaryPasswordBytes)
	if _, err := rand.Read(password); err != nil {
		return "", fmt.Errorf("generate temporary password: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(password), nil
}

func CreateOpaqueToken() (string, error) {
	token := make([]byte, opaqueTokenBytes)
	if _, err := rand.Read(token); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(token), nil
}

func HashOpaqueToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
