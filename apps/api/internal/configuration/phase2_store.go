package configuration

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zerkc/ProxyCore/apps/api/internal/cluster"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
)

// PgPhase2Store is the PostgreSQL implementation of the continuity
// transaction port. It intentionally exposes only typed operations; callers
// cannot bypass the row-lock/compare-and-set boundary with arbitrary SQL.
const EnrollmentTokenHashVersion = "sha256-v1"

type PgPhase2Store struct {
	pool            *pgxpool.Pool
	clusterKeyStore cluster.KeyStore
}

func NewPhase2Store(pool *pgxpool.Pool) *PgPhase2Store {
	return &PgPhase2Store{pool: pool}
}

func NewPhase2StoreWithClusterKeyStore(pool *pgxpool.Pool, keyStore cluster.KeyStore) *PgPhase2Store {
	return &PgPhase2Store{pool: pool, clusterKeyStore: keyStore}
}

func NewPhase2StoreWithMasterKey(pool *pgxpool.Pool, masterKeyBase64 string) *PgPhase2Store {
	return NewPhase2StoreWithClusterKeyStore(pool, cluster.NewStore(masterKeyBase64))
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
	if err := fn(&pgPhase2Transaction{tx: tx, clusterKeyStore: s.clusterKeyStore}); err != nil {
		return err
	}
	return tx.Commit(ctx)
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
	if err := fn(&pgPhase2Transaction{tx: tx, clusterKeyStore: s.clusterKeyStore}); err != nil {
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
			content_hash, revision_id, status, apply_job_id, failure_code, applied_at, discarded_at
		) values ($1, $2, $3, $4, $5, $6, $7, $8::proxycore_applied_snapshot_status, $9, $10, $11, $12)
		on conflict (id) do update set
			status = excluded.status,
			apply_job_id = excluded.apply_job_id,
			failure_code = excluded.failure_code,
			applied_at = excluded.applied_at,
			discarded_at = excluded.discarded_at
	`, snapshot.ID, snapshot.SourcePrimaryID, snapshot.LeadershipGeneration, snapshot.SnapshotVersion,
		snapshot.ReplicationVersion, snapshot.ContentHash, phase2String(snapshot.RevisionID), string(snapshot.Status),
		phase2String(snapshot.ApplyJobID), phase2String(snapshot.FailureCode), snapshot.AppliedAt, snapshot.DiscardedAt,
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
