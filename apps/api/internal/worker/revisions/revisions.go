// Package revisions ports the worker's SQLite revision store from
// packages/db/src/sqlite-foundation (revision-record, revision-latest-record and
// revision-mark-applied-record). Revision creation stays with the API.
package revisions

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
	"github.com/zerkc/ProxyCore/apps/api/internal/worker/store"
)

// Revision mirrors the TS RevisionRecord. Snapshot keeps the stored JSON bytes
// so callers decode it with the snapshot package. A tagged infinity projects to
// the zero time, like the TS Invalid Date.
type Revision struct {
	ID                   string
	RevisionNumber       int32
	Checksum             string
	Snapshot             json.RawMessage
	ActorUserID          *string
	Source               store.Source
	SourcePrimaryID      *string
	SourceNodeID         *string
	SourceRevisionID     *string
	SnapshotContentHash  *string
	SnapshotVersion      *int32
	ReplicationVersion   *int32
	LeadershipGeneration *int64
	CreatedAt            time.Time
	AppliedAt            *time.Time
}

// ErrRevisionNotFound is returned by MarkApplied when no row has the id.
var ErrRevisionNotFound = errors.New("revision not found")

const columns = `id,revision_number,checksum,snapshot,actor_user_id,created_at,applied_at,source,
	source_primary_id,source_node_id,source_revision_id,snapshot_content_hash,snapshot_version,
	replication_version,leadership_generation`

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// Store reads revisions and marks them applied. The caller owns the *sql.DB.
type Store struct {
	db     *sql.DB
	writer *sqlitedb.Writer
	clock  func() time.Time
}

// Option customizes a Store.
type Option func(*Store)

// WithClock injects the time source used when markApplied has no explicit time
// and for initializing a missing installation_settings row.
func WithClock(clock func() time.Time) Option { return func(s *Store) { s.clock = clock } }

// New builds a revision store over a migrated database opened by sqlitedb.Open.
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

// Get returns the revision with the literal id, or nil when there is none.
func (s *Store) Get(ctx context.Context, id string) (*Revision, error) {
	return one(s.db.QueryRowContext(ctx, "SELECT "+columns+" FROM main.config_revisions WHERE id = ?", id))
}

// Latest returns the revision with the highest revision_number, or nil.
func (s *Store) Latest(ctx context.Context) (*Revision, error) {
	return one(s.db.QueryRowContext(ctx, "SELECT "+columns+" FROM main.config_revisions ORDER BY revision_number DESC LIMIT 1"))
}

// LatestApplied returns the reconcile target: the revision of the most recent
// applied job, ordered by finished_at (created_at when it was never finished),
// skipping revisions that have no applied_at. Reconcile jobs are applied jobs
// too, which keeps the target stable after a repair. It is not Latest(): the API
// reuses an older revision (keeping its old number) when a snapshot checksum
// matches, so the highest number is not necessarily the one applied last. Only
// when no applied job yields a revision does it fall back to the highest-numbered
// revision, if that one has applied_at; nil otherwise. This is the rule of
// latestAppliedRevision in apps/worker/src/poller.ts.
func (s *Store) LatestApplied(ctx context.Context) (*Revision, error) {
	prefixed := ""
	for i, column := range strings.Split(columns, ",") {
		if i > 0 {
			prefixed += ","
		}
		prefixed += "r." + strings.TrimSpace(column)
	}
	found, err := one(s.db.QueryRowContext(ctx, `SELECT `+prefixed+` FROM main.apply_jobs j
		JOIN main.config_revisions r ON r.id = j.revision_id
		WHERE j.status = 'applied' AND r.applied_at IS NOT NULL
		ORDER BY coalesce(j.finished_at, j.created_at) DESC, j.created_at DESC LIMIT 1`))
	if err != nil || found != nil {
		return found, err
	}
	latest, err := s.Latest(ctx)
	if err != nil || latest == nil || latest.AppliedAt == nil {
		return nil, err
	}
	return latest, nil
}

// MarkApplied stamps applied_at on the revision and, in the same transaction,
// points installation_settings.current_applied_revision_id at it (creating the
// default settings row when missing). A zero appliedAt means the store clock.
func (s *Store) MarkApplied(ctx context.Context, id string, appliedAt time.Time) (Revision, error) {
	if appliedAt.IsZero() {
		appliedAt = s.clock()
	}
	key, err := sqlitedb.EncodeTimestamp(sqlitedb.FiniteTimestamp{Seconds: appliedAt.Unix(), Nanos: int32(appliedAt.Nanosecond())})
	if err != nil {
		return Revision{}, err
	}
	var marked Revision
	err = s.writer.Write(ctx, func(v sqlitedb.WriteValue) error {
		conn, c := v.Conn(), v.Context()
		revision, err := one(conn.QueryRowContext(c, "UPDATE main.config_revisions SET applied_at=? WHERE id=? RETURNING "+columns, *key, id))
		if err != nil {
			return err
		}
		if revision == nil {
			return fmt.Errorf("%w: %s", ErrRevisionNotFound, id)
		}
		if _, err := conn.ExecContext(c, `INSERT INTO main.installation_settings
			(id,forwarding_rules,default_resolver_pool,created_at,updated_at)
			VALUES ('default','[]',NULL,?,?) ON CONFLICT DO NOTHING`, v.Timestamp(), v.Timestamp()); err != nil {
			return err
		}
		if _, err := conn.ExecContext(c, `UPDATE main.installation_settings SET current_applied_revision_id=?,updated_at=?
			WHERE id='default'`, id, *key); err != nil {
			return err
		}
		marked = *revision
		return nil
	})
	return marked, err
}

func one(row *sql.Row) (*Revision, error) {
	revision, err := scan(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &revision, nil
}

func scan(row *sql.Row) (Revision, error) {
	var (
		id, checksum, snapshot, createdAt, source           string
		actor, applied, primary, node, sourceRevision, hash sql.NullString
		number                                              int64
		snapshotVersion, replicationVersion, leadership     sql.NullInt64
	)
	if err := row.Scan(&id, &number, &checksum, &snapshot, &actor, &createdAt, &applied, &source,
		&primary, &node, &sourceRevision, &hash, &snapshotVersion, &replicationVersion, &leadership); err != nil {
		return Revision{}, err
	}
	r := Revision{ID: id, Checksum: checksum, Source: store.Source(source)}
	if !uuidPattern.MatchString(id) {
		return Revision{}, errors.New("Invalid stored UUID id")
	}
	if r.Source != store.SourceOrdinary && r.Source != store.SourceImport && r.Source != store.SourceSync {
		return Revision{}, errors.New("Invalid source domain")
	}
	if number < math.MinInt32 || number > math.MaxInt32 {
		return Revision{}, errors.New("Invalid INTEGER revision_number")
	}
	r.RevisionNumber = int32(number)
	if strings.ContainsRune(checksum, 0) || strings.ContainsRune(snapshot, 0) || !json.Valid([]byte(snapshot)) {
		return Revision{}, errors.New("Invalid revision TEXT")
	}
	r.Snapshot = json.RawMessage(snapshot)
	var err error
	if r.CreatedAt, err = decode("created_at", createdAt); err != nil {
		return Revision{}, err
	}
	if applied.Valid {
		at, err := decode("applied_at", applied.String)
		if err != nil {
			return Revision{}, err
		}
		r.AppliedAt = &at
	}
	for _, f := range []struct {
		name string
		in   sql.NullString
		out  **string
	}{{"actor_user_id", actor, &r.ActorUserID}, {"source_primary_id", primary, &r.SourcePrimaryID},
		{"source_node_id", node, &r.SourceNodeID}, {"source_revision_id", sourceRevision, &r.SourceRevisionID}} {
		if !f.in.Valid {
			continue
		}
		if !uuidPattern.MatchString(f.in.String) {
			return Revision{}, fmt.Errorf("Invalid stored UUID %s", f.name)
		}
		v := f.in.String
		*f.out = &v
	}
	if hash.Valid {
		if strings.ContainsRune(hash.String, 0) {
			return Revision{}, errors.New("Invalid TEXT snapshot_content_hash")
		}
		r.SnapshotContentHash = &hash.String
	}
	for _, f := range []struct {
		name string
		in   sql.NullInt64
		out  **int32
	}{{"snapshot_version", snapshotVersion, &r.SnapshotVersion}, {"replication_version", replicationVersion, &r.ReplicationVersion}} {
		if !f.in.Valid {
			continue
		}
		if f.in.Int64 < math.MinInt32 || f.in.Int64 > math.MaxInt32 {
			return Revision{}, fmt.Errorf("Invalid INTEGER %s", f.name)
		}
		v := int32(f.in.Int64)
		*f.out = &v
	}
	if leadership.Valid {
		r.LeadershipGeneration = &leadership.Int64
	}
	return r, nil
}

func decode(name, key string) (time.Time, error) {
	decoded, err := sqlitedb.DecodeTimestamp(&key)
	if err != nil {
		return time.Time{}, fmt.Errorf("Invalid revision %s: %w", name, err)
	}
	if finite, ok := decoded.(sqlitedb.FiniteTimestamp); ok {
		return time.Unix(finite.Seconds, int64(finite.Nanos)).UTC(), nil
	}
	return time.Time{}, nil
}
