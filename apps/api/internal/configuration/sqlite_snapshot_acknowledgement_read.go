package configuration

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"
)

// SQLiteSnapshotAcknowledgementReadStore reads stored tuples without owning
// the database lifecycle, authenticating callers, or implementing writes.
type SQLiteSnapshotAcknowledgementReadStore struct{ db *sql.DB }

func NewSQLiteSnapshotAcknowledgementReadStore(db *sql.DB) *SQLiteSnapshotAcknowledgementReadStore {
	return &SQLiteSnapshotAcknowledgementReadStore{db: db}
}

func (s *SQLiteSnapshotAcknowledgementReadStore) GetSnapshotAcknowledgement(ctx context.Context, nodeID, contentHash string) (SnapshotAcknowledgement, error) {
	if ctx == nil || !validSnapshotAcknowledgementInput(nodeID, contentHash, time.Unix(1, 0)) {
		return SnapshotAcknowledgement{}, ErrSnapshotAcknowledgementDenied
	}
	if s == nil || s.db == nil {
		return SnapshotAcknowledgement{}, ErrSnapshotAcknowledgementUnavailable
	}
	// Validation preserves the existing canonical-only UUID input contract.
	node, _ := uuid.Parse(nodeID)
	var ack SnapshotAcknowledgement
	var applied, received string
	err := s.db.QueryRowContext(ctx, `
  select node_id, content_hash, snapshot_version, replication_version,
   revision_id, leadership_generation, applied_at, received_at
  from node_snapshot_acks where node_id = ? and content_hash = ?
 `, node.String(), contentHash).Scan(&ack.NodeID, &ack.ContentHash,
		&ack.SnapshotVersion, &ack.ReplicationVersion, &ack.RevisionID,
		&ack.LeadershipGeneration, &applied, &received)
	if errors.Is(err, sql.ErrNoRows) {
		return SnapshotAcknowledgement{}, ErrSnapshotAcknowledgementNotFound
	}
	if err != nil {
		return SnapshotAcknowledgement{}, ErrSnapshotAcknowledgementUnavailable
	}
	// Public time.Time values require finite, representable clocks. This does
	// not claim PostgreSQL infinity parity or enforce acknowledgement write rules.
	appliedAt, err := sqliteEnqueueTime(&applied)
	if err != nil || appliedAt == nil {
		return SnapshotAcknowledgement{}, ErrSnapshotAcknowledgementUnavailable
	}
	receivedAt, err := sqliteEnqueueTime(&received)
	if err != nil || receivedAt == nil {
		return SnapshotAcknowledgement{}, ErrSnapshotAcknowledgementUnavailable
	}
	ack.AppliedAt, ack.ReceivedAt = *appliedAt, *receivedAt
	return ack, nil
}
