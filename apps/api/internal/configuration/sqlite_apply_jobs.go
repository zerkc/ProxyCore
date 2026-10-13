package configuration

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// SQLiteApplyJobStore is the unwired SQLite counterpart of
// Store.GetApplyJobTerminal: a read-only projection of one apply job. It reads
// through the shared *sql.DB and never takes the writer.
type SQLiteApplyJobStore struct {
	db *sql.DB
}

func NewSQLiteApplyJobStore(db *sql.DB) *SQLiteApplyJobStore {
	return &SQLiteApplyJobStore{db: db}
}

var _ TerminalApplyReader = (*SQLiteApplyJobStore)(nil)

// GetApplyJobTerminal mirrors getApplyJobTerminal, including its validation
// errors and the "apply job <id> not found" text.
func (s *SQLiteApplyJobStore) GetApplyJobTerminal(ctx context.Context, jobID uuid.UUID) (ApplyJobTerminal, error) {
	if ctx == nil {
		return ApplyJobTerminal{}, errors.New("apply status context is required")
	}
	if s == nil || s.db == nil {
		return ApplyJobTerminal{}, errors.New("apply status store is required")
	}
	if jobID == uuid.Nil {
		return ApplyJobTerminal{}, errors.New("apply status job id is required")
	}
	var status string
	var finishedKey, failureCode *string
	err := s.db.QueryRowContext(ctx, `select status, finished_at, error_message from apply_jobs where id = ?`, jobID.String()).
		Scan(&status, &finishedKey, &failureCode)
	if errors.Is(err, sql.ErrNoRows) {
		return ApplyJobTerminal{}, fmt.Errorf("apply job %s not found", jobID)
	}
	if err != nil {
		return ApplyJobTerminal{}, err
	}
	finished, err := sqliteEnqueueTime(finishedKey)
	if err != nil {
		return ApplyJobTerminal{}, fmt.Errorf("apply job finished_at: %w", err)
	}
	return ApplyJobTerminal{Status: ApplyTerminalStatus(status), FinishedAt: finished, FailureCode: failureCode}, nil
}

// sqliteJobState is what waitForJob needs from one apply job.
type sqliteJobState struct {
	status       string
	errorMessage *string
}

// sqliteGetJobState mirrors Store.getJob for waitForJob: a missing job is
// (nil, nil) and a malformed id fails with PG's uuid text.
func sqliteGetJobState(ctx context.Context, db *sql.DB, id string) (*sqliteJobState, error) {
	canonical, err := sqliteCertificateLookupID(id)
	if err != nil {
		return nil, err
	}
	var job sqliteJobState
	err = db.QueryRowContext(ctx, `select status, error_message from apply_jobs where id = ?`, canonical).Scan(&job.status, &job.errorMessage)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &job, nil
}

// sqliteWaitForJob mirrors Store.waitForJob. No Write is held while polling.
// Intentional difference: the 250ms pause is context aware, so a canceled
// context returns promptly instead of after the sleep (PG reports the same
// context error from its next poll).
func sqliteWaitForJob(ctx context.Context, db *sql.DB, jobID string, timeout, poll time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		job, err := sqliteGetJobState(ctx, db, jobID)
		if err != nil {
			return err
		}
		if job == nil {
			return fmt.Errorf("Certificate apply job disappeared: %s", jobID)
		}
		switch job.status {
		case "applied":
			return nil
		case "failed", "rolled-back":
			message := job.status
			if job.errorMessage != nil {
				message = *job.errorMessage
			}
			return fmt.Errorf("HTTP-01 challenge route apply failed: %s", message)
		}
		timer := time.NewTimer(poll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return errors.New("Timed out waiting for the HTTP-01 challenge route")
}
