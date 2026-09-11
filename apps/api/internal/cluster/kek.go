// Package cluster contains the cluster-key-encryption-key (KEK) envelope
// used by the primary-node continuity change. The cluster KEK is a
// 32-byte AES-256 key shared between the primary and its enrolled nodes
// during enrollment and used to wrap secrets that must be decryptable on a
// node after offline promotion.
//
// The envelope format is intentionally distinct from the local-master-key
// envelope used by the existing secrets package:
//
//	v1.kek:<iv-base64url>:<tag-base64url>:<ciphertext-base64url>
//
// A reader can therefore tell at a glance which wrapping key protects a
// given ciphertext and route to the right decryption path. The KEK itself
// is stored locally encrypted with the installation master key; this file
// only deals with the cluster-KEK envelope.
package cluster

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
)

// KEKLength is the required byte length of a cluster key-encryption key.
const KEKLength = 32

// envelopePrefix is the version prefix for cluster-KEK envelopes.
const envelopePrefix = "v1.kek"

// additionalData binds the envelope version to the ciphertext so a future
// envelope version cannot reuse a v1 ciphertext under a different prefix.
var additionalData = []byte(envelopePrefix)

// KEK wraps and unwraps data with a cluster key-encryption key.
type KEK struct {
	key []byte
}

// NewKEK constructs a KEK from a 32-byte key. Returns an error when the key
// length is invalid; callers should treat the returned error as fatal.
func NewKEK(key []byte) (*KEK, error) {
	if len(key) != KEKLength {
		return nil, fmt.Errorf("cluster KEK must be %d bytes, got %d", KEKLength, len(key))
	}
	return &KEK{key: append([]byte(nil), key...)}, nil
}

// GenerateKEK generates a fresh 32-byte cluster KEK using a
// cryptographically secure random source.
func GenerateKEK() ([]byte, error) {
	key := make([]byte, KEKLength)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, fmt.Errorf("generate cluster KEK: %w", err)
	}
	return key, nil
}

// Wrap encrypts plaintext with the cluster KEK and returns the envelope
// string "v1.kek:<iv>:<tag>:<ciphertext>" with base64url segments.
func (k *KEK) Wrap(plaintext []byte) (string, error) {
	block, err := aes.NewCipher(k.key)
	if err != nil {
		return "", fmt.Errorf("aes cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("gcm: %w", err)
	}
	iv := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, iv); err != nil {
		return "", fmt.Errorf("generate iv: %w", err)
	}
	sealed := gcm.Seal(nil, iv, plaintext, additionalData)
	tagOffset := len(sealed) - gcm.Overhead()
	ciphertext := sealed[:tagOffset]
	tag := sealed[tagOffset:]
	segments := []string{
		envelopePrefix,
		base64.RawURLEncoding.EncodeToString(iv),
		base64.RawURLEncoding.EncodeToString(tag),
		base64.RawURLEncoding.EncodeToString(ciphertext),
	}
	return strings.Join(segments, ":"), nil
}

// Unwrap decrypts a cluster-KEK envelope and returns the plaintext. It
// rejects envelopes that do not start with v1.kek, segments that do not
// decode, or ciphertexts that fail the GCM authentication tag.
func (k *KEK) Unwrap(envelope string) ([]byte, error) {
	segments := strings.Split(envelope, ":")
	if len(segments) != 4 || segments[0] != envelopePrefix {
		return nil, errors.New("not a cluster-KEK envelope")
	}
	iv, err := base64.RawURLEncoding.DecodeString(segments[1])
	if err != nil {
		return nil, fmt.Errorf("decode iv: %w", err)
	}
	tag, err := base64.RawURLEncoding.DecodeString(segments[2])
	if err != nil {
		return nil, fmt.Errorf("decode tag: %w", err)
	}
	ciphertext, err := base64.RawURLEncoding.DecodeString(segments[3])
	if err != nil {
		return nil, fmt.Errorf("decode ciphertext: %w", err)
	}
	block, err := aes.NewCipher(k.key)
	if err != nil {
		return nil, fmt.Errorf("aes cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("gcm: %w", err)
	}
	if len(iv) != gcm.NonceSize() {
		return nil, errors.New("invalid iv length")
	}
	if len(tag) != gcm.Overhead() {
		return nil, errors.New("invalid tag length")
	}
	sealed := make([]byte, 0, len(ciphertext)+len(tag))
	sealed = append(sealed, ciphertext...)
	sealed = append(sealed, tag...)
	return gcm.Open(nil, iv, sealed, additionalData)
}

// IsKEKEnvelope reports whether the given string is a cluster-KEK envelope
// produced by Wrap. It does not verify the envelope's contents or
// authenticate the ciphertext.
func IsKEKEnvelope(value string) bool {
	return strings.HasPrefix(value, envelopePrefix+":")
}
