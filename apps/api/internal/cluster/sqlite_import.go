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
// bytes succeeds without writing; a retired row with identical bytes is
// reactivated; a retired row with different or unreadable bytes is replaced;
// a different key under an active row with the same id is denied. Denials carry a
// cause-specific sentinel that still satisfies ErrClusterKeyDenied.
// The caller keeps ownership of key and must zero it.
func (s *SQLiteKeyStore) ImportInWrite(v sqlitedb.WriteValue, id uuid.UUID, key []byte) error {
	if v.Conn() == nil || !s.readable(v.Context()) || id == uuid.Nil || len(key) != KEKLength {
		return ErrClusterKeyDenied
	}
	existing, retired, found, err := s.loadByIDAnyState(v.Context(), v.Conn(), id)
	if found {
		trusted := err == nil
		if trusted {
			current := existing.CopyBytes()
			same := subtle.ConstantTimeCompare(current, key) == 1
			zeroBytes(current)
			existing.Destroy()
			switch {
			case same && !retired:
				return nil
			case same:
				// Same id and identical material: this installation already
				// held the primary's KEK and retired it. Reactivate it.
				if _, err := v.Conn().ExecContext(v.Context(),
					`update cluster_keys set retired_at = null where id = ? and retired_at is not null`, id.String()); err != nil {
					return ErrClusterKeyInsertFailed
				}
				return nil
			case !retired:
				return ErrClusterKeyMaterialMismatch
			}
		} else if !retired {
			return ErrClusterKeyUnwrapFailed
		}
		// A retired row that differs or cannot be unwrapped is replaced by the
		// primary's key. The delete shares the caller's write, so a failed
		// insert below rolls it back. Active rows are never deleted.
		if _, err := v.Conn().ExecContext(v.Context(),
			`delete from cluster_keys where id = ? and retired_at is not null`, id.String()); err != nil {
			return ErrClusterKeyInsertFailed
		}
	} else if err != nil {
		return ErrClusterKeyDenied
	}
	wrapped, err := secrets.EncryptSecret(string(key), s.master)
	if err != nil {
		return ErrClusterKeyDenied
	}
	if _, err := v.Conn().ExecContext(v.Context(), `insert into cluster_keys
		(id, purpose, wrapped_kek, wrapping_key_version, created_at) values (?, ?, ?, ?, ?)`,
		id.String(), ClusterKeyPurpose, wrapped, ClusterKeyWrappingVersion, v.Timestamp()); err != nil {
		return ErrClusterKeyInsertFailed
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
