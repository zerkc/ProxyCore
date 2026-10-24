package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
	"github.com/zerkc/ProxyCore/apps/api/internal/worker/jobs"
)

// ErrJobNotFound is returned by Update when no row has the given id.
var ErrJobNotFound = errors.New("job not found")

// recoveredMessage is the error_message recovery writes, matching the TS store.
const recoveredMessage = "Recovered after worker lease expiration"

// Store is the SQLite job store. Writes run in BEGIN IMMEDIATE transactions via
// sqlitedb.Writer, which serializes every writer on the file (broader than the
// PostgreSQL row locks; there is no SKIP LOCKED). The caller owns the *sql.DB.
type Store struct {
	db     *sql.DB
	writer *sqlitedb.Writer
	clock  func() time.Time
}

// Option customizes a Store.
type Option func(*Store)

// WithClock injects the time source used for claim stamps and the recovery
// cutoff. It is for tests; production uses time.Now.
func WithClock(clock func() time.Time) Option {
	return func(s *Store) { s.clock = clock }
}

// New builds a job store over a migrated database opened by sqlitedb.Open.
func New(db *sql.DB, opts ...Option) *Store {
	s := &Store{db: db, clock: time.Now}
	for _, opt := range opts {
		opt(s)
	}
	s.writer = sqlitedb.NewWriter(db, func() sqlitedb.FiniteTimestamp {
		now := s.clock()
		return sqlitedb.FiniteTimestamp{Seconds: now.Unix(), Nanos: int32(now.Nanosecond())}
	})
	return s
}

// Get returns the job with the literal id, or nil when there is none.
func (s *Store) Get(ctx context.Context, id string) (*Job, error) {
	job, err := scanJob(s.db.QueryRowContext(ctx, "SELECT "+jobColumns+" FROM main.apply_jobs WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &job, nil
}

// List returns every job, newest created_at first. Any invalid row fails the
// whole call; only an empty table yields an empty slice.
func (s *Store) List(ctx context.Context) ([]Job, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+jobColumns+" FROM main.apply_jobs ORDER BY created_at DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []Job{}
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, job)
	}
	return list, rows.Err()
}

// ClaimNext atomically moves the oldest queued job (optionally of one target;
// "" means any) to validating, stamping claimed_at and keeping an existing
// started_at. It returns nil when nothing is queued. The returned job is
// projected from the UPDATE ... RETURNING row after the transaction commits.
func (s *Store) ClaimNext(ctx context.Context, target Target) (*Job, error) {
	if target != "" && !validTarget(target) {
		return nil, errors.New("Invalid job target")
	}
	var claimed *Job
	err := s.writer.Write(ctx, func(v sqlitedb.WriteValue) error {
		query := "SELECT " + jobColumns + " FROM main.apply_jobs WHERE status = 'queued'"
		args := []any{}
		if target != "" {
			query += " AND target = ?"
			args = append(args, target)
		}
		selected, err := scanJob(v.Conn().QueryRowContext(v.Context(), query+" ORDER BY created_at ASC LIMIT 1", args...))
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := s.references(v, selected); err != nil {
			return err
		}
		job, err := scanJob(v.Conn().QueryRowContext(v.Context(), `UPDATE main.apply_jobs SET status = 'validating',
			claimed_at = ?, started_at = COALESCE(started_at, ?) WHERE id = ? AND status = 'queued'
			RETURNING `+jobColumns, v.Timestamp(), v.Timestamp(), selected.ID))
		if errors.Is(err, sql.ErrNoRows) {
			return errors.New("SQLite queued claim lost its selected row")
		}
		if err != nil {
			return err
		}
		if err := s.references(v, job); err != nil {
			return err
		}
		claimed = &job
		return nil
	})
	if err != nil {
		return nil, err
	}
	return claimed, nil
}

// references mirrors the TS defensive check that the selected row's revision
// (and actor, when set) still exist.
func (s *Store) references(v sqlitedb.WriteValue, job Job) error {
	missing := errors.New("Selected SQLite job has a missing foreign-key reference")
	var found string
	err := v.Conn().QueryRowContext(v.Context(), "SELECT id FROM main.config_revisions WHERE id = ?", job.RevisionID).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return missing
	}
	if err != nil {
		return err
	}
	if job.ActorUserID == nil {
		return nil
	}
	err = v.Conn().QueryRowContext(v.Context(), "SELECT id FROM main.users WHERE id = ?", *job.ActorUserID).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return missing
	}
	return err
}

// RecoverStale requeues validating/applying jobs whose claimed_at is older than
// leaseMs before now, clearing claimed_at and started_at, and returns how many
// rows changed. The cutoff is floored to milliseconds like the TS Date math.
func (s *Store) RecoverStale(ctx context.Context, leaseMs int64) (int64, error) {
	cutoffMs := s.clock().UnixMilli() - leaseMs
	remainder := ((cutoffMs % 1000) + 1000) % 1000
	cutoff, err := sqlitedb.EncodeTimestamp(sqlitedb.FiniteTimestamp{
		Seconds: (cutoffMs - remainder) / 1000, Nanos: int32(remainder) * 1_000_000,
	})
	if err != nil {
		return 0, err
	}
	var count int64
	err = s.writer.Write(ctx, func(v sqlitedb.WriteValue) error {
		result, err := v.Conn().ExecContext(v.Context(), `UPDATE main.apply_jobs SET status='queued',claimed_at=NULL,started_at=NULL,
			error_message=?
			WHERE status IN ('validating','applying') AND claimed_at IS NOT NULL AND claimed_at COLLATE BINARY < ?`,
			recoveredMessage, *cutoff)
		if err != nil {
			return err
		}
		count, err = result.RowsAffected()
		return err
	})
	return count, err
}

// Nullable is a tri-state patch value: the zero value leaves a column alone,
// Set writes a value and Clear writes SQL NULL.
type Nullable[T any] struct {
	set   bool
	value *T
}

// Set patches a column to value.
func Set[T any](value T) Nullable[T] { return Nullable[T]{set: true, value: &value} }

// Clear patches a nullable column to SQL NULL.
func Clear[T any]() Nullable[T] { return Nullable[T]{set: true} }

// JobPatch holds the eight columns the TS store lets callers update.
type JobPatch struct {
	Status           *jobs.Status
	ValidationOutput Nullable[json.RawMessage]
	ApplyOutput      Nullable[json.RawMessage]
	HealthOutput     Nullable[json.RawMessage]
	ErrorMessage     Nullable[string]
	ClaimedAt        Nullable[time.Time]
	StartedAt        Nullable[time.Time]
	FinishedAt       Nullable[time.Time]
}

type assignment struct {
	column string
	value  any
}

func (p JobPatch) assignments() ([]assignment, error) {
	var out []assignment
	if p.Status != nil {
		out = append(out, assignment{"status", string(*p.Status)})
	}
	for _, f := range []struct {
		column string
		patch  Nullable[json.RawMessage]
	}{{"validation_output", p.ValidationOutput}, {"apply_output", p.ApplyOutput}, {"health_output", p.HealthOutput}} {
		if !f.patch.set {
			continue
		}
		if f.patch.value == nil {
			out = append(out, assignment{f.column, nil})
			continue
		}
		text, err := compactOutput(f.column, *f.patch.value)
		if err != nil {
			return nil, err
		}
		out = append(out, assignment{f.column, text})
	}
	if p.ErrorMessage.set {
		if p.ErrorMessage.value == nil {
			out = append(out, assignment{"error_message", nil})
		} else {
			out = append(out, assignment{"error_message", *p.ErrorMessage.value})
		}
	}
	for _, f := range []struct {
		column string
		patch  Nullable[time.Time]
	}{{"claimed_at", p.ClaimedAt}, {"started_at", p.StartedAt}, {"finished_at", p.FinishedAt}} {
		if !f.patch.set {
			continue
		}
		if f.patch.value == nil {
			out = append(out, assignment{f.column, nil})
			continue
		}
		key, err := encodeTime(*f.patch.value)
		if err != nil {
			return nil, err
		}
		out = append(out, assignment{f.column, key})
	}
	return out, nil
}

// Update applies a patch and returns the updated job. When the patch sets a
// status, the move from the stored status must be allowed by jobs.Transition
// (the TS orchestrator enforces this before calling its store; the Go store
// enforces it itself, in the same transaction as the write).
func (s *Store) Update(ctx context.Context, id string, patch JobPatch) (Job, error) {
	assignments, err := patch.assignments()
	if err != nil {
		return Job{}, err
	}
	if len(assignments) == 0 {
		return Job{}, errors.New("No values to set")
	}
	set, args := "", make([]any, 0, len(assignments)+1)
	for i, a := range assignments {
		if i > 0 {
			set += ", "
		}
		set += a.column + " = ?"
		args = append(args, a.value)
	}
	args = append(args, id)
	var updated Job
	err = s.writer.Write(ctx, func(v sqlitedb.WriteValue) error {
		if patch.Status != nil {
			var current string
			err := v.Conn().QueryRowContext(v.Context(), "SELECT status FROM main.apply_jobs WHERE id = ?", id).Scan(&current)
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: %s", ErrJobNotFound, id)
			}
			if err != nil {
				return err
			}
			if _, err := jobs.Transition(jobs.Status(current), *patch.Status); err != nil {
				return err
			}
		}
		job, err := scanJob(v.Conn().QueryRowContext(v.Context(),
			"UPDATE main.apply_jobs SET "+set+" WHERE id = ? RETURNING "+jobColumns, args...))
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: %s", ErrJobNotFound, id)
		}
		if err != nil {
			return err
		}
		updated = job
		return nil
	})
	return updated, err
}
