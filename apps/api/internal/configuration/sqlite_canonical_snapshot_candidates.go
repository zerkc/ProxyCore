package configuration

import (
	"context"
	"database/sql"
	"errors"
)

// SQLiteCanonicalSnapshotCandidateStore is an unwired, read-only candidate
// reader. The caller retains ownership of the shared database handle.
type SQLiteCanonicalSnapshotCandidateStore struct {
	db *sql.DB
}

func NewSQLiteCanonicalSnapshotCandidateStore(db *sql.DB) *SQLiteCanonicalSnapshotCandidateStore {
	return &SQLiteCanonicalSnapshotCandidateStore{db: db}
}

// ListCanonicalSnapshotCandidates reads one statement-consistent view. These
// advisory candidates are not reservations: publication must revalidate later.
// Tagged TEXT clocks are ordered in SQL, without decoding or narrowing them.
func (s *SQLiteCanonicalSnapshotCandidateStore) ListCanonicalSnapshotCandidates(ctx context.Context) ([]CanonicalSnapshotApplyRef, error) {
	if ctx == nil {
		return nil, errors.New("snapshot candidate context is required")
	}
	if s == nil || s.db == nil {
		return nil, ErrSnapshotPublicationStore
	}
	rows, err := s.db.QueryContext(ctx, `
  select j.id
  from apply_jobs j
  left join applied_snapshots a
   on a.apply_job_id = j.id
   and a.status = 'applied'
   and a.discarded_at is null
   and a.snapshot_body is not null
  where j.status = 'applied'
    and j.target = 'combined'
    and j.source = 'ordinary'
    and j.finished_at is not null
    and a.id is null
    -- A job whose content hash already has a published row was superseded by a
    -- newer revision publishing the same hash (a revert): it is not a candidate.
    -- A discarded row (healed stale body) does not count: its job is regenerated.
    and not exists (select 1 from applied_snapshots s
      where j.snapshot_content_hash is not null and s.content_hash = j.snapshot_content_hash
        and s.discarded_at is null)
  order by j.finished_at asc, j.id asc
 `)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	candidates := make([]CanonicalSnapshotApplyRef, 0)
	for rows.Next() {
		var candidate CanonicalSnapshotApplyRef
		if err := rows.Scan(&candidate.JobID); err != nil {
			return nil, err
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return candidates, nil
}
