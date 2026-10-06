package cluster

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type pgKeyTxBeginner interface {
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
}

type pgKeyLoadStore interface {
	LoadOrCreate(ctx context.Context, tx SQLTx, activeID *uuid.UUID) (KeyMaterial, error)
}

// PGKeyLoader adapts the PostgreSQL Store to the tx-agnostic KeyLoader. It owns
// one short transaction per call: begin, load the requested active key, verify
// the id, commit; the deferred rollback releases the connection on every
// failure path. Every failure is reported as the bare ErrClusterKeyDenied.
type PGKeyLoader struct {
	begin pgKeyTxBeginner
	store pgKeyLoadStore
}

// NewPGKeyLoader returns a KeyLoader over pool and store. A nil pool or store
// yields a loader that denies every request.
func NewPGKeyLoader(pool *pgxpool.Pool, store *Store) *PGKeyLoader {
	l := &PGKeyLoader{}
	if pool != nil {
		l.begin = pool
	}
	if store != nil {
		l.store = store
	}
	return l
}

func (l *PGKeyLoader) LoadActive(ctx context.Context, id uuid.UUID) (KeyMaterial, error) {
	if l == nil || ctx == nil || l.begin == nil || l.store == nil {
		return KeyMaterial{}, ErrClusterKeyDenied
	}
	tx, err := l.begin.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return KeyMaterial{}, ErrClusterKeyDenied
	}
	defer tx.Rollback(ctx)

	material, err := l.store.LoadOrCreate(ctx, tx, &id)
	if err != nil || material.ID != id {
		material.Destroy()
		return KeyMaterial{}, ErrClusterKeyDenied
	}
	if err := tx.Commit(ctx); err != nil {
		material.Destroy()
		return KeyMaterial{}, ErrClusterKeyDenied
	}
	return material, nil
}

var _ KeyLoader = (*PGKeyLoader)(nil)
