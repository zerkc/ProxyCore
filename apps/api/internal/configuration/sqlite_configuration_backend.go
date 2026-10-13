package configuration

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/secrets"
	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

// sqliteConfigurationBackendContract mirrors httpserver.ConfigurationBackend
// (ConfigurationStore + CertificateStore). httpserver imports this package, so
// the real assertion cannot live here; a method drift fails this build first.
type sqliteConfigurationBackendContract interface {
	GetSettings(ctx context.Context) (domain.Settings, error)
	InitializeIngress(ctx context.Context, ingress domain.Ingress) (bool, error)
	UpdateSettings(ctx context.Context, patch SettingsPatch) (domain.Settings, error)
	ListZones(ctx context.Context) ([]domain.ZoneState, error)
	GetZone(ctx context.Context, id string) (domain.ZoneState, error)
	CreateZone(ctx context.Context, name, actorUserID string) (AutoApplyResult[domain.ZoneState], error)
	AddRecord(ctx context.Context, zoneID string, input domain.DNSRecordInput, actorUserID string) (AutoApplyResult[domain.DNSRecord], error)
	ListStreams(ctx context.Context) ([]domain.StreamRoute, error)
	AddStream(ctx context.Context, input domain.StreamRoute) (domain.StreamRoute, error)
	DeleteStream(ctx context.Context, id string) error
	CreateApplyJob(ctx context.Context, actorUserID string) (ApplyResult, error)
	Status(ctx context.Context) (StatusResult, error)
	GetEnrollmentHostnames(ctx context.Context) (EnrollmentHostnameConfig, error)
	UpdateEnrollmentHostnames(ctx context.Context, hostnames []string) (EnrollmentHostnameConfig, error)
	ListCertificates(ctx context.Context) ([]domain.CertificateStatus, error)
	IssueCertificate(ctx context.Context, input CertificateIssueInput, actorUserID string) (domain.CertificateStatus, error)
	DeleteCertificate(ctx context.Context, id string) error
	RenewSelfSignedCertificateByID(ctx context.Context, id string) (RenewResult, error)
	EnsureInternalCACertificatePEM(ctx context.Context) (string, error)
	LoadEnrollmentTLSTrust(ctx context.Context) (EnrollmentTLSTrust, error)
}

var (
	_ sqliteConfigurationBackendContract = (*SQLiteConfigurationBackend)(nil)
)

// SQLiteConfigurationBackend is the unwired SQLite composition of the
// configuration and certificate stores. Every part shares one read *sql.DB and
// one *sqlitedb.Writer. Methods are promoted from the embedded stores; Status,
// ForceApply and CreateApplyJob are added here with PostgreSQL semantics.
type SQLiteConfigurationBackend struct {
	*SQLiteSettingsStore
	*SQLiteZoneReadStore
	*SQLiteRecordWriteStore
	*SQLiteStreamStore
	*SQLiteCertificateIssueStore
	db     *sql.DB
	writer *sqlitedb.Writer
}

// NewSQLiteConfigurationBackend is cheap and nil-safe for secretStore: a nil
// (or typed-nil *secrets.SQLiteStore) store behaves like PG's nil secrets.Store.
func NewSQLiteConfigurationBackend(db *sql.DB, writer *sqlitedb.Writer, ingress domain.Ingress, secretStore sqliteInternalCASecrets) *SQLiteConfigurationBackend {
	if typed, ok := secretStore.(*secrets.SQLiteStore); ok && typed == nil {
		secretStore = nil
	}
	return &SQLiteConfigurationBackend{
		SQLiteSettingsStore:         NewSQLiteSettingsStore(db, writer, ingress),
		SQLiteZoneReadStore:         NewSQLiteZoneReadStore(db),
		SQLiteRecordWriteStore:      NewSQLiteRecordWriteStore(writer, secretStore),
		SQLiteStreamStore:           NewSQLiteStreamStore(db, writer),
		SQLiteCertificateIssueStore: NewSQLiteCertificateIssueStore(db, writer, secretStore),
		db:                          db,
		writer:                      writer,
	}
}

// CreateApplyJob enqueues an apply job for the current desired state in one
// writer transaction (Store.CreateApplyJob).
func (b *SQLiteConfigurationBackend) CreateApplyJob(ctx context.Context, actorUserID string) (ApplyResult, error) {
	return sqliteCreateApplyJob(ctx, b.writer, actorUserID)
}

// ForceApply is identical to CreateApplyJob, as in Store.ForceApply.
func (b *SQLiteConfigurationBackend) ForceApply(ctx context.Context, actorUserID string) (ApplyResult, error) {
	return b.CreateApplyJob(ctx, actorUserID)
}

// Status aggregates the desired/applied state like Store.Status. GetSettings
// runs first because it creates the installation row when missing.
func (b *SQLiteConfigurationBackend) Status(ctx context.Context) (StatusResult, error) {
	var result StatusResult
	var err error
	if result.Settings, err = b.GetSettings(ctx); err != nil {
		return StatusResult{}, err
	}
	var desiredID, appliedID *string
	if err := b.db.QueryRowContext(ctx, `select current_desired_revision_id,current_applied_revision_id
		from installation_settings where id=?`, installationID).Scan(&desiredID, &appliedID); err != nil {
		return StatusResult{}, err
	}
	if desiredID != nil {
		if result.DesiredRevision, err = sqliteGetRevision(ctx, b.db, *desiredID); err != nil {
			return StatusResult{}, err
		}
	}
	if appliedID != nil {
		if result.AppliedRevision, err = sqliteGetRevision(ctx, b.db, *appliedID); err != nil {
			return StatusResult{}, err
		}
	}
	if result.Jobs, err = sqliteListJobs(ctx, b.db); err != nil {
		return StatusResult{}, err
	}
	if result.Zones, err = b.ListZones(ctx); err != nil {
		return StatusResult{}, err
	}
	if result.Streams, err = b.ListStreams(ctx); err != nil {
		return StatusResult{}, err
	}
	if result.Certificates, err = b.ListCertificates(ctx); err != nil {
		return StatusResult{}, err
	}
	return result, nil
}

// sqliteGetRevision mirrors getRevision: a missing row is (nil, nil) and an
// undecodable snapshot leaves Snapshot nil.
func sqliteGetRevision(ctx context.Context, db *sql.DB, id string) (*RevisionRecord, error) {
	var (
		rev                    RevisionRecord
		snapshot               []byte
		createdKey, appliedKey *string
	)
	err := db.QueryRowContext(ctx, `select id,revision_number,checksum,snapshot,actor_user_id,
		source,source_primary_id,source_node_id,source_revision_id,snapshot_content_hash,
		snapshot_version,replication_version,leadership_generation,created_at,applied_at
		from config_revisions where id=?`, id).Scan(
		&rev.ID, &rev.RevisionNumber, &rev.Checksum, &snapshot, &rev.ActorUserID,
		&rev.Source, &rev.SourcePrimaryID, &rev.SourceNodeID, &rev.SourceRevisionID,
		&rev.SnapshotContentHash, &rev.SnapshotVersion, &rev.ReplicationVersion,
		&rev.LeadershipGeneration, &createdKey, &appliedKey)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	created, err := sqliteEnqueueTime(createdKey)
	if err != nil {
		return nil, fmt.Errorf("revision created_at: %w", err)
	}
	if created == nil {
		return nil, errors.New("revision created_at is NULL")
	}
	rev.CreatedAt = *created
	if rev.AppliedAt, err = sqliteEnqueueTime(appliedKey); err != nil {
		return nil, fmt.Errorf("revision applied_at: %w", err)
	}
	if len(snapshot) > 0 {
		_ = json.Unmarshal(snapshot, &rev.Snapshot)
	}
	return &rev, nil
}

// sqliteListJobs mirrors listJobs (newest first), decoding clocks like enqueue.
func sqliteListJobs(ctx context.Context, db *sql.DB) ([]JobRecord, error) {
	rows, err := db.QueryContext(ctx, `select id,revision_id,actor_user_id,target,status,
		source,source_primary_id,source_node_id,source_revision_id,snapshot_content_hash,
		snapshot_version,replication_version,leadership_generation,correlation_id,
		created_at,claimed_at,started_at,finished_at,
		validation_output,apply_output,health_output,error_message
		from apply_jobs order by created_at desc`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	jobs := []JobRecord{}
	for rows.Next() {
		job, err := scanJob(sqliteEnqueueJobScanner{rows})
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}
