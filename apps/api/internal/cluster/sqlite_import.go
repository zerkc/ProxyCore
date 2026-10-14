package cluster

import (
	"crypto/subtle"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/secrets"
	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

// ImportInWrite persists a cluster KEK received from a primary under the
// primary-assigned id, wrapped with this installation's master key exactly as
// NewKeyCandidate does, so LocalKEKLoader/LoadActive can read it back. It runs
// inside the caller's Write and therefore commits or rolls back with the
// caller's other rows.
//
// It is idempotent: an existing active row with the same id and identical key
// bytes succeeds without writing; a different key under the same id is denied.
// The caller keeps ownership of key and must zero it.
func (s *SQLiteKeyStore) ImportInWrite(v sqlitedb.WriteValue, id uuid.UUID, key []byte) error {
	if v.Conn() == nil || !s.readable(v.Context()) || id == uuid.Nil || len(key) != KEKLength {
		return ErrClusterKeyDenied
	}
	existing, err := s.loadByID(v.Context(), v.Conn(), id)
	if err == nil {
		defer existing.Destroy()
		current := existing.CopyBytes()
		defer zeroBytes(current)
		if subtle.ConstantTimeCompare(current, key) == 1 {
			return nil
		}
		return ErrClusterKeyDenied
	}
	wrapped, err := secrets.EncryptSecret(string(key), s.master)
	if err != nil {
		return ErrClusterKeyDenied
	}
	if _, err := v.Conn().ExecContext(v.Context(), `insert into cluster_keys
		(id, purpose, wrapped_kek, wrapping_key_version, created_at) values (?, ?, ?, ?, ?)`,
		id.String(), ClusterKeyPurpose, wrapped, ClusterKeyWrappingVersion, v.Timestamp()); err != nil {
		return ErrClusterKeyDenied
	}
	return nil
}

// LoadActiveInWrite reads the unretired KEK with the given id on the caller's
// writer connection, so a KEK imported earlier in the same Write is visible.
// Every failure maps to ErrClusterKeyDenied. The caller must Destroy the result.
func (s *SQLiteKeyStore) LoadActiveInWrite(v sqlitedb.WriteValue, id uuid.UUID) (KeyMaterial, error) {
	if v.Conn() == nil || !s.readable(v.Context()) || id == uuid.Nil {
		return KeyMaterial{}, ErrClusterKeyDenied
	}
	return s.loadByID(v.Context(), v.Conn(), id)
}
