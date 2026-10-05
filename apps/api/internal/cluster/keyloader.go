package cluster

import (
	"context"

	"github.com/google/uuid"
)

// KeyLoader is the storage-agnostic read boundary for one active cluster KEK.
// Unlike KeyStore it carries no database transaction type. Every failure
// (unknown, retired, unreadable or undecryptable key, canceled context) is
// reported as the bare ErrClusterKeyDenied, and the caller must Destroy the
// returned material.
type KeyLoader interface {
	LoadActive(ctx context.Context, id uuid.UUID) (KeyMaterial, error)
}

var _ KeyLoader = (*SQLiteKeyStore)(nil)
