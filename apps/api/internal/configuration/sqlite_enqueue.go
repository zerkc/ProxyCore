package configuration

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

// sqliteCreateApplyJobInTransaction only persists a queued job. The caller owns
// commit/rollback and future worker notification. Writer's BEGIN IMMEDIATE
// serializes all writers, more broadly than PostgreSQL's advisory/row locks.
func sqliteCreateApplyJobInTransaction(v sqlitedb.WriteValue, actorUserID string) (ApplyResult, error) {
	snapshot, err := (&SQLiteSnapshotStore{}).snapshot(v)
	if err != nil {
		return ApplyResult{}, err
	}
	if snapshot.Settings.DefaultPool == nil {
		return ApplyResult{}, errors.New("Configure a default resolver pool before apply")
	}
	checksum := domain.ChecksumSnapshot(snapshot)
	stableJSON := domain.StableStringify(snapshot)
	ctx, conn := v.Context(), v.Conn()
	var revisionID string
	err = conn.QueryRowContext(ctx, `select id from config_revisions where checksum=? limit 1`, checksum).Scan(&revisionID)
	if errors.Is(err, sql.ErrNoRows) {
		var maxNumber sql.NullInt64
		if err := conn.QueryRowContext(ctx, `select max(revision_number) from config_revisions`).Scan(&maxNumber); err != nil {
			return ApplyResult{}, err
		}
		nextNumber := int64(1)
		if maxNumber.Valid {
			nextNumber = maxNumber.Int64 + 1
		}
		revisionID, err = v.NewUUID()
		if err != nil {
			return ApplyResult{}, err
		}
		actor, err := sqliteEnqueueActor(actorUserID)
		if err != nil {
			return ApplyResult{}, err
		}
		if _, err := conn.ExecContext(ctx, `insert into config_revisions
			(id,revision_number,checksum,snapshot,actor_user_id,created_at) values (?,?,?,?,?,?)`,
			revisionID, nextNumber, checksum, stableJSON, actor, v.Timestamp()); err != nil {
			return ApplyResult{}, err
		}
	} else if err != nil {
		return ApplyResult{}, err
	}
	if _, err := conn.ExecContext(ctx, `update installation_settings
		set current_desired_revision_id=?,updated_at=? where id=?`, revisionID, v.Timestamp(), installationID); err != nil {
		return ApplyResult{}, err
	}
	jobID, err := v.NewUUID()
	if err != nil {
		return ApplyResult{}, err
	}
	correlationID, err := v.NewUUID()
	if err != nil {
		return ApplyResult{}, err
	}
	actor, err := sqliteEnqueueActor(actorUserID)
	if err != nil {
		return ApplyResult{}, err
	}
	row := conn.QueryRowContext(ctx, `insert into apply_jobs
		(id,revision_id,actor_user_id,target,status,correlation_id,created_at)
		values (?,?,?,'combined','queued',?,?) returning
		id,revision_id,actor_user_id,target,status,source,source_primary_id,source_node_id,
		source_revision_id,snapshot_content_hash,snapshot_version,replication_version,
		leadership_generation,correlation_id,created_at,claimed_at,started_at,finished_at,
		validation_output,apply_output,health_output,error_message`, jobID, revisionID, actor, correlationID, v.Timestamp())
	job, err := scanJob(sqliteEnqueueJobScanner{row})
	if err != nil {
		return ApplyResult{}, err
	}
	return ApplyResult{RevisionID: revisionID, Job: job}, nil
}

// Match UUID input parsing only at the SQL binding boundary, not at entry.
func sqliteEnqueueActor(actor string) (any, error) {
	if actor == "" {
		return nil, nil
	}
	return sqlitedb.ParseUUIDLookupText(actor)
}

// Keep scanJob's nullable lifecycle destinations and shared output decoding.
// Decode the clocks returned by SQL, rather than assuming the writer's input.
type sqliteEnqueueJobScanner struct{ scanner }

func (s sqliteEnqueueJobScanner) Scan(dest ...any) error {
	created := dest[14].(*time.Time)
	lifecycle := [3]**time.Time{dest[15].(**time.Time), dest[16].(**time.Time), dest[17].(**time.Time)}
	var keys [4]*string
	for i := range keys {
		dest[14+i] = &keys[i]
	}
	if err := s.scanner.Scan(dest...); err != nil {
		return err
	}
	for i, key := range keys {
		value, err := sqliteEnqueueTime(key)
		if err != nil {
			return fmt.Errorf("apply job clock %d: %w", i, err)
		}
		if i == 0 {
			if value == nil {
				return errors.New("apply job created_at is NULL")
			}
			*created = *value
		} else {
			*lifecycle[i-1] = value
		}
	}
	return nil
}

func sqliteEnqueueTime(key *string) (*time.Time, error) {
	value, err := sqlitedb.DecodeTimestamp(key)
	if err != nil {
		return nil, err
	}
	switch stamp := value.(type) {
	case sqlitedb.NullTimestamp:
		return nil, nil
	case sqlitedb.FiniteTimestamp:
		// Go adds 62135596800 seconds internally: guard that signed addition.
		// At the negative calendar boundary, absolute-date arithmetic can wrap
		// into a positive year. Check UTC so local offsets cannot obscure it.
		// Neither PostgreSQL finite bounds nor JSON year limits apply here.
		if stamp.Seconds > math.MaxInt64-62135596800 {
			return nil, errors.New("apply job clock outside Go calendar range")
		}
		result := time.Unix(stamp.Seconds, int64(stamp.Nanos))
		if stamp.Seconds < 0 && result.UTC().Year() > 1970 {
			return nil, errors.New("apply job clock outside Go calendar range")
		}
		return &result, nil
	default:
		return nil, errors.New("nonfinite apply job clock")
	}
}
