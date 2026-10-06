package configuration

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/cluster"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/secrets"
	replicationsnapshot "github.com/zerkc/ProxyCore/apps/api/internal/snapshot"
	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

// sqlitePhase2Contract mirrors what cmd/server's phase2Backend and
// nodeConverterStore (minus the Store-only terminal reader wiring) need from a
// phase-2 store, plus the token listing main.go runs as raw SQL. cmd/server,
// httpserver and sync import this package, so assertions against their real
// interfaces cannot live here.
type sqlitePhase2Contract interface {
	CreateEnrollmentToken(ctx context.Context, id, selector, hash, version, ownerID string, createdAt, expiresAt time.Time) (domain.TopologyRole, error)
	CheckEnrollmentToken(ctx context.Context, selector, hash string, now time.Time, consume bool) error
	RevokeEnrollmentToken(ctx context.Context, id, ownerID string, now time.Time) error
	SnapshotPublicationStore
	SnapshotAcknowledgementStore
	CanonicalSnapshotStore
	ReadSnapshotPublicationByToken(context.Context, string, SnapshotPublicationIdentityRecord) ([]byte, error)
	replicationsnapshot.ArchiveStore
	CreateApplyJobFromSnapshot(ctx context.Context, sourceLabel string, snapshotBytes []byte) (uuid.UUID, error)
	TerminalApplyReader
	ListEnrollmentTokens(ctx context.Context, ownerID string) ([]EnrollmentTokenListRecord, error)
}

var (
	_ sqlitePhase2Contract             = (*SQLitePhase2Store)(nil)
	_ CanonicalSnapshotTransaction     = (*sqlitePhase2Transaction)(nil)
	_ replicationsnapshot.ArchiveStore = (*SQLitePhase2Store)(nil)
)

// EnrollmentTokenListRecord has the fields of httpserver.EnrollmentTokenRecord;
// httpserver imports this package, so the wiring adapts one into the other.
type EnrollmentTokenListRecord struct {
	ID            string
	Selector      string
	CreatedAt     time.Time
	ExpiresAt     time.Time
	ConsumedAt    *time.Time
	RevokedAt     *time.Time
	AttemptCount  int
	LastAttemptAt *time.Time
}

// SQLitePhase2Store is the unwired SQLite counterpart of PgPhase2Store. It
// delegates the pieces that already exist to their SQLite stores (token
// lifecycle, publication reads, acknowledgements, candidate scan, apply-job
// terminal reads) and adds the canonical-snapshot transaction, the imported
// apply job, the standalone archive and the token listing. The caller owns the
// database, writer and key store lifecycles.
type SQLitePhase2Store struct {
	*SQLiteEnrollmentTokenStore
	*SQLiteSnapshotPublicationStore
	*SQLiteSnapshotAcknowledgementStore
	*SQLiteCanonicalSnapshotCandidateStore
	*SQLiteApplyJobStore
	db     *sql.DB
	writer *sqlitedb.Writer
	keys   *cluster.SQLiteKeyStore
	master string
}

func NewSQLitePhase2Store(db *sql.DB, writer *sqlitedb.Writer, keys *cluster.SQLiteKeyStore, masterKeyBase64 string) *SQLitePhase2Store {
	return &SQLitePhase2Store{
		SQLiteEnrollmentTokenStore:            NewSQLiteEnrollmentTokenStore(writer),
		SQLiteSnapshotPublicationStore:        NewSQLiteSnapshotPublicationStore(writer, keys),
		SQLiteSnapshotAcknowledgementStore:    NewSQLiteSnapshotAcknowledgementStore(db, writer, keys),
		SQLiteCanonicalSnapshotCandidateStore: NewSQLiteCanonicalSnapshotCandidateStore(db),
		SQLiteApplyJobStore:                   NewSQLiteApplyJobStore(db),
		db:                                    db,
		writer:                                writer,
		keys:                                  keys,
		master:                                masterKeyBase64,
	}
}

// WithCanonicalSnapshot runs work inside one writer transaction. Writer's
// BEGIN IMMEDIATE provides the serialization PG's row locks give the producer.
// Any error from work rolls everything back and is returned unchanged.
func (s *SQLitePhase2Store) WithCanonicalSnapshot(ctx context.Context, work func(CanonicalSnapshotTransaction) error) error {
	if ctx == nil || s == nil || s.writer == nil || s.keys == nil || work == nil {
		return ErrSnapshotPublicationStore
	}
	return s.writer.Write(ctx, func(v sqlitedb.WriteValue) error {
		return work(&sqlitePhase2Transaction{v: v, keys: s.keys, master: s.master})
	})
}

type sqlitePhase2Transaction struct {
	v      sqlitedb.WriteValue
	keys   *cluster.SQLiteKeyStore
	master string
}

func (t *sqlitePhase2Transaction) LockPrimaryIdentity(ctx context.Context) (PrimaryIdentityRecord, error) {
	var installation, node, role string
	var generation, latest int64
	var keyText *string
	if err := t.v.Conn().QueryRowContext(ctx, `select installation_id, node_id, role, leadership_generation,
			latest_known_generation, cluster_key_id
		from installation_identity where id = ?`, installationIDSingleton).Scan(
		&installation, &node, &role, &generation, &latest, &keyText); err != nil || generation <= 0 || latest <= 0 {
		return PrimaryIdentityRecord{}, ErrEnrollmentGrantStore
	}
	record := PrimaryIdentityRecord{
		InstallationID: installation, NodeID: node, Role: domain.TopologyRole(role),
		LeadershipGeneration: uint64(generation), LatestKnownGeneration: uint64(latest),
	}
	if keyText != nil {
		key, err := uuid.Parse(*keyText)
		if err != nil {
			return PrimaryIdentityRecord{}, ErrEnrollmentGrantStore
		}
		record.ClusterKeyID = &key
	}
	return record, nil
}

func (t *sqlitePhase2Transaction) LoadOrCreateClusterKEK(ctx context.Context, currentID string) (ClusterKeyMaterial, error) {
	if t.keys == nil {
		return ClusterKeyMaterial{}, ErrEnrollmentGrantStore
	}
	var activeID *uuid.UUID
	if currentID != "" {
		parsed, err := uuid.Parse(currentID)
		if err != nil || parsed == uuid.Nil {
			return ClusterKeyMaterial{}, ErrEnrollmentGrantStore
		}
		activeID = &parsed
	}
	material, err := t.keys.LoadOrCreateInWrite(t.v, activeID, nil)
	if err != nil {
		return ClusterKeyMaterial{}, ErrEnrollmentGrantStore
	}
	defer material.Destroy()
	if currentID == "" {
		result, err := t.v.Conn().ExecContext(ctx, `update installation_identity
			set cluster_key_id = ?, updated_at = ?
			where id = ? and cluster_key_id is null`, material.ID.String(), t.v.Timestamp(), installationIDSingleton)
		if err != nil {
			return ClusterKeyMaterial{}, ErrEnrollmentGrantStore
		}
		if n, err := result.RowsAffected(); err != nil || n != 1 {
			return ClusterKeyMaterial{}, ErrEnrollmentGrantStore
		}
	}
	key := material.CopyBytes()
	defer zeroGrantBytes(key)
	result, err := NewClusterKeyMaterial(material.ID.String(), key)
	if err != nil {
		return ClusterKeyMaterial{}, ErrEnrollmentGrantStore
	}
	return result, nil
}

func (t *sqlitePhase2Transaction) GetCanonicalSnapshotApply(ctx context.Context, id string) (CanonicalSnapshotApply, error) {
	canonical, err := sqliteCertificateLookupID(id)
	if err != nil {
		return CanonicalSnapshotApply{}, err
	}
	var (
		apply                     CanonicalSnapshotApply
		revisionSource, jobSource string
		desired                   string
		finished, revisionApplied *string
	)
	err = t.v.Conn().QueryRowContext(ctx, `select j.id, j.revision_id, r.revision_number, r.snapshot,
			j.status, j.target, j.source,
			j.source_primary_id, j.source_node_id, j.source_revision_id,
			j.snapshot_content_hash, j.snapshot_version, j.replication_version,
			j.leadership_generation, j.finished_at,
			r.source, r.source_primary_id, r.source_node_id,
			r.source_revision_id, r.snapshot_content_hash, r.snapshot_version,
			r.replication_version, r.leadership_generation, r.applied_at
		from apply_jobs j
		join config_revisions r on r.id = j.revision_id
		where j.id = ?`, canonical).Scan(
		&apply.JobID, &apply.RevisionID, &apply.RevisionNumber, &desired,
		&apply.Status, &apply.Target, &jobSource,
		&apply.SourcePrimaryID, &apply.SourceNodeID, &apply.SourceRevisionID,
		&apply.SnapshotContentHash, &apply.SnapshotVersion, &apply.ReplicationVersion,
		&apply.LeadershipGeneration, &finished,
		&revisionSource, &apply.RevisionSourcePrimaryID, &apply.RevisionSourceNodeID,
		&apply.RevisionSourceRevisionID, &apply.RevisionSnapshotHash,
		&apply.RevisionSnapshotVersion, &apply.RevisionReplication,
		&apply.RevisionGeneration, &revisionApplied)
	if errors.Is(err, sql.ErrNoRows) {
		return CanonicalSnapshotApply{}, ErrCanonicalSnapshotNotFound
	}
	if err != nil {
		return CanonicalSnapshotApply{}, err
	}
	if apply.FinishedAt, err = sqliteEnqueueTime(finished); err != nil {
		return CanonicalSnapshotApply{}, fmt.Errorf("apply job finished_at: %w", err)
	}
	if apply.RevisionAppliedAt, err = sqliteEnqueueTime(revisionApplied); err != nil {
		return CanonicalSnapshotApply{}, fmt.Errorf("revision applied_at: %w", err)
	}
	apply.DesiredSnapshot = []byte(desired)
	apply.Source = PersistenceSource(jobSource)
	apply.RevisionSource = PersistenceSource(revisionSource)
	return apply, nil
}

const sqliteAppliedSnapshotColumns = `id, source_primary_id, leadership_generation,
	snapshot_version, replication_version, content_hash, snapshot_body,
	revision_id, status, apply_job_id, failure_code, applied_at, discarded_at`

func sqliteScanCanonicalSnapshot(row scanner) (*CanonicalSnapshotRecord, error) {
	var (
		record            AppliedSnapshotRecord
		status            string
		applied           string
		discarded         *string
		appliedTime, void *time.Time
	)
	if err := row.Scan(&record.ID, &record.SourcePrimaryID, &record.LeadershipGeneration,
		&record.SnapshotVersion, &record.ReplicationVersion, &record.ContentHash, &record.SnapshotBody,
		&record.RevisionID, &status, &record.ApplyJobID, &record.FailureCode, &applied, &discarded); err != nil {
		return nil, err
	}
	var err error
	if appliedTime, err = sqliteEnqueueTime(&applied); err != nil || appliedTime == nil {
		return nil, errors.New("applied snapshot applied_at is not a finite clock")
	}
	record.AppliedAt = *appliedTime
	if void, err = sqliteEnqueueTime(discarded); err != nil {
		return nil, fmt.Errorf("applied snapshot discarded_at: %w", err)
	}
	record.DiscardedAt = void
	record.Status = AppliedSnapshotStatus(status)
	return &CanonicalSnapshotRecord{AppliedSnapshotRecord: record}, nil
}

func (t *sqlitePhase2Transaction) FindCanonicalSnapshotForJob(ctx context.Context, jobID string) (*CanonicalSnapshotRecord, error) {
	canonical, err := sqliteCertificateLookupID(jobID)
	if err != nil {
		return nil, err
	}
	record, err := sqliteScanCanonicalSnapshot(t.v.Conn().QueryRowContext(ctx, `select `+sqliteAppliedSnapshotColumns+`
		from applied_snapshots where apply_job_id = ?
		order by applied_at desc, id desc limit 1`, canonical))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return record, err
}

func (t *sqlitePhase2Transaction) findCanonicalSnapshotByHash(ctx context.Context, contentHash string) (*CanonicalSnapshotRecord, error) {
	record, err := sqliteScanCanonicalSnapshot(t.v.Conn().QueryRowContext(ctx, `select `+sqliteAppliedSnapshotColumns+`
		from applied_snapshots where content_hash = ?`, contentHash))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return record, err
}

func (t *sqlitePhase2Transaction) ListSecrets(ctx context.Context) ([]replicationsnapshot.PlainSecret, error) {
	rows, err := t.v.Conn().QueryContext(ctx, `select id, purpose, ciphertext from secrets order by id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]replicationsnapshot.PlainSecret, 0)
	for rows.Next() {
		var idText, purpose, ciphertext string
		if err := rows.Scan(&idText, &purpose, &ciphertext); err != nil {
			zeroCanonicalPlainSecrets(result)
			return nil, err
		}
		id, err := uuid.Parse(idText)
		if err != nil {
			zeroCanonicalPlainSecrets(result)
			return nil, err
		}
		plaintext, err := secrets.DecryptSecret(ciphertext, t.master)
		if err != nil {
			zeroCanonicalPlainSecrets(result)
			return nil, err
		}
		result = append(result, replicationsnapshot.PlainSecret{ID: id, Purpose: purpose, Value: []byte(plaintext)})
	}
	if err := rows.Err(); err != nil {
		zeroCanonicalPlainSecrets(result)
		return nil, err
	}
	return result, nil
}

func (t *sqlitePhase2Transaction) ListReplicableOwners(ctx context.Context) ([]replicationsnapshot.ReplicableOwner, error) {
	rows, err := t.v.Conn().QueryContext(ctx, `select id, username, password_hash, role from users order by id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]replicationsnapshot.ReplicableOwner, 0)
	for rows.Next() {
		var owner replicationsnapshot.ReplicableOwner
		var idText string
		if err := rows.Scan(&idText, &owner.Username, &owner.PasswordHash, &owner.Role); err != nil {
			return nil, err
		}
		if owner.UserID, err = uuid.Parse(idText); err != nil {
			return nil, err
		}
		result = append(result, owner)
	}
	return result, rows.Err()
}

// PublishCanonicalSnapshot mirrors the PG transaction method, including its
// idempotent replay and ErrCanonicalSnapshotConflict mapping. Unlike PG, a
// failed statement does not poison the transaction, so the duplicate-hash
// fallback after a failed insert can still read.
func (t *sqlitePhase2Transaction) PublishCanonicalSnapshot(ctx context.Context, write CanonicalSnapshotWrite) error {
	if t == nil || len(write.SnapshotBody) == 0 || !validPublicationUUID(write.ID) ||
		!validPublicationUUID(write.SourcePrimaryID) || !validPublicationUUID(write.RevisionID) ||
		!validPublicationUUID(write.ApplyJobID) || write.LeadershipGeneration <= 0 ||
		write.SnapshotVersion <= 0 || write.ReplicationVersion <= 0 || write.ContentHash == "" ||
		write.AppliedAt.IsZero() {
		return ErrCanonicalSnapshotConflict
	}
	appliedAt, err := sqliteEnrollmentTokenTime(write.AppliedAt)
	if err != nil {
		return err
	}
	if existing, err := t.findCanonicalSnapshotByHash(ctx, write.ContentHash); err != nil {
		return err
	} else if existing != nil {
		if sqliteCanonicalSnapshotMatches(existing.AppliedSnapshotRecord, write) {
			return nil
		}
		return ErrCanonicalSnapshotConflict
	}
	conn := t.v.Conn()
	revision, err := conn.ExecContext(ctx, `update config_revisions
		set source = 'ordinary', source_primary_id = ?, source_node_id = null,
			source_revision_id = null, snapshot_content_hash = ?,
			snapshot_version = ?, replication_version = ?, leadership_generation = ?
		where id = ? and source = 'ordinary' and applied_at is not null`,
		write.SourcePrimaryID, write.ContentHash, write.SnapshotVersion, write.ReplicationVersion,
		write.LeadershipGeneration, write.RevisionID)
	if err := sqliteSingleRow(revision, err); err != nil {
		return err
	}
	job, err := conn.ExecContext(ctx, `update apply_jobs
		set source = 'ordinary', source_primary_id = ?, source_node_id = null,
			source_revision_id = null, snapshot_content_hash = ?,
			snapshot_version = ?, replication_version = ?,
			leadership_generation = ?, target = 'combined'
		where id = ? and revision_id = ? and status = 'applied' and finished_at is not null`,
		write.SourcePrimaryID, write.ContentHash, write.SnapshotVersion, write.ReplicationVersion,
		write.LeadershipGeneration, write.ApplyJobID, write.RevisionID)
	if err := sqliteSingleRow(job, err); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `insert into applied_snapshots (
			id, source_primary_id, leadership_generation, snapshot_version,
			replication_version, content_hash, snapshot_body, revision_id,
			status, apply_job_id, failure_code, applied_at, discarded_at
		) values (?, ?, ?, ?, ?, ?, ?, ?, 'applied', ?, null, ?, null)`,
		write.ID, write.SourcePrimaryID, write.LeadershipGeneration, write.SnapshotVersion,
		write.ReplicationVersion, write.ContentHash, write.SnapshotBody, write.RevisionID,
		write.ApplyJobID, appliedAt); err != nil {
		if existing, lookupErr := t.findCanonicalSnapshotByHash(ctx, write.ContentHash); lookupErr == nil &&
			existing != nil && sqliteCanonicalSnapshotMatches(existing.AppliedSnapshotRecord, write) {
			return nil
		}
		return err
	}
	return nil
}

// sqliteSingleRow maps an exec outcome to PG's "err, or exactly one row" rule.
func sqliteSingleRow(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		if err != nil {
			return err
		}
		return ErrCanonicalSnapshotConflict
	}
	return nil
}

func sqliteCanonicalSnapshotMatches(record AppliedSnapshotRecord, write CanonicalSnapshotWrite) bool {
	return record.Status == AppliedSnapshotApplied && record.DiscardedAt == nil &&
		record.SourcePrimaryID == write.SourcePrimaryID &&
		record.LeadershipGeneration == write.LeadershipGeneration &&
		record.SnapshotVersion == write.SnapshotVersion &&
		record.ReplicationVersion == write.ReplicationVersion &&
		record.ContentHash == write.ContentHash && record.RevisionID != nil &&
		*record.RevisionID == write.RevisionID && record.ApplyJobID != nil &&
		*record.ApplyJobID == write.ApplyJobID && record.AppliedAt.Equal(write.AppliedAt) &&
		bytes.Equal(record.SnapshotBody, write.SnapshotBody)
}

// CreateApplyJobFromSnapshot persists one imported revision and its queued
// combined job in one writer transaction (createApplyJobFromSnapshot). PG's
// pg_notify has no counterpart: waking the worker is the caller's concern.
func (s *SQLitePhase2Store) CreateApplyJobFromSnapshot(ctx context.Context, sourceLabel string, snapshotBytes []byte) (uuid.UUID, error) {
	if ctx == nil {
		return uuid.Nil, errors.New("import apply context is required")
	}
	if s == nil || s.writer == nil {
		return uuid.Nil, errors.New("import apply store is required")
	}
	if sourceLabel != string(PersistenceSourceImport) {
		return uuid.Nil, fmt.Errorf("unsupported import source %q", sourceLabel)
	}
	metadata, err := importedSnapshotMetadata(snapshotBytes)
	if err != nil {
		return uuid.Nil, fmt.Errorf("read imported snapshot metadata: %w", err)
	}
	var jobID uuid.UUID
	err = s.writer.Write(ctx, func(v sqlitedb.WriteValue) error {
		conn := v.Conn()
		var revisionNumber int
		if err := conn.QueryRowContext(ctx, `select coalesce(max(revision_number), 0) + 1 from config_revisions`).Scan(&revisionNumber); err != nil {
			return err
		}
		revisionID, correlationID, jobText := uuid.NewString(), uuid.NewString(), uuid.NewString()
		if _, err := conn.ExecContext(ctx, `insert into config_revisions (
				id, revision_number, checksum, snapshot, source, source_primary_id,
				source_node_id, source_revision_id, snapshot_content_hash,
				snapshot_version, replication_version, leadership_generation, created_at, applied_at
			) values (?, ?, ?, ?, 'import', ?, ?, null, ?, ?, ?, ?, ?, null)`,
			revisionID, revisionNumber, metadata.snapshotHash, string(snapshotBytes),
			metadata.sourcePrimaryID.String(), metadata.sourceNodeID.String(), metadata.contentHash,
			metadata.snapshotVersion, metadata.replicationVersion, metadata.leadershipGeneration, v.Timestamp()); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `insert into apply_jobs (
				id, revision_id, actor_user_id, target, status, source,
				source_primary_id, source_node_id, source_revision_id,
				snapshot_content_hash, snapshot_version, replication_version,
				leadership_generation, correlation_id, created_at
			) values (?, ?, null, 'combined', 'queued', 'import', ?, ?, null, ?, ?, ?, ?, ?, ?)`,
			jobText, revisionID, metadata.sourcePrimaryID.String(), metadata.sourceNodeID.String(),
			metadata.contentHash, metadata.snapshotVersion, metadata.replicationVersion,
			metadata.leadershipGeneration, correlationID, v.Timestamp()); err != nil {
			return err
		}
		jobID = uuid.MustParse(jobText)
		return nil
	})
	if err != nil {
		return uuid.Nil, err
	}
	return jobID, nil
}

// SaveStandaloneArchive mirrors saveStandaloneArchive in one writer
// transaction. created_at is the writer clock; retention_started_at stays NULL
// exactly as PG's insert leaves it.
func (s *SQLitePhase2Store) SaveStandaloneArchive(ctx context.Context, archive replicationsnapshot.StandaloneArchive) error {
	if ctx == nil {
		return errors.New("archive context is required")
	}
	if s == nil || s.writer == nil {
		return errors.New("archive store is required")
	}
	if archive.ID == uuid.Nil || archive.CapturedAt.IsZero() || archive.ExpiresAt.IsZero() || len(archive.SnapshotData) == 0 {
		return errors.New("archive is incomplete")
	}
	captured, err := sqliteEnrollmentTokenTime(archive.CapturedAt)
	if err != nil {
		return err
	}
	expires, err := sqliteEnrollmentTokenTime(archive.ExpiresAt)
	if err != nil {
		return err
	}
	return s.writer.Write(ctx, func(v sqlitedb.WriteValue) error {
		_, err := v.Conn().ExecContext(ctx, `insert into standalone_archives
			(id, archive_blob, capture_reason, captured_at, expires_at, created_at)
			values (?, ?, ?, ?, ?, ?)`,
			archive.ID.String(), string(archive.SnapshotData), archive.Reason, captured, expires, v.Timestamp())
		return err
	})
}

// PurgeExpiredArchives deletes archives that expired before now (wall clock
// when zero) and reports how many were removed.
func (s *SQLitePhase2Store) PurgeExpiredArchives(ctx context.Context, now time.Time) (int, error) {
	if ctx == nil {
		return 0, errors.New("archive context is required")
	}
	if s == nil || s.writer == nil {
		return 0, errors.New("archive store is required")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	key, err := sqliteEnrollmentTokenTime(now)
	if err != nil {
		return 0, err
	}
	var removed int64
	err = s.writer.Write(ctx, func(v sqlitedb.WriteValue) error {
		result, err := v.Conn().ExecContext(ctx, `delete from standalone_archives where expires_at is not null and expires_at < ?`, key)
		if err != nil {
			return err
		}
		removed, err = result.RowsAffected()
		return err
	})
	return int(removed), err
}

// ListEnrollmentTokens is main.go's listEnrollmentTokens: the owner's tokens,
// newest first (created_at, then id), with grant attempt count and last attempt.
func (s *SQLitePhase2Store) ListEnrollmentTokens(ctx context.Context, ownerID string) ([]EnrollmentTokenListRecord, error) {
	if ctx == nil || s == nil || s.db == nil || strings.TrimSpace(ownerID) == "" {
		return nil, errors.New("enrollment token list unavailable")
	}
	owner, err := sqliteCertificateLookupID(ownerID)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `select t.id, t.token_selector, t.created_at, t.expires_at,
			t.consumed_at, t.revoked_at, count(g.attempt_id), max(g.created_at)
		from enrollment_tokens t
		left join enrollment_grants g on g.token_id = t.id
		where t.created_by_user_id = ?
		group by t.id, t.token_selector, t.created_at, t.expires_at, t.consumed_at, t.revoked_at
		order by t.created_at desc, t.id desc`, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]EnrollmentTokenListRecord, 0)
	for rows.Next() {
		var record EnrollmentTokenListRecord
		var created, expires string
		var consumed, revoked, last *string
		var attempts int64
		if err := rows.Scan(&record.ID, &record.Selector, &created, &expires, &consumed, &revoked, &attempts, &last); err != nil {
			return nil, err
		}
		if attempts > int64(^uint(0)>>1) {
			return nil, errors.New("enrollment token attempt count overflow")
		}
		record.AttemptCount = int(attempts)
		for _, field := range []struct {
			key  *string
			into **time.Time
		}{{consumed, &record.ConsumedAt}, {revoked, &record.RevokedAt}, {last, &record.LastAttemptAt}} {
			if *field.into, err = sqliteEnqueueTime(field.key); err != nil {
				return nil, fmt.Errorf("enrollment token clock: %w", err)
			}
		}
		for _, field := range []struct {
			key  *string
			into *time.Time
		}{{&created, &record.CreatedAt}, {&expires, &record.ExpiresAt}} {
			value, err := sqliteEnqueueTime(field.key)
			if err != nil || value == nil {
				return nil, errors.New("enrollment token clock is not finite")
			}
			*field.into = *value
		}
		result = append(result, record)
	}
	return result, rows.Err()
}
