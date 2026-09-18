package configuration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zerkc/ProxyCore/apps/api/internal/snapshot"
)

// ApplyTerminalStatus is the small status projection needed by the NODE
// conversion waiter. The worker's error_message is projected as FailureCode
// without adding a second apply_jobs schema column.
type ApplyTerminalStatus string

const (
	ApplyTerminalStatusApplied    ApplyTerminalStatus = "applied"
	ApplyTerminalStatusFailed     ApplyTerminalStatus = "failed"
	ApplyTerminalStatusRolledBack ApplyTerminalStatus = "rolled-back"
)

// ApplyJobTerminal is a read-only projection of an apply job's terminal state.
type ApplyJobTerminal struct {
	Status      ApplyTerminalStatus
	FinishedAt  *time.Time
	FailureCode *string
}

// TerminalApplyReader reads the current durable state of one apply job.
type TerminalApplyReader interface {
	GetApplyJobTerminal(context.Context, uuid.UUID) (ApplyJobTerminal, error)
}

// ApplyWaitOptions controls the bounded terminal apply poller. The sync
// package aliases this lower-level contract so configuration stores can
// implement the reader without importing the sync package back.
type ApplyWaitOptions struct {
	PollInterval time.Duration
	Timeout      time.Duration
	Clock        func() time.Time
}

const (
	DefaultApplyWaitPollInterval = 250 * time.Millisecond
	DefaultApplyWaitTimeout      = 60 * time.Second
)

var (
	ErrApplyWaitTimeout         = errors.New("apply wait timed out")
	ErrApplyWaitJobIDRequired   = errors.New("apply wait job id is required")
	ErrApplyWaitStoreRequired   = errors.New("apply wait store is required")
	ErrApplyWaitContextRequired = errors.New("apply wait context is required")
)

// ApplyWaitTimeoutError preserves the last durable observation while
// remaining compatible with errors.Is(err, ErrApplyWaitTimeout).
type ApplyWaitTimeoutError struct {
	Last ApplyJobTerminal
}

func (e *ApplyWaitTimeoutError) Error() string {
	if e == nil {
		return ErrApplyWaitTimeout.Error()
	}
	return fmt.Sprintf("%s: last status %q", ErrApplyWaitTimeout, e.Last.Status)
}

func (e *ApplyWaitTimeoutError) Unwrap() error { return ErrApplyWaitTimeout }

// WaitForTerminalApply polls immediately, then waits between observations
// with a timer. It never sleeps or hides reader/context errors.
func WaitForTerminalApply(ctx context.Context, store TerminalApplyReader, jobID uuid.UUID, opts ApplyWaitOptions) (ApplyJobTerminal, error) {
	if ctx == nil {
		return ApplyJobTerminal{}, ErrApplyWaitContextRequired
	}
	if store == nil {
		return ApplyJobTerminal{}, ErrApplyWaitStoreRequired
	}
	if jobID == uuid.Nil {
		return ApplyJobTerminal{}, ErrApplyWaitJobIDRequired
	}
	pollInterval := opts.PollInterval
	if pollInterval <= 0 {
		pollInterval = DefaultApplyWaitPollInterval
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultApplyWaitTimeout
	}
	clock := opts.Clock
	if clock == nil {
		clock = time.Now
	}
	deadline := clock().Add(timeout)
	var last ApplyJobTerminal
	for {
		if err := ctx.Err(); err != nil {
			return last, err
		}
		observed, err := store.GetApplyJobTerminal(ctx, jobID)
		if err != nil {
			return last, err
		}
		last = observed
		if isTerminalApplyStatus(observed.Status) {
			return observed, nil
		}
		remaining := deadline.Sub(clock())
		if remaining <= 0 {
			return last, &ApplyWaitTimeoutError{Last: last}
		}
		delay := pollInterval
		if remaining < delay {
			delay = remaining
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if err := ctx.Err(); err != nil {
				timer.Stop()
				return last, err
			}
		case <-timer.C:
		}
		timer.Stop()
	}
}

func isTerminalApplyStatus(status ApplyTerminalStatus) bool {
	switch status {
	case ApplyTerminalStatusApplied, ApplyTerminalStatusFailed, ApplyTerminalStatusRolledBack:
		return true
	default:
		return false
	}
}

type importedSnapshotFields struct {
	sourcePrimaryID      uuid.UUID
	sourceNodeID         uuid.UUID
	snapshotHash         string
	contentHash          string
	snapshotVersion      int
	replicationVersion   int
	leadershipGeneration int64
}

// importedSnapshotMetadata parses only the fields required for persistence.
// Cryptographic and content validation remains the caller/validator boundary.
func importedSnapshotMetadata(snapshotBytes []byte) (importedSnapshotFields, error) {
	if len(snapshotBytes) == 0 {
		return importedSnapshotFields{}, errors.New("import snapshot is empty")
	}
	env, err := snapshot.Unmarshal(snapshotBytes)
	if err != nil {
		return importedSnapshotFields{}, err
	}
	if env.Transient.SourcePrimaryID == uuid.Nil || !env.NodeLocal.NodeID.IsValid() {
		return importedSnapshotFields{}, errors.New("import snapshot has invalid source identity")
	}
	if !env.Transient.SnapshotVersion.IsValid() || !env.Transient.ReplicationVersion.IsValid() || env.Transient.LeadershipGeneration == 0 {
		return importedSnapshotFields{}, errors.New("import snapshot has invalid transient metadata")
	}
	const maxInt64 = uint64(^uint64(0) >> 1)
	if uint64(env.Transient.LeadershipGeneration) > maxInt64 {
		return importedSnapshotFields{}, errors.New("import snapshot leadership generation exceeds PostgreSQL range")
	}
	if env.ContentHash == "" {
		return importedSnapshotFields{}, errors.New("import snapshot content hash is empty")
	}
	sourceNodeID, err := uuid.Parse(env.NodeLocal.NodeID.String())
	if err != nil {
		return importedSnapshotFields{}, fmt.Errorf("parse import source node id: %w", err)
	}
	checksum := sha256.Sum256(snapshotBytes)
	return importedSnapshotFields{
		sourcePrimaryID:      env.Transient.SourcePrimaryID,
		sourceNodeID:         sourceNodeID,
		snapshotHash:         hex.EncodeToString(checksum[:]),
		contentHash:          env.ContentHash,
		snapshotVersion:      int(env.Transient.SnapshotVersion),
		replicationVersion:   int(env.Transient.ReplicationVersion),
		leadershipGeneration: int64(env.Transient.LeadershipGeneration),
	}, nil
}

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
