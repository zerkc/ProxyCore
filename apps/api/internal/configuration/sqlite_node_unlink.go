package configuration

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

var (
	// ErrEnrolledNodeNotFound reports an unlink for a node this primary never enrolled.
	ErrEnrolledNodeNotFound = errors.New("enrolled node not found")
	// ErrNodeUnlinkStore reports a store failure; callers must treat it as unavailable.
	ErrNodeUnlinkStore = errors.New("node unlink store unavailable")
)

// UnlinkEnrolledNode revokes one enrolled node's credential and marks the node
// row as explicitly unlinked in a single write, so sync endpoints can answer
// the node with an authoritative NODE_UNLINKED signal. It is idempotent: an
// already revoked or unlinked row keeps its original timestamps and the
// effective unlink time is returned.
func (s *SQLitePhase2Store) UnlinkEnrolledNode(ctx context.Context, nodeID string, now time.Time) (time.Time, error) {
	if ctx == nil || s == nil || s.writer == nil {
		return time.Time{}, ErrNodeUnlinkStore
	}
	canonical, err := sqlitedb.ParseUUIDLookupText(nodeID)
	if err != nil {
		return time.Time{}, ErrEnrolledNodeNotFound
	}
	stamp, err := sqliteEnrollmentTokenTime(now)
	if err != nil {
		return time.Time{}, ErrNodeUnlinkStore
	}
	var unlinked time.Time
	err = s.writer.Write(ctx, func(v sqlitedb.WriteValue) error {
		var exists int
		if err := v.Conn().QueryRowContext(v.Context(), `select 1 from enrolled_nodes where node_id = ?`, canonical).Scan(&exists); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrEnrolledNodeNotFound
			}
			return ErrNodeUnlinkStore
		}
		if _, err := v.Conn().ExecContext(v.Context(), `update node_credentials set revoked_at = coalesce(revoked_at, ?) where node_id = ?`, stamp, canonical); err != nil {
			return ErrNodeUnlinkStore
		}
		if _, err := v.Conn().ExecContext(v.Context(), `update enrolled_nodes
			set revoked_at = coalesce(revoked_at, ?), unlinked_at = coalesce(unlinked_at, ?) where node_id = ?`, stamp, stamp, canonical); err != nil {
			return ErrNodeUnlinkStore
		}
		var effective string
		if err := v.Conn().QueryRowContext(v.Context(), `select unlinked_at from enrolled_nodes where node_id = ?`, canonical).Scan(&effective); err != nil {
			return ErrNodeUnlinkStore
		}
		at, err := sqliteEnqueueTime(&effective)
		if err != nil || at == nil {
			return ErrNodeUnlinkStore
		}
		unlinked = *at
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrEnrolledNodeNotFound) {
			return time.Time{}, ErrEnrolledNodeNotFound
		}
		return time.Time{}, ErrNodeUnlinkStore
	}
	return unlinked, nil
}
