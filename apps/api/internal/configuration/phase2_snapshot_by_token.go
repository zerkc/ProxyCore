package configuration

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
)

var (
	ErrSnapshotByTokenUnauthenticated = errors.New("snapshot by token unauthenticated")
	ErrSnapshotByTokenGone            = errors.New("snapshot by token gone")
	ErrSnapshotByTokenDenied          = errors.New("snapshot by token denied")
	ErrSnapshotByTokenUnavailable     = errors.New("snapshot by token unavailable")
	ErrNoPublishableSnapshot          = errors.New("snapshot by token has no publishable snapshot")
)

// ReadSnapshotPublicationByToken reads one canonical PRIMARY publication while
// holding the enrollment-token row lock. The caller must authenticate the full
// pcenr1 credential before calling this method: only its canonical selector is
// accepted here, so the token_hash is selected for the locked lifecycle
// projection but cannot be compared without the secret portion.
func (s *PgPhase2Store) ReadSnapshotPublicationByToken(
	ctx context.Context,
	selector string,
	identity SnapshotPublicationIdentityRecord,
) ([]byte, error) {
	if s == nil || s.pool == nil || ctx == nil {
		return nil, ErrSnapshotByTokenUnavailable
	}
	if selector == "" {
		return nil, ErrSnapshotByTokenUnauthenticated
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, ErrSnapshotByTokenUnavailable
	}
	defer tx.Rollback(ctx)

	var tokenID uuid.UUID
	var tokenHash, hashVersion string
	var expiresAt time.Time
	var consumedAt, revokedAt *time.Time
	if err := tx.QueryRow(ctx, `
		select id, token_hash, hash_version, expires_at, consumed_at, revoked_at
		from enrollment_tokens
		where token_selector = $1
		for update
	`, selector).Scan(&tokenID, &tokenHash, &hashVersion, &expiresAt, &consumedAt, &revokedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrSnapshotByTokenUnauthenticated
		}
		return nil, ErrSnapshotByTokenUnavailable
	}
	_ = tokenID
	_ = tokenHash

	now := time.Now().UTC()
	if hashVersion != EnrollmentTokenHashVersion || !expiresAt.After(now) {
		return nil, ErrSnapshotByTokenUnauthenticated
	}
	if consumedAt != nil || revokedAt != nil {
		return nil, ErrSnapshotByTokenGone
	}
	if !validSnapshotByTokenIdentity(identity) {
		return nil, ErrSnapshotByTokenDenied
	}
	if s.clusterKeyStore == nil {
		return nil, ErrSnapshotByTokenUnavailable
	}

	var activeKeyID uuid.UUID
	if err := tx.QueryRow(ctx, `
		select id
		from cluster_keys
		where retired_at is null
		order by created_at asc, id asc
		limit 1
		for update
	`).Scan(&activeKeyID); err != nil {
		return nil, ErrSnapshotByTokenUnavailable
	}
	material, err := s.clusterKeyStore.LoadOrCreate(ctx, tx, &activeKeyID)
	if err != nil {
		return nil, ErrSnapshotByTokenUnavailable
	}
	materialID := material.ID
	keyBytes := material.CopyBytes()
	material.Destroy()
	keyUsable := len(keyBytes) > 0
	zeroGrantBytes(keyBytes)
	if !keyUsable {
		return nil, ErrSnapshotByTokenUnavailable
	}
	if identity.ClusterKeyID == nil || materialID != *identity.ClusterKeyID {
		return nil, ErrSnapshotByTokenDenied
	}

	var body []byte
	var proofComplete bool
	if err := tx.QueryRow(ctx, `
		select a.snapshot_body,
			coalesce((
				a.status::text = 'applied'
				and a.snapshot_body is not null
				and a.discarded_at is null
				and a.revision_id is not null
				and a.applied_at is not null
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
			), false) as proof_complete
		from applied_snapshots a
		left join config_revisions r on r.id = a.revision_id
		left join apply_jobs j on j.id = a.apply_job_id
		where a.source_primary_id = $1
		  and a.leadership_generation = $2
		order by a.applied_at desc, a.id desc
		limit 1
		for share of a
	`, identity.InstallationID, identity.LeadershipGeneration).Scan(&body, &proofComplete); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNoPublishableSnapshot
		}
		return nil, ErrSnapshotByTokenUnavailable
	}
	if !proofComplete || len(body) == 0 {
		return nil, ErrNoPublishableSnapshot
	}

	result := append([]byte(nil), body...)
	if err := tx.Commit(ctx); err != nil {
		return nil, ErrSnapshotByTokenUnavailable
	}
	return result, nil
}

func validSnapshotByTokenIdentity(identity SnapshotPublicationIdentityRecord) bool {
	return validPublicationUUID(identity.InstallationID) &&
		validPublicationUUID(identity.NodeID) &&
		(identity.Role == domain.TopologyRolePrimary || identity.Role == domain.TopologyRolePrimaryWithNodes) &&
		identity.LeadershipGeneration > 0 &&
		identity.LeadershipGeneration == identity.LatestKnownGeneration &&
		identity.ClusterKeyID != nil && *identity.ClusterKeyID != uuid.Nil
}
