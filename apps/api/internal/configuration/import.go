package configuration

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zerkc/ProxyCore/apps/api/internal/snapshot"
)

// CreateApplyJobFromSnapshot persists one imported revision and its queued
// combined apply job in the same transaction. The correlation id is shared by
// both rows so the worker and audit readers can follow one import operation.
func (s *Store) CreateApplyJobFromSnapshot(ctx context.Context, sourceLabel string, snapshotBytes []byte) (uuid.UUID, error) {
	return createApplyJobFromSnapshot(ctx, s.pool, sourceLabel, snapshotBytes)
}

// CreateApplyJobFromSnapshot implements the phase-2 store variant used by
// synchronization/runtime wiring.
func (s *PgPhase2Store) CreateApplyJobFromSnapshot(ctx context.Context, sourceLabel string, snapshotBytes []byte) (uuid.UUID, error) {
	return createApplyJobFromSnapshot(ctx, s.pool, sourceLabel, snapshotBytes)
}

func createApplyJobFromSnapshot(ctx context.Context, pool *pgxpool.Pool, sourceLabel string, snapshotBytes []byte) (uuid.UUID, error) {
	if ctx == nil {
		return uuid.Nil, errors.New("import apply context is required")
	}
	if pool == nil {
		return uuid.Nil, errors.New("import apply store is required")
	}
	if sourceLabel != string(PersistenceSourceImport) {
		return uuid.Nil, fmt.Errorf("unsupported import source %q", sourceLabel)
	}
	metadata, err := importedSnapshotMetadata(snapshotBytes)
	if err != nil {
		return uuid.Nil, fmt.Errorf("read imported snapshot metadata: %w", err)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock($1)`, revisionLockKey); err != nil {
		return uuid.Nil, err
	}
	var revisionNumber int
	if err := tx.QueryRow(ctx, `select coalesce(max(revision_number), 0) + 1 from config_revisions`).Scan(&revisionNumber); err != nil {
		return uuid.Nil, err
	}

	revisionID := uuid.New()
	correlationID := uuid.NewString()
	if _, err := tx.Exec(ctx, `
		insert into config_revisions (
			id, revision_number, checksum, snapshot, source, source_primary_id,
			source_node_id, source_revision_id, snapshot_content_hash,
			snapshot_version, replication_version, leadership_generation, applied_at
		) values ($1, $2, $3, $4, 'import', $5, $6, null, $7, $8, $9, $10, null)
	`, revisionID, revisionNumber, metadata.snapshotHash, snapshotBytes,
		metadata.sourcePrimaryID, metadata.sourceNodeID, metadata.contentHash,
		metadata.snapshotVersion, metadata.replicationVersion, metadata.leadershipGeneration); err != nil {
		return uuid.Nil, err
	}

	jobID := uuid.New()
	if _, err := tx.Exec(ctx, `
		insert into apply_jobs (
			id, revision_id, actor_user_id, target, status, source,
			source_primary_id, source_node_id, source_revision_id,
			snapshot_content_hash, snapshot_version, replication_version,
			leadership_generation, correlation_id
		) values ($1, $2, null, 'combined', 'queued', 'import', $3, $4, null, $5, $6, $7, $8, $9)
	`, jobID, revisionID, metadata.sourcePrimaryID, metadata.sourceNodeID,
		metadata.contentHash, metadata.snapshotVersion, metadata.replicationVersion,
		metadata.leadershipGeneration, correlationID); err != nil {
		return uuid.Nil, err
	}
	if _, err := tx.Exec(ctx, `select pg_notify($1, $2)`, jobChannel, jobID.String()); err != nil {
		return uuid.Nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, err
	}
	return jobID, nil
}

// SaveStandaloneArchive persists the import archive using the additive
// standalone_archives table. The importer owns the archive payload and TTL.
func (s *Store) SaveStandaloneArchive(ctx context.Context, archive snapshot.StandaloneArchive) error {
	return saveStandaloneArchive(ctx, s.pool, archive)
}

func (s *PgPhase2Store) SaveStandaloneArchive(ctx context.Context, archive snapshot.StandaloneArchive) error {
	return saveStandaloneArchive(ctx, s.pool, archive)
}

func saveStandaloneArchive(ctx context.Context, pool *pgxpool.Pool, archive snapshot.StandaloneArchive) error {
	if ctx == nil {
		return errors.New("archive context is required")
	}
	if pool == nil {
		return errors.New("archive store is required")
	}
	if archive.ID == uuid.Nil || archive.CapturedAt.IsZero() || archive.ExpiresAt.IsZero() || len(archive.SnapshotData) == 0 {
		return errors.New("archive is incomplete")
	}
	_, err := pool.Exec(ctx, `
		insert into standalone_archives (id, archive_blob, capture_reason, captured_at, expires_at)
		values ($1, $2, $3, $4, $5)
	`, archive.ID, string(archive.SnapshotData), archive.Reason, archive.CapturedAt, archive.ExpiresAt)
	return err
}

func (s *Store) PurgeExpiredArchives(ctx context.Context, now time.Time) (int, error) {
	return purgeExpiredArchives(ctx, s.pool, now)
}

func (s *PgPhase2Store) PurgeExpiredArchives(ctx context.Context, now time.Time) (int, error) {
	return purgeExpiredArchives(ctx, s.pool, now)
}

func purgeExpiredArchives(ctx context.Context, pool *pgxpool.Pool, now time.Time) (int, error) {
	if ctx == nil {
		return 0, errors.New("archive context is required")
	}
	if pool == nil {
		return 0, errors.New("archive store is required")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	result, err := pool.Exec(ctx, `delete from standalone_archives where expires_at is not null and expires_at < $1`, now)
	if err != nil {
		return 0, err
	}
	return int(result.RowsAffected()), nil
}

// GetApplyJobTerminal returns the latest durable worker status. Error message
// is intentionally exposed only as the existing failure-code projection.
func (s *Store) GetApplyJobTerminal(ctx context.Context, jobID uuid.UUID) (ApplyJobTerminal, error) {
	return getApplyJobTerminal(ctx, s.pool, jobID)
}

func (s *PgPhase2Store) GetApplyJobTerminal(ctx context.Context, jobID uuid.UUID) (ApplyJobTerminal, error) {
	return getApplyJobTerminal(ctx, s.pool, jobID)
}

func getApplyJobTerminal(ctx context.Context, q querier, jobID uuid.UUID) (ApplyJobTerminal, error) {
	if ctx == nil {
		return ApplyJobTerminal{}, errors.New("apply status context is required")
	}
	if q == nil {
		return ApplyJobTerminal{}, errors.New("apply status store is required")
	}
	if jobID == uuid.Nil {
		return ApplyJobTerminal{}, errors.New("apply status job id is required")
	}
	var status string
	var finishedAt *time.Time
	var failureCode *string
	if err := q.QueryRow(ctx, `
		select status::text, finished_at, error_message
		from apply_jobs where id = $1
	`, jobID).Scan(&status, &finishedAt, &failureCode); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ApplyJobTerminal{}, fmt.Errorf("apply job %s not found", jobID)
		}
		return ApplyJobTerminal{}, err
	}
	return ApplyJobTerminal{
		Status:      ApplyTerminalStatus(status),
		FinishedAt:  finishedAt,
		FailureCode: failureCode,
	}, nil
}
