package configuration

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
)

var ErrEnrollmentGrantStore = errors.New("enrollment grant store denied")

// EnrollmentTokenRecord is the locked, non-secret token projection used by
// the primary exchange. Hash and lifecycle fields are never returned outside
// the primary service transaction.
type EnrollmentTokenRecord struct {
	ID                  string
	Hash                string
	HashVersion         string
	ExpiresAt           time.Time
	ConsumedAt          *time.Time
	ConsumedByAttemptID string
	RevokedAt           *time.Time
}

func (r EnrollmentTokenRecord) String() string { return "[enrollment token record redacted]" }

// PrimaryIdentityRecord is the locked authoritative identity used for the
// current exchange. IDs are canonical UUID strings; the service maps all
// validation failures to its generic denial sentinel.
type PrimaryIdentityRecord struct {
	InstallationID        string
	NodeID                string
	Role                  domain.TopologyRole
	LeadershipGeneration  uint64
	LatestKnownGeneration uint64
	ClusterKeyID          *uuid.UUID
}

func (r PrimaryIdentityRecord) String() string { return "[primary identity record redacted]" }

// ClusterKeyMaterial is a bounded in-memory view returned by the existing
// cluster secure boundary. Its bytes are never persisted by configuration.
type ClusterKeyMaterial struct {
	id  string
	key []byte
}

func NewClusterKeyMaterial(id string, key []byte) (ClusterKeyMaterial, error) {
	parsed, err := uuid.Parse(id)
	if err != nil || parsed == uuid.Nil || len(key) != 32 {
		return ClusterKeyMaterial{}, ErrEnrollmentGrantStore
	}
	return ClusterKeyMaterial{id: parsed.String(), key: append([]byte(nil), key...)}, nil
}

func (m ClusterKeyMaterial) ID() string { return m.id }

func (m ClusterKeyMaterial) String() string { return "[cluster key material redacted]" }

func (m ClusterKeyMaterial) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, m.String())
}

func (m ClusterKeyMaterial) CopyBytes() []byte {
	if len(m.key) != 32 {
		return nil
	}
	return append([]byte(nil), m.key...)
}

func (m *ClusterKeyMaterial) Destroy() {
	if m == nil {
		return
	}
	for i := range m.key {
		m.key[i] = 0
	}
	m.key = nil
}

// EnrollmentGrantRecord is the complete safe persisted grant envelope. The
// verified preview digest is a dedicated binding field; legacy rows without it
// are not eligible for retry.
type EnrollmentGrantRecord struct {
	AttemptID              string
	TokenID                string
	InstallationID         string
	NodeID                 string
	PrimaryID              string
	PrimaryGeneration      uint64
	VerifiedPreviewDigest  string
	NodeEphemeralPublicKey string
	SealedBootstrapPayload []byte
	PayloadHash            string
	CreatedAt              time.Time
	ExpiresAt              time.Time
}

type NodeCredentialRecord struct {
	ID             string
	NodeID         string
	CredentialHash string
	HashVersion    string
	CreatedAt      time.Time
}

func (r NodeCredentialRecord) String() string { return "[node credential record redacted]" }

type EnrolledNodeRecord struct {
	NodeID             string
	InstallationID     string
	PrimaryID          string
	CredentialID       string
	CreatedByAttemptID string
	EnrolledAt         time.Time
}

// PrimaryGrantTransaction is the atomic PostgreSQL port for one primary grant
// exchange. Implementations must execute every method on the transaction
// supplied to WithPrimaryGrant.
type PrimaryGrantTransaction interface {
	LockEnrollmentToken(ctx context.Context, selector string) (EnrollmentTokenRecord, error)
	LockPrimaryIdentity(ctx context.Context) (PrimaryIdentityRecord, error)
	LoadOrCreateClusterKEK(ctx context.Context, currentID string) (ClusterKeyMaterial, error)
	GetEnrollmentGrant(ctx context.Context, attemptID string) (EnrollmentGrantRecord, error)
	CreateNodeCredential(ctx context.Context, credential NodeCredentialRecord) error
	CreateEnrolledNode(ctx context.Context, node EnrolledNodeRecord) error
	CreateEnrollmentGrant(ctx context.Context, grant EnrollmentGrantRecord) error
	ConsumeEnrollmentToken(ctx context.Context, tokenID, attemptID string, consumedAt time.Time) error
	TransitionPrimaryToWithNodes(ctx context.Context, generation uint64) error
}

// PrimaryGrantStore owns the transaction boundary for primary grant issuance.
type PrimaryGrantStore interface {
	WithPrimaryGrant(ctx context.Context, fn func(PrimaryGrantTransaction) error) error
}

func (t *pgPhase2Transaction) LockEnrollmentToken(ctx context.Context, selector string) (EnrollmentTokenRecord, error) {
	var record EnrollmentTokenRecord
	var consumedBy *string
	if err := t.tx.QueryRow(ctx, `
		select id::text, token_hash, hash_version, expires_at, consumed_at,
			consumed_by_attempt_id::text, revoked_at
		from enrollment_tokens
		where token_selector = $1
		for update
	`, selector).Scan(
		&record.ID, &record.Hash, &record.HashVersion, &record.ExpiresAt,
		&record.ConsumedAt, &consumedBy, &record.RevokedAt,
	); err != nil {
		return EnrollmentTokenRecord{}, ErrEnrollmentGrantStore
	}
	if consumedBy != nil {
		record.ConsumedByAttemptID = *consumedBy
	}
	return record, nil
}

func (t *pgPhase2Transaction) LockPrimaryIdentity(ctx context.Context) (PrimaryIdentityRecord, error) {
	var installationID, nodeID uuid.UUID
	var role string
	var generation, latestGeneration int64
	var clusterKeyID *uuid.UUID
	if err := t.tx.QueryRow(ctx, `
		select installation_id, node_id, role::text, leadership_generation,
			latest_known_generation, cluster_key_id
		from installation_identity
		where id = $1
		for update
	`, installationIDSingleton).Scan(
		&installationID, &nodeID, &role, &generation, &latestGeneration, &clusterKeyID,
	); err != nil || generation <= 0 || latestGeneration <= 0 {
		return PrimaryIdentityRecord{}, ErrEnrollmentGrantStore
	}
	return PrimaryIdentityRecord{
		InstallationID:        installationID.String(),
		NodeID:                nodeID.String(),
		Role:                  domain.TopologyRole(role),
		LeadershipGeneration:  uint64(generation),
		LatestKnownGeneration: uint64(latestGeneration),
		ClusterKeyID:          clusterKeyID,
	}, nil
}

func (t *pgPhase2Transaction) LoadOrCreateClusterKEK(ctx context.Context, currentID string) (ClusterKeyMaterial, error) {
	if t.clusterKeyStore == nil {
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
	material, err := t.clusterKeyStore.LoadOrCreate(ctx, t.tx, activeID)
	if err != nil {
		return ClusterKeyMaterial{}, ErrEnrollmentGrantStore
	}
	if currentID == "" {
		result, err := t.tx.Exec(ctx, `
			update installation_identity
			set cluster_key_id = $2, updated_at = now()
			where id = $1 and cluster_key_id is null
		`, installationIDSingleton, material.ID)
		if err != nil || result.RowsAffected() != 1 {
			material.Destroy()
			return ClusterKeyMaterial{}, ErrEnrollmentGrantStore
		}
	}
	key := material.CopyBytes()
	result, err := NewClusterKeyMaterial(material.ID.String(), key)
	zeroGrantBytes(key)
	material.Destroy()
	if err != nil {
		return ClusterKeyMaterial{}, ErrEnrollmentGrantStore
	}
	return result, nil
}

func (t *pgPhase2Transaction) GetEnrollmentGrant(ctx context.Context, attemptID string) (EnrollmentGrantRecord, error) {
	var grant EnrollmentGrantRecord
	var digest *string
	var sealed string
	if err := t.tx.QueryRow(ctx, `
		select attempt_id::text, token_id::text, installation_id::text, node_id::text,
			primary_id::text, primary_generation, verified_preview_digest,
			node_ephemeral_public_key, sealed_bootstrap_payload, payload_hash,
			created_at, expires_at
		from enrollment_grants
		where attempt_id = $1
	`, attemptID).Scan(
		&grant.AttemptID, &grant.TokenID, &grant.InstallationID, &grant.NodeID,
		&grant.PrimaryID, &grant.PrimaryGeneration, &digest, &grant.NodeEphemeralPublicKey,
		&sealed, &grant.PayloadHash, &grant.CreatedAt, &grant.ExpiresAt,
	); err != nil {
		return EnrollmentGrantRecord{}, ErrEnrollmentGrantStore
	}
	if digest != nil {
		grant.VerifiedPreviewDigest = *digest
	}
	grant.SealedBootstrapPayload = []byte(sealed)
	return grant, nil
}

func (t *pgPhase2Transaction) CreateNodeCredential(ctx context.Context, credential NodeCredentialRecord) error {
	if _, err := t.tx.Exec(ctx, `
		insert into node_credentials (id, node_id, credential_hash, hash_version, created_at)
		values ($1, $2, $3, $4, $5)
	`, credential.ID, credential.NodeID, credential.CredentialHash, credential.HashVersion, credential.CreatedAt); err != nil {
		return ErrEnrollmentGrantStore
	}
	return nil
}

func (t *pgPhase2Transaction) CreateEnrolledNode(ctx context.Context, node EnrolledNodeRecord) error {
	if _, err := t.tx.Exec(ctx, `
		insert into enrolled_nodes (
			node_id, installation_id, primary_id, credential_id, enrolled_at, created_by_attempt_id
		) values ($1, $2, $3, $4, $5, $6)
	`, node.NodeID, node.InstallationID, node.PrimaryID, node.CredentialID, node.EnrolledAt, node.CreatedByAttemptID); err != nil {
		return ErrEnrollmentGrantStore
	}
	return nil
}

func (t *pgPhase2Transaction) CreateEnrollmentGrant(ctx context.Context, grant EnrollmentGrantRecord) error {
	if _, err := t.tx.Exec(ctx, `
		insert into enrollment_grants (
			attempt_id, token_id, installation_id, node_id, primary_id, primary_generation,
			verified_preview_digest, node_ephemeral_public_key, sealed_bootstrap_payload,
			payload_hash, created_at, expires_at
		) values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
	`, grant.AttemptID, grant.TokenID, grant.InstallationID, grant.NodeID, grant.PrimaryID,
		grant.PrimaryGeneration, grant.VerifiedPreviewDigest, grant.NodeEphemeralPublicKey,
		string(grant.SealedBootstrapPayload), grant.PayloadHash, grant.CreatedAt, grant.ExpiresAt); err != nil {
		return ErrEnrollmentGrantStore
	}
	return nil
}

func (t *pgPhase2Transaction) ConsumeEnrollmentToken(ctx context.Context, tokenID, attemptID string, consumedAt time.Time) error {
	result, err := t.tx.Exec(ctx, `
		update enrollment_tokens
		set consumed_at = $3, consumed_by_attempt_id = $2
		where id = $1 and consumed_at is null and revoked_at is null
	`, tokenID, attemptID, consumedAt)
	if err != nil || result.RowsAffected() != 1 {
		return ErrEnrollmentGrantStore
	}
	return nil
}

func (t *pgPhase2Transaction) TransitionPrimaryToWithNodes(ctx context.Context, generation uint64) error {
	result, err := t.tx.Exec(ctx, `
		update installation_identity
		set role = 'primary-with-nodes', updated_at = now()
		where id = $1 and role = 'primary' and leadership_generation = $2
	`, installationIDSingleton, generation)
	if err != nil || result.RowsAffected() != 1 {
		return ErrEnrollmentGrantStore
	}
	return nil
}

func zeroGrantBytes(value []byte) {
	for i := range value {
		value[i] = 0
	}
}

var _ PrimaryGrantTransaction = (*pgPhase2Transaction)(nil)
var _ PrimaryGrantStore = (*PgPhase2Store)(nil)

const installationIDSingleton = "default"
