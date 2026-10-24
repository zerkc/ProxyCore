package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
	"github.com/zerkc/ProxyCore/apps/api/internal/worker/jobs"
)

// EnqueueInput mirrors the TS JobEnqueueInput. Status defaults to queued and
// Source to ordinary; created_at is the transaction clock.
type EnqueueInput struct {
	RevisionID           string
	ActorUserID          *string
	Target               Target
	Status               jobs.Status
	Source               Source
	SourcePrimaryID      *string
	SourceNodeID         *string
	SourceRevisionID     *string
	SnapshotContentHash  *string
	SnapshotVersion      *int32
	ReplicationVersion   *int32
	LeadershipGeneration *int64
	CorrelationID        string
	ValidationOutput     json.RawMessage
	ApplyOutput          json.RawMessage
	HealthOutput         json.RawMessage
	ErrorMessage         *string
	ClaimedAt            *time.Time
	StartedAt            *time.Time
	FinishedAt           *time.Time
}

// Enqueue inserts a job and returns it.
func (s *Store) Enqueue(ctx context.Context, input EnqueueInput) (Job, error) {
	var inserted Job
	err := s.writer.Write(ctx, func(v sqlitedb.WriteValue) (err error) {
		inserted, err = insertJob(v, input)
		return err
	})
	return inserted, err
}

// EnqueueIfNotActive inserts a job only when no job anywhere is queued,
// validating or applying, and returns nil otherwise. The guard and the insert
// share one BEGIN IMMEDIATE transaction, so concurrent callers insert at most
// one job between them.
func (s *Store) EnqueueIfNotActive(ctx context.Context, input EnqueueInput) (*Job, error) {
	var inserted *Job
	err := s.writer.Write(ctx, func(v sqlitedb.WriteValue) error {
		var active string
		err := v.Conn().QueryRowContext(v.Context(),
			"SELECT id FROM main.apply_jobs WHERE status IN ('queued','validating','applying') LIMIT 1").Scan(&active)
		if err == nil {
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		job, err := insertJob(v, input)
		if err != nil {
			return err
		}
		inserted = &job
		return nil
	})
	if err != nil {
		return nil, err
	}
	return inserted, nil
}

func insertJob(v sqlitedb.WriteValue, in EnqueueInput) (Job, error) {
	id, err := v.NewUUID()
	if err != nil {
		return Job{}, err
	}
	status, source := in.Status, in.Source
	if status == "" {
		status = jobs.Queued
	}
	if source == "" {
		source = SourceOrdinary
	}
	outputs := make([]any, 3)
	for i, raw := range []json.RawMessage{in.ValidationOutput, in.ApplyOutput, in.HealthOutput} {
		if raw == nil {
			continue
		}
		if outputs[i], err = compactOutput("output", raw); err != nil {
			return Job{}, err
		}
	}
	times := make([]any, 3)
	for i, at := range []*time.Time{in.ClaimedAt, in.StartedAt, in.FinishedAt} {
		if at == nil {
			continue
		}
		if times[i], err = encodeTime(*at); err != nil {
			return Job{}, err
		}
	}
	return scanJob(v.Conn().QueryRowContext(v.Context(), `INSERT INTO main.apply_jobs
		(id,revision_id,actor_user_id,target,status,source,source_primary_id,source_node_id,source_revision_id,
		 snapshot_content_hash,snapshot_version,replication_version,leadership_generation,correlation_id,
		 validation_output,apply_output,health_output,error_message,claimed_at,started_at,finished_at,created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) RETURNING `+jobColumns,
		id, in.RevisionID, in.ActorUserID, string(in.Target), string(status), string(source),
		in.SourcePrimaryID, in.SourceNodeID, in.SourceRevisionID, in.SnapshotContentHash,
		in.SnapshotVersion, in.ReplicationVersion, in.LeadershipGeneration, in.CorrelationID,
		outputs[0], outputs[1], outputs[2], in.ErrorMessage, times[0], times[1], times[2], v.Timestamp()))
}
