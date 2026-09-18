package configuration

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zerkc/ProxyCore/apps/api/internal/cluster"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	securesecrets "github.com/zerkc/ProxyCore/apps/api/internal/secrets"
	replicationsnapshot "github.com/zerkc/ProxyCore/apps/api/internal/snapshot"
)

// PgPhase2Store is the PostgreSQL implementation of the continuity
// transaction port. It intentionally exposes only typed operations; callers
// cannot bypass the row-lock/compare-and-set boundary with arbitrary SQL.
const EnrollmentTokenHashVersion = "sha256-v1"

type PgPhase2Store struct {
	pool                         *pgxpool.Pool
	clusterKeyStore              cluster.KeyStore
	masterKeyBase64              string
	snapshotPublicationAfterAuth func()
}

func NewPhase2Store(pool *pgxpool.Pool) *PgPhase2Store {
	return &PgPhase2Store{pool: pool}
}

func NewPhase2StoreWithClusterKeyStore(pool *pgxpool.Pool, keyStore cluster.KeyStore) *PgPhase2Store {
	return &PgPhase2Store{pool: pool, clusterKeyStore: keyStore}
}

func NewPhase2StoreWithMasterKey(pool *pgxpool.Pool, masterKeyBase64 string) *PgPhase2Store {
	store := NewPhase2StoreWithClusterKeyStore(pool, cluster.NewStore(masterKeyBase64))
	store.masterKeyBase64 = masterKeyBase64
	return store
}

// WithSnapshotPublicationHooks returns a shallow store copy with test-only
// synchronization hooks. The hook executes while credential and node rows are
// locked, before identity or snapshot reads continue.
func (s *PgPhase2Store) WithSnapshotPublicationHooks(hooks SnapshotPublicationHooks) *PgPhase2Store {
	if s == nil {
		return nil
	}
	copy := *s
	copy.snapshotPublicationAfterAuth = hooks.AfterCredentialAuthorization
	return &copy
}

// GetEnrollmentHostnames returns the durable exact SAN configuration without
// exposing any certificate, CA, or private-key material.
func (s *Store) GetEnrollmentHostnames(ctx context.Context) (EnrollmentHostnameConfig, error) {
	if _, err := ensureSettings(ctx, s.pool); err != nil {
		return EnrollmentHostnameConfig{}, err
	}
	var raw []byte
	if err := s.pool.QueryRow(ctx,
		`select enrollment_hostnames from installation_settings where id = $1`, installationID,
	).Scan(&raw); err != nil {
		return EnrollmentHostnameConfig{}, err
	}
	if len(raw) == 0 || string(raw) == "null" {
		return EnrollmentHostnameConfig{Hostnames: []string{}}, nil
	}
	var hostnames []string
	if err := json.Unmarshal(raw, &hostnames); err != nil {
		return EnrollmentHostnameConfig{}, fmt.Errorf("read enrollment hostnames: %w", err)
	}
	canonical, err := NormalizeEnrollmentHostnames(hostnames)
	if err != nil {
		return EnrollmentHostnameConfig{}, fmt.Errorf("read enrollment hostnames: %w", err)
	}
	return EnrollmentHostnameConfig{Configured: len(canonical) > 0, Hostnames: canonical}, nil
}

// UpdateEnrollmentHostnames validates, canonicalizes, deduplicates, and
// durably stores exact enrollment DNS names and IP literals.
func (s *Store) UpdateEnrollmentHostnames(ctx context.Context, hostnames []string) (EnrollmentHostnameConfig, error) {
	canonical, err := NormalizeEnrollmentHostnames(hostnames)
	if err != nil {
		return EnrollmentHostnameConfig{}, err
	}
	if _, err := ensureSettings(ctx, s.pool); err != nil {
		return EnrollmentHostnameConfig{}, err
	}
	var payload any
	if len(canonical) > 0 {
		payload, err = json.Marshal(canonical)
		if err != nil {
			return EnrollmentHostnameConfig{}, err
		}
	}
	if _, err := s.pool.Exec(ctx, `
		update installation_settings
		set enrollment_hostnames = $2, updated_at = now()
		where id = $1
	`, installationID, payload); err != nil {
		return EnrollmentHostnameConfig{}, err
	}
	return EnrollmentHostnameConfig{Configured: len(canonical) > 0, Hostnames: canonical}, nil
}

func (s *PgPhase2Store) WithTransaction(ctx context.Context, fn func(Phase2Transaction) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := fn(&pgPhase2Transaction{tx: tx, clusterKeyStore: s.clusterKeyStore, masterKeyBase64: s.masterKeyBase64}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// WithCanonicalSnapshot owns the producer's atomic identity/job/revision/
// applied-snapshot transaction. The callback must not publish outside it.
func (s *PgPhase2Store) WithCanonicalSnapshot(ctx context.Context, fn func(CanonicalSnapshotTransaction) error) error {
	if s == nil || s.pool == nil || s.clusterKeyStore == nil || fn == nil {
		return ErrSnapshotPublicationStore
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := fn(&pgPhase2Transaction{tx: tx, clusterKeyStore: s.clusterKeyStore, masterKeyBase64: s.masterKeyBase64}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ListCanonicalSnapshotCandidates finds terminal ordinary combined jobs that
// do not yet have a non-null applied snapshot body. Legacy null-body rows stay
// candidates and are never treated as publishable.
func (s *PgPhase2Store) ListCanonicalSnapshotCandidates(ctx context.Context) ([]CanonicalSnapshotApplyRef, error) {
	if s == nil || s.pool == nil {
		return nil, ErrSnapshotPublicationStore
	}
	rows, err := s.pool.Query(ctx, `
		select j.id::text
		from apply_jobs j
		left join applied_snapshots a
			on a.apply_job_id = j.id
			and a.status::text = 'applied'
			and a.discarded_at is null
			and a.snapshot_body is not null
		where j.status::text = 'applied'
		  and j.target::text = 'combined'
		  and j.source::text = 'ordinary'
		  and j.finished_at is not null
		  and a.id is null
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
	return candidates, rows.Err()
}

func (s *PgPhase2Store) WithPrimaryGrant(ctx context.Context, fn func(PrimaryGrantTransaction) error) error {
	if s == nil || s.pool == nil || fn == nil {
		return ErrEnrollmentGrantStore
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := fn(&pgPhase2Transaction{tx: tx, clusterKeyStore: s.clusterKeyStore, masterKeyBase64: s.masterKeyBase64}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *PgPhase2Store) tokenTransaction(ctx context.Context, fn func(*pgPhase2Transaction) error) error {
	return s.WithTransaction(ctx, func(tx Phase2Transaction) error { return fn(tx.(*pgPhase2Transaction)) })
}

func (s *PgPhase2Store) CreateEnrollmentToken(ctx context.Context, id, selector, hash, version, ownerID string, createdAt, expiresAt time.Time) (domain.TopologyRole, error) {
	var role domain.TopologyRole
	err := s.WithTransaction(ctx, func(tx Phase2Transaction) error {
		var err error
		role, err = tx.(*pgPhase2Transaction).CreateEnrollmentToken(ctx, id, selector, hash, version, ownerID, createdAt, expiresAt)
		return err
	})
	return role, err
}
func (s *PgPhase2Store) CheckEnrollmentToken(ctx context.Context, selector, hash string, now time.Time, consume bool) error {
	return s.tokenTransaction(ctx, func(tx *pgPhase2Transaction) error { return tx.CheckEnrollmentToken(ctx, selector, hash, now, consume) })
}
func (s *PgPhase2Store) RevokeEnrollmentToken(ctx context.Context, id, ownerID string, now time.Time) error {
	return s.tokenTransaction(ctx, func(tx *pgPhase2Transaction) error { return tx.RevokeEnrollmentToken(ctx, id, ownerID, now) })
}

type pgPhase2Transaction struct {
	tx              pgx.Tx
	clusterKeyStore cluster.KeyStore
	masterKeyBase64 string
}

var errEnrollmentTokenDenied = errors.New("enrollment token denied")

func (t *pgPhase2Transaction) CreateEnrollmentToken(ctx context.Context, id, selector, hash, version, ownerID string, createdAt, expiresAt time.Time) (domain.TopologyRole, error) {
	if version != EnrollmentTokenHashVersion {
		return "", errEnrollmentTokenDenied
	}
	var role string
	err := t.tx.QueryRow(ctx, `with eligible as (
		update installation_identity set role = case when role = 'standalone-primary' then 'primary' else role end, updated_at = $7
		where id = $1 and role in ('standalone-primary', 'primary', 'primary-with-nodes')
		and leadership_generation >= latest_known_generation returning role
	), inserted as (
		insert into enrollment_tokens (id, token_selector, token_hash, hash_version, created_by_user_id, created_at, expires_at)
		select $2, $3, $4, $5, $6, $7, $8 from eligible returning 1
	) select role::text from eligible cross join inserted`, installationID, id, selector, hash, version, ownerID, createdAt, expiresAt).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", errEnrollmentTokenDenied
	}
	return domain.TopologyRole(role), err
}

func (t *pgPhase2Transaction) CheckEnrollmentToken(ctx context.Context, selector, hash string, now time.Time, consume bool) error {
	var id, storedHash, version string
	var expiresAt time.Time
	var consumedAt, revokedAt *time.Time
	if err := t.tx.QueryRow(ctx, `select id::text, token_hash, hash_version, expires_at, consumed_at, revoked_at from enrollment_tokens where token_selector = $1 for update`, selector).Scan(&id, &storedHash, &version, &expiresAt, &consumedAt, &revokedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return errEnrollmentTokenDenied
		}
		return err
	}
	hashMatches := subtle.ConstantTimeCompare([]byte(storedHash), []byte(hash)) == 1
	if version != EnrollmentTokenHashVersion || !hashMatches || !expiresAt.After(now) || consumedAt != nil || revokedAt != nil {
		return errEnrollmentTokenDenied
	}
	if !consume {
		return nil
	}
	result, err := t.tx.Exec(ctx, `update enrollment_tokens set consumed_at = $2 where id = $1 and consumed_at is null`, id, now)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return errEnrollmentTokenDenied
	}
	return nil
}

func (t *pgPhase2Transaction) RevokeEnrollmentToken(ctx context.Context, id, ownerID string, now time.Time) error {
	_, err := t.tx.Exec(ctx, `update enrollment_tokens set revoked_at = coalesce(revoked_at, $3) where id = $1 and created_by_user_id = $2`, id, ownerID, now)
	return err
}

func (t *pgPhase2Transaction) LoadNodeState(ctx context.Context) (NodeStateRecord, error) {
	return loadNodeState(ctx, t.tx)
}

func (t *pgPhase2Transaction) SaveNodeState(ctx context.Context, state NodeStateRecord) error {
	if state.ID == "" {
		state.ID = installationID
	}
	_, err := t.tx.Exec(ctx, `
		insert into node_state (
			id, enrolled_at, enrollment_primary_id, last_seen_at, last_applied_snapshot_id,
			enrollment_attempt_id, primary_url, primary_installation_id,
			primary_tls_spki_sha256, credential_id, sync_enabled, last_attempt_at,
			last_success_at, consecutive_failures, next_attempt_at, last_error_code, updated_at
		) values (
			$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, now()
		)
		on conflict (id) do update set
			enrolled_at = excluded.enrolled_at,
			enrollment_primary_id = excluded.enrollment_primary_id,
			last_seen_at = excluded.last_seen_at,
			last_applied_snapshot_id = excluded.last_applied_snapshot_id,
			enrollment_attempt_id = excluded.enrollment_attempt_id,
			primary_url = excluded.primary_url,
			primary_installation_id = excluded.primary_installation_id,
			primary_tls_spki_sha256 = excluded.primary_tls_spki_sha256,
			credential_id = excluded.credential_id,
			sync_enabled = excluded.sync_enabled,
			last_attempt_at = excluded.last_attempt_at,
			last_success_at = excluded.last_success_at,
			consecutive_failures = excluded.consecutive_failures,
			next_attempt_at = excluded.next_attempt_at,
			last_error_code = excluded.last_error_code,
			updated_at = now()
	`, state.ID, state.EnrolledAt, phase2String(state.EnrollmentPrimaryID), state.LastSeenAt,
		phase2String(state.LastAppliedSnapshotID), phase2String(state.EnrollmentAttemptID),
		phase2String(state.PrimaryURL), phase2String(state.PrimaryInstallationID),
		phase2String(state.PrimaryTLSSPKISHA256), phase2String(state.CredentialID), state.SyncEnabled,
		state.LastAttemptAt, state.LastSuccessAt, state.ConsecutiveFailures, state.NextAttemptAt,
		phase2String(state.LastErrorCode),
	)
	return err
}

func (t *pgPhase2Transaction) CreateEnrollmentAttempt(ctx context.Context, attempt EnrollmentAttemptRecord) error {
	if attempt.ID == "" {
		attempt.ID = newUUID()
	}
	if !attempt.State.IsValid() {
		return fmt.Errorf("invalid enrollment attempt state: %s", attempt.State)
	}
	_, err := t.tx.Exec(ctx, `
		insert into enrollment_attempts (
			id, state, primary_url, expected_primary_id, verified_primary_id, verified_primary_node_id,
			verified_leadership_generation, verified_primary_tls_spki_sha256, verified_primary_ca_fingerprint,
			preview_digest, local_node_ip, archive_id, ephemeral_private_key_wrapped, bootstrap_payload,
			node_credential_secret_id, cluster_key_id, initial_snapshot_hash, initial_snapshot_revision_id,
			initial_apply_job_id, failure_code, confirmed_at, created_at, updated_at
		) values (
			$1, $2::proxycore_enrollment_attempt_state, $3, $4, $5, $6, $7, $8, $9, $10, $11,
			$12, $13, $14, $15, $16, $17, $18, $19, $20, $21, now(), now()
		)
	`, attempt.ID, string(attempt.State), attempt.PrimaryURL, phase2String(attempt.ExpectedPrimaryID),
		phase2String(attempt.VerifiedPrimaryID), phase2String(attempt.VerifiedPrimaryNodeID),
		phase2Int64(attempt.VerifiedLeadershipGeneration), phase2String(attempt.VerifiedPrimaryTLSSPKISHA256),
		phase2String(attempt.VerifiedPrimaryCAFingerprint), phase2String(attempt.PreviewDigest), attempt.LocalNodeIP,
		phase2String(attempt.ArchiveID), attempt.EphemeralPrivateKeyWrapped, phase2Bytes(attempt.BootstrapPayload),
		phase2String(attempt.NodeCredentialSecretID), phase2String(attempt.ClusterKeyID),
		phase2String(attempt.InitialSnapshotHash), phase2String(attempt.InitialSnapshotRevisionID),
		phase2String(attempt.InitialApplyJobID), phase2String(attempt.FailureCode), attempt.ConfirmedAt,
	)
	return err
}

func (t *pgPhase2Transaction) TransitionEnrollmentAttempt(ctx context.Context, id string, from, to EnrollmentAttemptState) error {
	if !from.IsValid() || !to.IsValid() {
		return errors.New("invalid enrollment attempt state")
	}
	result, err := t.tx.Exec(ctx, `
		update enrollment_attempts
		set state = $3::proxycore_enrollment_attempt_state, updated_at = now()
		where id = $1 and state = $2::proxycore_enrollment_attempt_state
	`, id, string(from), string(to))
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		return errors.New("enrollment attempt state conflict")
	}
	return nil
}

func (t *pgPhase2Transaction) CreateSyncAttempt(ctx context.Context, attempt SyncAttemptRecord) error {
	if attempt.ID == "" {
		attempt.ID = newUUID()
	}
	if !attempt.Trigger.IsValid() || !attempt.Status.IsValid() {
		return errors.New("invalid sync attempt contract value")
	}
	_, err := t.tx.Exec(ctx, `
		insert into sync_attempts (
			id, node_id, trigger, status, source_primary_id, leadership_generation,
			snapshot_version, replication_version, content_hash, revision_id, apply_job_id,
			result_code, started_at, finished_at, created_at, updated_at
		) values (
			$1, $2, $3::proxycore_sync_trigger, $4::proxycore_sync_attempt_status, $5, $6,
			$7, $8, $9, $10, $11, $12, $13, $14, now(), now()
		)
	`, attempt.ID, attempt.NodeID, string(attempt.Trigger), string(attempt.Status),
		phase2String(attempt.SourcePrimaryID), phase2Int64(attempt.LeadershipGeneration),
		phase2Int(attempt.SnapshotVersion), phase2Int(attempt.ReplicationVersion),
		phase2String(attempt.ContentHash), phase2String(attempt.RevisionID), phase2String(attempt.ApplyJobID),
		phase2String(attempt.ResultCode), attempt.StartedAt, attempt.FinishedAt,
	)
	return err
}

func (t *pgPhase2Transaction) RecordAppliedSnapshot(ctx context.Context, snapshot AppliedSnapshotRecord) error {
	if snapshot.ID == "" {
		snapshot.ID = newUUID()
	}
	if !snapshot.Status.IsValid() {
		return fmt.Errorf("invalid applied snapshot status: %s", snapshot.Status)
	}
	_, err := t.tx.Exec(ctx, `
		insert into applied_snapshots (
			id, source_primary_id, leadership_generation, snapshot_version, replication_version,
			content_hash, snapshot_body, revision_id, status, apply_job_id, failure_code, applied_at, discarded_at
		) values ($1, $2, $3, $4, $5, $6, $7, $8, $9::proxycore_applied_snapshot_status, $10, $11, $12, $13)
		on conflict (id) do update set
			status = excluded.status,
			apply_job_id = excluded.apply_job_id,
			failure_code = excluded.failure_code,
			applied_at = excluded.applied_at,
			discarded_at = excluded.discarded_at
	`, snapshot.ID, snapshot.SourcePrimaryID, snapshot.LeadershipGeneration, snapshot.SnapshotVersion,
		snapshot.ReplicationVersion, snapshot.ContentHash, snapshot.SnapshotBody,
		phase2String(snapshot.RevisionID), string(snapshot.Status), phase2String(snapshot.ApplyJobID),
		phase2String(snapshot.FailureCode), snapshot.AppliedAt, snapshot.DiscardedAt,
	)
	return err
}

func (t *pgPhase2Transaction) RecordSnapshotAcknowledgement(ctx context.Context, ack SnapshotAcknowledgement) error {
	_, err := t.tx.Exec(ctx, `
		insert into node_snapshot_acks (
			node_id, content_hash, snapshot_version, replication_version, revision_id,
			leadership_generation, applied_at, received_at
		) values ($1, $2, $3, $4, $5, $6, $7, $8)
		on conflict (node_id, content_hash) do update set
			snapshot_version = excluded.snapshot_version,
			replication_version = excluded.replication_version,
			revision_id = excluded.revision_id,
			leadership_generation = excluded.leadership_generation,
			applied_at = excluded.applied_at,
			received_at = excluded.received_at
	`, ack.NodeID, ack.ContentHash, ack.SnapshotVersion, ack.ReplicationVersion, ack.RevisionID,
		ack.LeadershipGeneration, ack.AppliedAt, ack.ReceivedAt)
	return err
}

func (t *pgPhase2Transaction) GetCanonicalSnapshotApply(ctx context.Context, id string) (CanonicalSnapshotApply, error) {
	var (
		apply                     CanonicalSnapshotApply
		revisionSource, jobSource string
		desiredSnapshot           []byte
	)
	err := t.tx.QueryRow(ctx, `
		select j.id::text, j.revision_id::text, r.revision_number, r.snapshot,
			j.status::text, j.target::text, j.source::text,
			j.source_primary_id::text, j.source_node_id::text, j.source_revision_id::text,
			j.snapshot_content_hash, j.snapshot_version, j.replication_version,
			j.leadership_generation, j.finished_at,
			r.source::text, r.source_primary_id::text, r.source_node_id::text,
			r.source_revision_id::text, r.snapshot_content_hash, r.snapshot_version,
			r.replication_version, r.leadership_generation, r.applied_at
		from apply_jobs j
		join config_revisions r on r.id = j.revision_id
		where j.id = $1
		for update of j, r
	`, id).Scan(
		&apply.JobID, &apply.RevisionID, &apply.RevisionNumber, &desiredSnapshot,
		&apply.Status, &apply.Target, &jobSource,
		&apply.SourcePrimaryID, &apply.SourceNodeID, &apply.SourceRevisionID,
		&apply.SnapshotContentHash, &apply.SnapshotVersion, &apply.ReplicationVersion,
		&apply.LeadershipGeneration, &apply.FinishedAt,
		&revisionSource, &apply.RevisionSourcePrimaryID, &apply.RevisionSourceNodeID,
		&apply.RevisionSourceRevisionID, &apply.RevisionSnapshotHash,
		&apply.RevisionSnapshotVersion, &apply.RevisionReplication,
		&apply.RevisionGeneration, &apply.RevisionAppliedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return CanonicalSnapshotApply{}, ErrCanonicalSnapshotNotFound
	}
	if err != nil {
		return CanonicalSnapshotApply{}, err
	}
	apply.DesiredSnapshot = append([]byte(nil), desiredSnapshot...)
	apply.Source = PersistenceSource(jobSource)
	apply.RevisionSource = PersistenceSource(revisionSource)
	return apply, nil
}

func (t *pgPhase2Transaction) FindCanonicalSnapshotForJob(ctx context.Context, jobID string) (*CanonicalSnapshotRecord, error) {
	row := t.tx.QueryRow(ctx, `
		select id::text, source_primary_id::text, leadership_generation,
			snapshot_version, replication_version, content_hash, snapshot_body,
			revision_id::text, status::text, apply_job_id::text, failure_code,
			applied_at, discarded_at
		from applied_snapshots
		where apply_job_id = $1
		order by applied_at desc, id desc
		limit 1
		for update
	`, jobID)
	record, err := scanAppliedSnapshot(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &CanonicalSnapshotRecord{AppliedSnapshotRecord: record}, nil
}

func (t *pgPhase2Transaction) ListSecrets(ctx context.Context) ([]replicationsnapshot.PlainSecret, error) {
	rows, err := t.tx.Query(ctx, `
		select id, purpose, ciphertext
		from secrets
		order by id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]replicationsnapshot.PlainSecret, 0)
	for rows.Next() {
		var id uuid.UUID
		var purpose, ciphertext string
		if err := rows.Scan(&id, &purpose, &ciphertext); err != nil {
			zeroCanonicalPlainSecrets(result)
			return nil, err
		}
		plaintext, err := securesecrets.DecryptSecret(ciphertext, t.masterKeyBase64)
		if err != nil {
			zeroCanonicalPlainSecrets(result)
			return nil, err
		}
		result = append(result, replicationsnapshot.PlainSecret{
			ID: id, Purpose: purpose, Value: []byte(plaintext),
		})
	}
	if err := rows.Err(); err != nil {
		zeroCanonicalPlainSecrets(result)
		return nil, err
	}
	return result, nil
}

func (t *pgPhase2Transaction) ListReplicableOwners(ctx context.Context) ([]replicationsnapshot.ReplicableOwner, error) {
	rows, err := t.tx.Query(ctx, `
		select id, username, password_hash, role::text
		from users
		order by id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]replicationsnapshot.ReplicableOwner, 0)
	for rows.Next() {
		var owner replicationsnapshot.ReplicableOwner
		if err := rows.Scan(&owner.UserID, &owner.Username, &owner.PasswordHash, &owner.Role); err != nil {
			return nil, err
		}
		result = append(result, owner)
	}
	return result, rows.Err()
}

func (t *pgPhase2Transaction) PublishCanonicalSnapshot(ctx context.Context, write CanonicalSnapshotWrite) error {
	if t == nil || len(write.SnapshotBody) == 0 || !validPublicationUUID(write.ID) ||
		!validPublicationUUID(write.SourcePrimaryID) || !validPublicationUUID(write.RevisionID) ||
		!validPublicationUUID(write.ApplyJobID) || write.LeadershipGeneration <= 0 ||
		write.SnapshotVersion <= 0 || write.ReplicationVersion <= 0 || write.ContentHash == "" ||
		write.AppliedAt.IsZero() {
		return ErrCanonicalSnapshotConflict
	}
	if existing, err := t.findCanonicalSnapshotByHash(ctx, write.ContentHash); err != nil {
		return err
	} else if existing != nil {
		if canonicalSnapshotMatches(existing.AppliedSnapshotRecord, write) {
			return nil
		}
		return ErrCanonicalSnapshotConflict
	}

	revisionTag, err := t.tx.Exec(ctx, `
		update config_revisions
		set source = 'ordinary', source_primary_id = $2, source_node_id = null,
			source_revision_id = null, snapshot_content_hash = $3,
			snapshot_version = $4, replication_version = $5,
			leadership_generation = $6
		where id = $1 and source = 'ordinary' and applied_at is not null
	`, write.RevisionID, write.SourcePrimaryID, write.ContentHash, write.SnapshotVersion,
		write.ReplicationVersion, write.LeadershipGeneration)
	if err != nil || revisionTag.RowsAffected() != 1 {
		if err != nil {
			return err
		}
		return ErrCanonicalSnapshotConflict
	}
	jobTag, err := t.tx.Exec(ctx, `
		update apply_jobs
		set source = 'ordinary', source_primary_id = $2, source_node_id = null,
			source_revision_id = null, snapshot_content_hash = $3,
			snapshot_version = $4, replication_version = $5,
			leadership_generation = $6, target = 'combined'
		where id = $1 and revision_id = $7 and status = 'applied' and finished_at is not null
	`, write.ApplyJobID, write.SourcePrimaryID, write.ContentHash, write.SnapshotVersion,
		write.ReplicationVersion, write.LeadershipGeneration, write.RevisionID)
	if err != nil || jobTag.RowsAffected() != 1 {
		if err != nil {
			return err
		}
		return ErrCanonicalSnapshotConflict
	}
	_, err = t.tx.Exec(ctx, `
		insert into applied_snapshots (
			id, source_primary_id, leadership_generation, snapshot_version,
			replication_version, content_hash, snapshot_body, revision_id,
			status, apply_job_id, failure_code, applied_at, discarded_at
		) values ($1, $2, $3, $4, $5, $6, $7, $8, 'applied', $9, null, $10, null)
	`, write.ID, write.SourcePrimaryID, write.LeadershipGeneration, write.SnapshotVersion,
		write.ReplicationVersion, write.ContentHash, write.SnapshotBody, write.RevisionID,
		write.ApplyJobID, write.AppliedAt)
	if err != nil {
		if existing, lookupErr := t.findCanonicalSnapshotByHash(ctx, write.ContentHash); lookupErr == nil &&
			existing != nil && canonicalSnapshotMatches(existing.AppliedSnapshotRecord, write) {
			return nil
		}
		return err
	}
	return nil
}

func (t *pgPhase2Transaction) findCanonicalSnapshotByHash(ctx context.Context, contentHash string) (*CanonicalSnapshotRecord, error) {
	row := t.tx.QueryRow(ctx, `
		select id::text, source_primary_id::text, leadership_generation,
			snapshot_version, replication_version, content_hash, snapshot_body,
			revision_id::text, status::text, apply_job_id::text, failure_code,
			applied_at, discarded_at
		from applied_snapshots
		where content_hash = $1
		for update
	`, contentHash)
	record, err := scanAppliedSnapshot(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &CanonicalSnapshotRecord{AppliedSnapshotRecord: record}, nil
}

func canonicalSnapshotMatches(record AppliedSnapshotRecord, write CanonicalSnapshotWrite) bool {
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

func zeroCanonicalPlainSecrets(values []replicationsnapshot.PlainSecret) {
	for index := range values {
		for byteIndex := range values[index].Value {
			values[index].Value[byteIndex] = 0
		}
		values[index].Value = nil
	}
}

func (t *pgPhase2Transaction) GetApplyJob(ctx context.Context, id string) (JobRecord, error) {
	row := t.tx.QueryRow(ctx, `
		select id::text, revision_id::text, actor_user_id::text, target::text, status::text,
			source::text, source_primary_id::text, source_node_id::text, source_revision_id::text,
			snapshot_content_hash, snapshot_version, replication_version, leadership_generation,
			correlation_id, created_at, claimed_at, started_at, finished_at,
			validation_output, apply_output, health_output, error_message
		from apply_jobs where id = $1
	`, id)
	job, err := scanJob(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return JobRecord{}, errors.New("apply job not found")
	}
	return job, err
}

func phase2String(value *string) any {
	if value == nil || *value == "" {
		return nil
	}
	return *value
}

func phase2Bytes(value []byte) any {
	if len(value) == 0 {
		return nil
	}
	return string(value)
}

func phase2Int(value *int) any {
	if value == nil {
		return nil
	}
	return *value
}

func phase2Int64(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

// ReadSnapshotPublication authenticates the presented node credential and
// selects the absolute newest relevant candidate in one repeatable-read
// transaction. It denies an incomplete or unpublishable newest candidate
// rather than falling back to an older revision. Credential and enrolled-node
// row locks are held through the bounded body copy and commit. A revocation
// committed after that commit is observed on the next call; transports must
// call this method before writing any response bytes.
func (s *PgPhase2Store) ReadSnapshotPublication(ctx context.Context, request SnapshotPublicationRequest) (SnapshotPublicationResult, error) {
	if s == nil || s.pool == nil || request.Authenticate == nil ||
		request.CredentialID == "" || request.PresentedCredential == "" ||
		request.MaxBytes <= 0 || request.MaxBytes > SnapshotPublicationMaxBytes {
		return SnapshotPublicationResult{}, ErrSnapshotPublicationStore
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return SnapshotPublicationResult{}, ErrSnapshotPublicationStore
	}
	defer tx.Rollback(ctx)

	credential, err := lockSnapshotPublicationCredential(ctx, tx, request)
	if err != nil {
		return SnapshotPublicationResult{}, ErrSnapshotPublicationStore
	}
	principal, err := request.Authenticate(request.PresentedCredential, credential)
	if err != nil || principal.CredentialID != credential.ID || principal.NodeID != credential.NodeID {
		return SnapshotPublicationResult{}, ErrSnapshotPublicationStore
	}
	if s.snapshotPublicationAfterAuth != nil {
		s.snapshotPublicationAfterAuth()
	}
	if request.AfterContentHash != "" && !validSnapshotPublicationHash(request.AfterContentHash) {
		return SnapshotPublicationResult{}, ErrSnapshotPublicationStore
	}

	identityRecord, err := lockSnapshotPublicationIdentity(ctx, tx)
	if err != nil || !readySnapshotPublicationIdentity(identityRecord) {
		return SnapshotPublicationResult{}, ErrSnapshotPublicationStore
	}
	if credential.PrimaryID != identityRecord.InstallationID {
		return SnapshotPublicationResult{}, ErrSnapshotPublicationStore
	}

	if identityRecord.ClusterKeyID == nil || s.clusterKeyStore == nil {
		return SnapshotPublicationResult{}, ErrSnapshotPublicationStore
	}
	keyMaterial, err := s.clusterKeyStore.LoadOrCreate(ctx, tx, identityRecord.ClusterKeyID)
	if err != nil {
		return SnapshotPublicationResult{}, ErrSnapshotPublicationStore
	}
	keyBytes := keyMaterial.CopyBytes()
	keyMaterial.Destroy()
	kek, err := cluster.NewKEK(keyBytes)
	if err != nil {
		zeroGrantBytes(keyBytes)
		return SnapshotPublicationResult{}, ErrSnapshotPublicationStore
	}
	defer kek.Destroy()
	zeroGrantBytes(keyBytes)
	identityRecord.ClusterKeyUsable = true

	// The metadata query computes octet_length(snapshot::text) without selecting
	// the body. PostgreSQL stores this column as JSONB, so the text cast is only
	// for its logical byte length; the second query fetches bytes after proof and
	// max-size checks pass.
	candidate, snapshotSize, err := selectLatestSnapshotPublication(ctx, tx, identityRecord, credential.PrimaryID)
	if err != nil || snapshotSize <= 0 || snapshotSize > request.MaxBytes || !candidate.ProofComplete {
		return SnapshotPublicationResult{}, ErrSnapshotPublicationStore
	}
	result := SnapshotPublicationResult{
		Principal: principal,
		Identity:  identityRecord,
		Snapshot:  candidate,
	}

	var raw []byte
	if err := tx.QueryRow(ctx, `
		select snapshot_body from applied_snapshots
		where id = $1
		for share
	`, candidate.SnapshotID).Scan(&raw); err != nil || len(raw) == 0 || len(raw) != snapshotSize || len(raw) > request.MaxBytes {
		return SnapshotPublicationResult{}, ErrSnapshotPublicationStore
	}
	rawBytes := []byte(raw)
	envelope, err := replicationsnapshot.Unmarshal(rawBytes)
	if err != nil || !replicationsnapshot.NewValidator(kek).Validate(ctx, envelope).OK {
		return SnapshotPublicationResult{}, ErrSnapshotPublicationStore
	}
	candidate.Bytes = append([]byte(nil), rawBytes...)
	result.Snapshot = candidate
	if request.AfterContentHash != "" && request.AfterContentHash == candidate.ContentHash {
		result.Current = true
		result.Snapshot.Bytes = nil
	}
	if err := tx.Commit(ctx); err != nil {
		return SnapshotPublicationResult{}, ErrSnapshotPublicationStore
	}
	return result, nil
}

func lockSnapshotPublicationCredential(ctx context.Context, tx pgx.Tx, request SnapshotPublicationRequest) (SnapshotPublicationCredentialRecord, error) {
	var record SnapshotPublicationCredentialRecord
	if err := tx.QueryRow(ctx, `
		select c.id::text, c.node_id::text, c.credential_hash, c.hash_version,
			c.revoked_at, n.revoked_at, n.primary_id::text
		from node_credentials c
		join enrolled_nodes n on n.credential_id = c.id and n.node_id = c.node_id
		where c.id = $1
		for update of c, n
	`, request.CredentialID).Scan(
		&record.ID, &record.NodeID, &record.CredentialHash, &record.HashVersion,
		&record.CredentialRevokedAt, &record.NodeRevokedAt, &record.PrimaryID,
	); err != nil {
		return SnapshotPublicationCredentialRecord{}, err
	}
	return record, nil
}

func lockSnapshotPublicationIdentity(ctx context.Context, tx pgx.Tx) (SnapshotPublicationIdentityRecord, error) {
	var record SnapshotPublicationIdentityRecord
	var role string
	var installationID, nodeID string
	if err := tx.QueryRow(ctx, `
		select installation_id::text, node_id::text, role::text,
			leadership_generation, latest_known_generation, cluster_key_id
		from installation_identity
		where id = $1
		for update
	`, installationIDSingleton).Scan(
		&installationID, &nodeID, &role, &record.LeadershipGeneration,
		&record.LatestKnownGeneration, &record.ClusterKeyID,
	); err != nil {
		return SnapshotPublicationIdentityRecord{}, err
	}
	record.InstallationID = installationID
	record.NodeID = nodeID
	record.Role = domain.TopologyRole(role)
	return record, nil
}

func readySnapshotPublicationIdentity(record SnapshotPublicationIdentityRecord) bool {
	return validPublicationUUID(record.InstallationID) && validPublicationUUID(record.NodeID) &&
		(record.Role == domain.TopologyRolePrimary || record.Role == domain.TopologyRolePrimaryWithNodes) &&
		record.LeadershipGeneration > 0 && record.LeadershipGeneration == record.LatestKnownGeneration
}

func selectLatestSnapshotPublication(ctx context.Context, tx pgx.Tx, _ SnapshotPublicationIdentityRecord, primaryID string) (SnapshotPublicationRecord, int, error) {
	var record SnapshotPublicationRecord
	var snapshotSize int
	var proofComplete bool
	if err := tx.QueryRow(ctx, `
		select a.id::text, a.source_primary_id::text, a.leadership_generation,
			a.snapshot_version, a.replication_version, a.content_hash,
			coalesce(r.id::text, ''), coalesce(j.id::text, ''), a.applied_at, a.discarded_at,
			coalesce(r.revision_number, 0), coalesce(octet_length(a.snapshot_body), 0),
			(
				a.status::text = 'applied'
				and a.snapshot_body is not null
				and a.discarded_at is null
				and r.id is not null
				and r.applied_at is not null
				and r.source::text = 'ordinary'
				and r.source_node_id is null
				and r.source_revision_id is null
				and r.source_primary_id = a.source_primary_id
				and r.snapshot_content_hash = a.content_hash
				and r.snapshot_version = a.snapshot_version
				and r.replication_version = a.replication_version
				and r.leadership_generation = a.leadership_generation
				and j.id is not null
				and j.revision_id = r.id
				and j.status::text = 'applied'
				and j.finished_at is not null
				and j.source::text = 'ordinary'
				and j.source_node_id is null
				and j.source_revision_id is null
				and j.source_primary_id = a.source_primary_id
				and j.snapshot_content_hash = a.content_hash
				and j.snapshot_version = a.snapshot_version
				and j.replication_version = a.replication_version
				and j.leadership_generation = a.leadership_generation
				and j.target::text = 'combined'
			) as proof_complete
		from applied_snapshots a
		left join config_revisions r on r.id = a.revision_id
		left join apply_jobs j on j.id = a.apply_job_id
		where a.source_primary_id = $1
		-- revision_number is unique in config_revisions; created_at and id
		-- deterministically order incomplete candidates without a revision row.
		order by coalesce(r.revision_number, -1) desc,
			coalesce(r.created_at, a.applied_at) desc,
			a.id desc
		limit 1
	`, primaryID).Scan(
		&record.SnapshotID, &record.SourcePrimaryID, &record.LeadershipGeneration,
		&record.SnapshotVersion, &record.ReplicationVersion, &record.ContentHash,
		&record.RevisionID, &record.ApplyJobID, &record.AppliedAt, &record.DiscardedAt,
		&record.RevisionNumber, &snapshotSize, &proofComplete,
	); err != nil {
		return SnapshotPublicationRecord{}, 0, err
	}
	record.ProofComplete = proofComplete
	return record, snapshotSize, nil
}

func validPublicationUUID(value string) bool {
	parsed, err := uuid.Parse(value)
	return err == nil && parsed != uuid.Nil && parsed.String() == value
}
