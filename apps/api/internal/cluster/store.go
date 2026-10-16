package cluster

import (
	"errors"
	"fmt"
	"io"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/secrets"
)

const (
	ClusterKeyPurpose         = "cluster-kek"
	ClusterKeyWrappingVersion = 1
)

var (
	ErrClusterKeyDenied = errors.New("cluster key denied")

	// Causes of an ImportInWrite denial. Each satisfies
	// errors.Is(err, ErrClusterKeyDenied) so existing callers keep working,
	// and carries no key material or row identifiers.
	ErrClusterKeyMaterialMismatch = clusterKeyDenial("active cluster key id holds different material")
	ErrClusterKeyUnwrapFailed     = clusterKeyDenial("stored cluster key cannot be unwrapped")
	ErrClusterKeyInsertFailed     = clusterKeyDenial("cluster key insert failed")

	clusterKeyRedacted = "[cluster key material redacted]"
)

// clusterKeyDenial is a cause-specific denial that still matches
// ErrClusterKeyDenied under errors.Is.
type clusterKeyDenial string

func (e clusterKeyDenial) Error() string { return "cluster key denied: " + string(e) }

func (e clusterKeyDenial) Is(target error) bool { return target == ErrClusterKeyDenied }

// KeyMaterial owns one plaintext cluster KEK and must be destroyed by its
// caller as soon as the bootstrap envelope has been sealed.
type KeyMaterial struct {
	ID  uuid.UUID
	key []byte
}

// NewKeyMaterial validates and copies a key for tests and secure adapters.
func NewKeyMaterial(id uuid.UUID, key []byte) (KeyMaterial, error) {
	if id == uuid.Nil || len(key) != KEKLength {
		return KeyMaterial{}, ErrClusterKeyDenied
	}
	return KeyMaterial{ID: id, key: append([]byte(nil), key...)}, nil
}

// CopyBytes returns one bounded copy of the plaintext key.
func (m KeyMaterial) CopyBytes() []byte {
	if len(m.key) != KEKLength {
		return nil
	}
	return append([]byte(nil), m.key...)
}

// Destroy zeroes the owned key bytes. It is safe to call repeatedly.
func (m *KeyMaterial) Destroy() {
	if m == nil {
		return
	}
	for i := range m.key {
		m.key[i] = 0
	}
	m.key = nil
}

func (m KeyMaterial) String() string { return clusterKeyRedacted }

func (m KeyMaterial) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, clusterKeyRedacted)
}

// Store unwraps cluster KEKs encrypted with the installation master key. The
// SQLite key store uses it for decryption; it performs no persistence.
type Store struct {
	masterKeyBase64 string
}

func NewStore(masterKeyBase64 string) *Store {
	return &Store{masterKeyBase64: masterKeyBase64}
}

func (s *Store) String() string { return "[cluster key store redacted]" }

func (s *Store) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, s.String())
}

func (s *Store) decrypt(id uuid.UUID, purpose, wrapped string, wrappingVersion int) (KeyMaterial, error) {
	if s == nil || purpose != ClusterKeyPurpose || wrappingVersion != ClusterKeyWrappingVersion || id == uuid.Nil {
		return KeyMaterial{}, ErrClusterKeyDenied
	}
	// The existing secret-store API returns plaintext as a string. Copy it once
	// into a byte buffer, then zero that buffer; Go cannot zero the immutable
	// string returned by that API, so the residual string lifetime is bounded
	// to this function and is an explicit boundary limitation.
	plaintext, err := secrets.DecryptSecret(wrapped, s.masterKeyBase64)
	if err != nil {
		return KeyMaterial{}, ErrClusterKeyDenied
	}
	key := []byte(plaintext)
	material, err := NewKeyMaterial(id, key)
	zeroBytes(key)
	if err != nil {
		return KeyMaterial{}, ErrClusterKeyDenied
	}
	return material, nil
}

func zeroBytes(value []byte) {
	for i := range value {
		value[i] = 0
	}
}
