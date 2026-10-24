// Package store ports the worker's SQLite job store from
// packages/db/src/sqlite-foundation (job-claim, job-recovery, job-update,
// job-enqueue-if-not-active and the read projections). It is a library: the
// worker orchestrator composes it, and nothing in the API imports it yet.
package store

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
	"github.com/zerkc/ProxyCore/apps/api/internal/worker/jobs"
)

// Target is the apply-job target domain.
type Target string

const (
	TargetCoreDNS     Target = "coredns"
	TargetNginx       Target = "nginx"
	TargetCombined    Target = "combined"
	TargetCertificate Target = "certificate"
)

// Source is the persistence-source domain.
type Source string

const (
	SourceOrdinary Source = "ordinary"
	SourceImport   Source = "import"
	SourceSync     Source = "sync"
)

func validTarget(t Target) bool {
	switch t {
	case TargetCoreDNS, TargetNginx, TargetCombined, TargetCertificate:
		return true
	}
	return false
}

func validSource(s Source) bool {
	return s == SourceOrdinary || s == SourceImport || s == SourceSync
}

func validStatus(s jobs.Status) bool {
	switch s {
	case jobs.Queued, jobs.Validating, jobs.Applying, jobs.Applied, jobs.Failed, jobs.RolledBack:
		return true
	}
	return false
}

// Job mirrors the TypeScript JobRecord. Optional values are nil when the column
// is SQL NULL. Output documents keep the stored JSON bytes; a stored JSON null
// projects to nil, like the TS `JSON.parse(text) ?? undefined`. A tagged
// infinity projects to the zero time (TS yields an Invalid Date).
type Job struct {
	ID                   string
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
	CreatedAt            time.Time
	ClaimedAt            *time.Time
	StartedAt            *time.Time
	FinishedAt           *time.Time
	ValidationOutput     json.RawMessage
	ApplyOutput          json.RawMessage
	HealthOutput         json.RawMessage
	ErrorMessage         *string
}

// jobColumns is the explicit apply_jobs projection in schema order; the scan
// order in scanJob must match it.
const jobColumns = `id,revision_id,correlation_id,created_at,target,status,source,
	actor_user_id,source_primary_id,source_node_id,source_revision_id,snapshot_content_hash,
	validation_output,apply_output,health_output,error_message,claimed_at,started_at,finished_at,
	snapshot_version,replication_version,leadership_generation`

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

type rowScanner interface{ Scan(dest ...any) error }

// scanJob reads one jobColumns row and applies the same boundary validation as
// the TS native readers: canonical UUIDs, NUL-free text, decodable clocks,
// valid JSON outputs, known domains and in-range integers.
func scanJob(row rowScanner) (Job, error) {
	var (
		id, revisionID, correlationID, createdAt, target, status, source string
		actor, primary, node, sourceRevision, hash                       sql.NullString
		outputs                                                          [3]sql.NullString
		message, claimed, started, finished                              sql.NullString
		snapshotVersion, replicationVersion, leadership                  sql.NullInt64
	)
	if err := row.Scan(&id, &revisionID, &correlationID, &createdAt, &target, &status, &source,
		&actor, &primary, &node, &sourceRevision, &hash,
		&outputs[0], &outputs[1], &outputs[2], &message, &claimed, &started, &finished,
		&snapshotVersion, &replicationVersion, &leadership); err != nil {
		return Job{}, err
	}
	job := Job{
		ID: id, RevisionID: revisionID, CorrelationID: correlationID,
		Target: Target(target), Status: jobs.Status(status), Source: Source(source),
	}
	if !validTarget(job.Target) || !validStatus(job.Status) || !validSource(job.Source) {
		return Job{}, errors.New("Invalid SQLite job domain")
	}
	for name, value := range map[string]string{"id": id, "revision_id": revisionID} {
		if !uuidPattern.MatchString(value) {
			return Job{}, fmt.Errorf("Invalid job UUID %s", name)
		}
	}
	if strings.ContainsRune(correlationID, 0) {
		return Job{}, errors.New("Invalid job correlation_id")
	}
	var err error
	if job.CreatedAt, err = decodeTime("created_at", createdAt); err != nil {
		return Job{}, err
	}
	optionalIDs := []struct {
		name string
		in   sql.NullString
		out  **string
	}{
		{"actor_user_id", actor, &job.ActorUserID}, {"source_primary_id", primary, &job.SourcePrimaryID},
		{"source_node_id", node, &job.SourceNodeID}, {"source_revision_id", sourceRevision, &job.SourceRevisionID},
	}
	for _, f := range optionalIDs {
		if !f.in.Valid {
			continue
		}
		if !uuidPattern.MatchString(f.in.String) {
			return Job{}, fmt.Errorf("Invalid job UUID %s", f.name)
		}
		v := f.in.String
		*f.out = &v
	}
	for name, in := range map[string]sql.NullString{"snapshot_content_hash": hash, "error_message": message} {
		if in.Valid && strings.ContainsRune(in.String, 0) {
			return Job{}, fmt.Errorf("Invalid job %s", name)
		}
	}
	if hash.Valid {
		job.SnapshotContentHash = &hash.String
	}
	if message.Valid {
		job.ErrorMessage = &message.String
	}
	for i, target := range []*json.RawMessage{&job.ValidationOutput, &job.ApplyOutput, &job.HealthOutput} {
		if !outputs[i].Valid {
			continue
		}
		text := outputs[i].String
		if strings.ContainsRune(text, 0) || !json.Valid([]byte(text)) {
			return Job{}, fmt.Errorf("Invalid job %s", [...]string{"validation_output", "apply_output", "health_output"}[i])
		}
		if bytes.Equal(bytes.TrimSpace([]byte(text)), []byte("null")) {
			continue
		}
		*target = json.RawMessage(text)
	}
	for _, f := range []struct {
		name string
		in   sql.NullString
		out  **time.Time
	}{{"claimed_at", claimed, &job.ClaimedAt}, {"started_at", started, &job.StartedAt}, {"finished_at", finished, &job.FinishedAt}} {
		if !f.in.Valid {
			continue
		}
		at, err := decodeTime(f.name, f.in.String)
		if err != nil {
			return Job{}, err
		}
		*f.out = &at
	}
	for _, f := range []struct {
		name string
		in   sql.NullInt64
		out  **int32
	}{{"snapshot_version", snapshotVersion, &job.SnapshotVersion}, {"replication_version", replicationVersion, &job.ReplicationVersion}} {
		if !f.in.Valid {
			continue
		}
		if f.in.Int64 < math.MinInt32 || f.in.Int64 > math.MaxInt32 {
			return Job{}, fmt.Errorf("Invalid job integer %s", f.name)
		}
		v := int32(f.in.Int64)
		*f.out = &v
	}
	if leadership.Valid {
		job.LeadershipGeneration = &leadership.Int64
	}
	return job, nil
}

func decodeTime(name, key string) (time.Time, error) {
	if strings.ContainsRune(key, 0) {
		return time.Time{}, fmt.Errorf("Invalid job %s", name)
	}
	decoded, err := sqlitedb.DecodeTimestamp(&key)
	if err != nil {
		return time.Time{}, fmt.Errorf("Invalid job %s: %w", name, err)
	}
	if finite, ok := decoded.(sqlitedb.FiniteTimestamp); ok {
		return time.Unix(finite.Seconds, int64(finite.Nanos)).UTC(), nil
	}
	return time.Time{}, nil
}

func encodeTime(at time.Time) (string, error) {
	key, err := sqlitedb.EncodeTimestamp(sqlitedb.FiniteTimestamp{Seconds: at.Unix(), Nanos: int32(at.Nanosecond())})
	if err != nil {
		return "", err
	}
	return *key, nil
}

// compactOutput validates a JSON document and stores it compacted, the form
// JSON.stringify produces on the TS side.
func compactOutput(name string, raw json.RawMessage) (string, error) {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return "", fmt.Errorf("invalid %s JSON: %w", name, err)
	}
	return buf.String(), nil
}
