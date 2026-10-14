package configuration

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"math"
	"time"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

var _ PrimaryGrantStore = (*SQLitePhase2Store)(nil)

// WithPrimaryGrant runs one primary grant exchange inside a single writer
// transaction (BEGIN IMMEDIATE). Token lock, identity lock, grant lookup, KEK
// load-or-create, credential/node/grant creation, token consumption and the
// role transition commit or roll back together. SQLite has no row locks: the
// writer's database-level write lock is what makes the Lock* operations
// exclusive, and it serializes concurrent exchanges for the same token.
func (s *SQLitePhase2Store) WithPrimaryGrant(ctx context.Context, fn func(PrimaryGrantTransaction) error) error {
	if ctx == nil || s == nil || s.writer == nil || s.keys == nil || fn == nil {
		return ErrEnrollmentGrantStore
	}
	return s.writer.Write(ctx, func(v sqlitedb.WriteValue) error {
		return fn(&sqlitePrimaryGrantTransaction{
			base: &sqlitePhase2Transaction{v: v, keys: s.keys, master: s.master},
			v:    v,
		})
	})
}

// sqlitePrimaryGrantTransaction exposes only the typed grant operations. Store
// failures collapse into ErrEnrollmentGrantStore so SQL text, which could carry
// bound values, never reaches the caller or its logs.
type sqlitePrimaryGrantTransaction struct {
	base *sqlitePhase2Transaction
	v    sqlitedb.WriteValue
	// rotatedNodeID is set when CreateNodeCredential re-keyed the credential
	// row of a revoked node in place; CreateEnrolledNode then updates that
	// node's enrolled row instead of inserting a second one.
	rotatedNodeID string
}

func (t *sqlitePrimaryGrantTransaction) LockPrimaryIdentity(ctx context.Context) (PrimaryIdentityRecord, error) {
	return t.base.LockPrimaryIdentity(ctx)
}

func (t *sqlitePrimaryGrantTransaction) LoadOrCreateClusterKEK(ctx context.Context, currentID string) (ClusterKeyMaterial, error) {
	return t.base.LoadOrCreateClusterKEK(ctx, currentID)
}

func (t *sqlitePrimaryGrantTransaction) LockEnrollmentToken(ctx context.Context, selector string) (EnrollmentTokenRecord, error) {
	var (
		record                       EnrollmentTokenRecord
		expires                      string
		consumed, consumedBy, revoke *string
	)
	err := t.v.Conn().QueryRowContext(ctx, `select id, token_hash, hash_version, expires_at,
			consumed_at, consumed_by_attempt_id, revoked_at
		from enrollment_tokens where token_selector = ?`, selector).Scan(
		&record.ID, &record.Hash, &record.HashVersion, &expires, &consumed, &consumedBy, &revoke)
	if err != nil {
		return EnrollmentTokenRecord{}, ErrEnrollmentGrantStore
	}
	expiresAt, err := sqliteEnqueueTime(&expires)
	if err != nil || expiresAt == nil {
		return EnrollmentTokenRecord{}, ErrEnrollmentGrantStore
	}
	record.ExpiresAt = expiresAt.UTC()
	if record.ConsumedAt, err = sqliteGrantOptionalTime(consumed); err != nil {
		return EnrollmentTokenRecord{}, ErrEnrollmentGrantStore
	}
	if record.RevokedAt, err = sqliteGrantOptionalTime(revoke); err != nil {
		return EnrollmentTokenRecord{}, ErrEnrollmentGrantStore
	}
	if consumedBy != nil {
		record.ConsumedByAttemptID = *consumedBy
	}
	return record, nil
}

func (t *sqlitePrimaryGrantTransaction) GetEnrollmentGrant(ctx context.Context, attemptID string) (EnrollmentGrantRecord, error) {
	var (
		grant                     EnrollmentGrantRecord
		generation                int64
		payload, created, expires string
		digest                    *string
	)
	err := t.v.Conn().QueryRowContext(ctx, `select attempt_id, token_id, installation_id, node_id, primary_id,
			primary_generation, verified_preview_digest, node_ephemeral_public_key,
			sealed_bootstrap_payload, payload_hash, created_at, expires_at
		from enrollment_grants where attempt_id = ?`, attemptID).Scan(
		&grant.AttemptID, &grant.TokenID, &grant.InstallationID, &grant.NodeID, &grant.PrimaryID,
		&generation, &digest, &grant.NodeEphemeralPublicKey,
		&payload, &grant.PayloadHash, &created, &expires)
	if errors.Is(err, sql.ErrNoRows) || err != nil || generation <= 0 {
		return EnrollmentGrantRecord{}, ErrEnrollmentGrantStore
	}
	grant.PrimaryGeneration = uint64(generation)
	if digest != nil {
		grant.VerifiedPreviewDigest = *digest
	}
	if grant.SealedBootstrapPayload, err = base64.StdEncoding.DecodeString(payload); err != nil {
		return EnrollmentGrantRecord{}, ErrEnrollmentGrantStore
	}
	createdAt, err := sqliteEnqueueTime(&created)
	if err != nil || createdAt == nil {
		return EnrollmentGrantRecord{}, ErrEnrollmentGrantStore
	}
	expiresAt, err := sqliteEnqueueTime(&expires)
	if err != nil || expiresAt == nil {
		return EnrollmentGrantRecord{}, ErrEnrollmentGrantStore
	}
	grant.CreatedAt, grant.ExpiresAt = createdAt.UTC(), expiresAt.UTC()
	return grant, nil
}

// CreateNodeCredential persists only the credential hash and its version; the
// plaintext secret never reaches the store. A node that was enrolled before
// and then revoked (demote/promote retire every relationship but keep the
// rows) is rotated in place: node_credentials_node_idx allows one row per
// node, so the revoked row is re-keyed instead of inserting a second one. A
// live row is never touched.
func (t *sqlitePrimaryGrantTransaction) CreateNodeCredential(ctx context.Context, credential NodeCredentialRecord) error {
	created, err := sqliteEnrollmentTokenTime(credential.CreatedAt)
	if err != nil {
		return ErrEnrollmentGrantStore
	}
	var existingID string
	var nodeRevoked *string
	err = t.v.Conn().QueryRowContext(ctx, `select c.id, n.revoked_at
		from node_credentials c
		join enrolled_nodes n on n.node_id = c.node_id and n.credential_id = c.id
		where c.node_id = ?`, credential.NodeID).Scan(&existingID, &nodeRevoked)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if _, err := t.v.Conn().ExecContext(ctx, `insert into node_credentials
			(id, node_id, credential_hash, hash_version, created_at) values (?, ?, ?, ?, ?)`,
			credential.ID, credential.NodeID, credential.CredentialHash, credential.HashVersion, created); err != nil {
			return ErrEnrollmentGrantStore
		}
		return nil
	case err != nil || nodeRevoked == nil:
		return ErrEnrollmentGrantStore
	}
	// enrolled_nodes.credential_id references node_credentials.id; defer the
	// check to commit so both rows can move to the new id in one transaction.
	if _, err := t.v.Conn().ExecContext(ctx, `pragma defer_foreign_keys = on`); err != nil {
		return ErrEnrollmentGrantStore
	}
	result, err := t.v.Conn().ExecContext(ctx, `update node_credentials
		set id = ?, credential_hash = ?, hash_version = ?, created_at = ?, revoked_at = null
		where id = ? and node_id = ?`,
		credential.ID, credential.CredentialHash, credential.HashVersion, created, existingID, credential.NodeID)
	if err != nil {
		return ErrEnrollmentGrantStore
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return ErrEnrollmentGrantStore
	}
	t.rotatedNodeID = credential.NodeID
	return nil
}

// CreateEnrolledNode inserts the enrolled row, or completes an in-place
// rotation started by CreateNodeCredential. Rotation requires the existing row
// to be revoked and to belong to the same installation; anything else updates
// zero rows and fails the whole transaction.
func (t *sqlitePrimaryGrantTransaction) CreateEnrolledNode(ctx context.Context, node EnrolledNodeRecord) error {
	enrolled, err := sqliteEnrollmentTokenTime(node.EnrolledAt)
	if err != nil {
		return ErrEnrollmentGrantStore
	}
	if t.rotatedNodeID != "" {
		if t.rotatedNodeID != node.NodeID {
			return ErrEnrollmentGrantStore
		}
		result, err := t.v.Conn().ExecContext(ctx, `update enrolled_nodes
			set primary_id = ?, credential_id = ?, enrolled_at = ?, created_by_attempt_id = ?, revoked_at = null
			where node_id = ? and installation_id = ? and revoked_at is not null`,
			node.PrimaryID, node.CredentialID, enrolled, node.CreatedByAttemptID, node.NodeID, node.InstallationID)
		if err != nil {
			return ErrEnrollmentGrantStore
		}
		if n, err := result.RowsAffected(); err != nil || n != 1 {
			return ErrEnrollmentGrantStore
		}
		return nil
	}
	if _, err := t.v.Conn().ExecContext(ctx, `insert into enrolled_nodes
		(node_id, installation_id, primary_id, credential_id, enrolled_at, created_by_attempt_id)
		values (?, ?, ?, ?, ?, ?)`,
		node.NodeID, node.InstallationID, node.PrimaryID, node.CredentialID, enrolled, node.CreatedByAttemptID); err != nil {
		return ErrEnrollmentGrantStore
	}
	return nil
}

// CreateEnrollmentGrant stores the sealed payload (already encrypted to the
// recipient's ephemeral key) as standard base64 text, since the column is TEXT.
func (t *sqlitePrimaryGrantTransaction) CreateEnrollmentGrant(ctx context.Context, grant EnrollmentGrantRecord) error {
	if grant.PrimaryGeneration == 0 || grant.PrimaryGeneration > math.MaxInt64 || len(grant.SealedBootstrapPayload) == 0 {
		return ErrEnrollmentGrantStore
	}
	created, err := sqliteEnrollmentTokenTime(grant.CreatedAt)
	if err != nil {
		return ErrEnrollmentGrantStore
	}
	expires, err := sqliteEnrollmentTokenTime(grant.ExpiresAt)
	if err != nil {
		return ErrEnrollmentGrantStore
	}
	if _, err := t.v.Conn().ExecContext(ctx, `insert into enrollment_grants
		(attempt_id, token_id, installation_id, node_id, primary_id, primary_generation,
		 node_ephemeral_public_key, sealed_bootstrap_payload, payload_hash,
		 created_at, expires_at, verified_preview_digest)
		values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		grant.AttemptID, grant.TokenID, grant.InstallationID, grant.NodeID, grant.PrimaryID,
		int64(grant.PrimaryGeneration), grant.NodeEphemeralPublicKey,
		base64.StdEncoding.EncodeToString(grant.SealedBootstrapPayload), grant.PayloadHash,
		created, expires, grant.VerifiedPreviewDigest); err != nil {
		return ErrEnrollmentGrantStore
	}
	return nil
}

// ConsumeEnrollmentToken is a compare-and-set: it succeeds only for a token
// that is still unconsumed and unrevoked.
func (t *sqlitePrimaryGrantTransaction) ConsumeEnrollmentToken(ctx context.Context, tokenID, attemptID string, consumedAt time.Time) error {
	if _, err := uuid.Parse(tokenID); err != nil {
		return ErrEnrollmentGrantStore
	}
	stamp, err := sqliteEnrollmentTokenTime(consumedAt)
	if err != nil {
		return ErrEnrollmentGrantStore
	}
	result, err := t.v.Conn().ExecContext(ctx, `update enrollment_tokens
		set consumed_at = ?, consumed_by_attempt_id = ?
		where id = ? and consumed_at is null and revoked_at is null`, stamp, attemptID, tokenID)
	if err != nil {
		return ErrEnrollmentGrantStore
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return ErrEnrollmentGrantStore
	}
	return nil
}

// TransitionPrimaryToWithNodes moves the identity from primary to
// primary-with-nodes only at the exact leadership generation the exchange
// verified; any drift leaves zero rows updated and fails the transaction.
func (t *sqlitePrimaryGrantTransaction) TransitionPrimaryToWithNodes(ctx context.Context, generation uint64) error {
	if generation == 0 || generation > math.MaxInt64 {
		return ErrEnrollmentGrantStore
	}
	result, err := t.v.Conn().ExecContext(ctx, `update installation_identity
		set role = 'primary-with-nodes', updated_at = ?
		where id = ? and role = 'primary'
		and leadership_generation = ? and latest_known_generation = ?`,
		t.v.Timestamp(), installationIDSingleton, int64(generation), int64(generation))
	if err != nil {
		return ErrEnrollmentGrantStore
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return ErrEnrollmentGrantStore
	}
	return nil
}

func sqliteGrantOptionalTime(key *string) (*time.Time, error) {
	value, err := sqliteEnqueueTime(key)
	if err != nil || value == nil {
		return nil, err
	}
	utc := value.UTC()
	return &utc, nil
}
