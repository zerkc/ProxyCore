package configuration

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/zerkc/ProxyCore/apps/api/internal/cluster"
)

var (
	// ErrSnapshotAcknowledgementDenied is the generic authorization or tuple
	// rejection returned by the acknowledgement persistence boundary.
	ErrSnapshotAcknowledgementDenied = errors.New("snapshot acknowledgement denied")
	// ErrSnapshotAcknowledgementRevoked distinguishes a currently revoked
	// credential or its enrollment authority without revealing which one.
	ErrSnapshotAcknowledgementRevoked = errors.New("snapshot acknowledgement revoked")
	// ErrSnapshotAcknowledgementUnavailable covers database and key-store
	// failures. Callers must not retry a denied or revoked acknowledgement as if
	// it were a transient persistence failure.
	ErrSnapshotAcknowledgementUnavailable = errors.New("snapshot acknowledgement unavailable")
	ErrSnapshotAcknowledgementNotFound    = errors.New("snapshot acknowledgement not found")

	// American spellings keep the HTTP package and external integrations from
	// having to depend on the historical British spelling of the table type.
	ErrSnapshotAcknowledgmentDenied      = ErrSnapshotAcknowledgementDenied
	ErrSnapshotAcknowledgmentRevoked     = ErrSnapshotAcknowledgementRevoked
	ErrSnapshotAcknowledgmentUnavailable = ErrSnapshotAcknowledgementUnavailable
	ErrSnapshotAcknowledgmentNotFound    = ErrSnapshotAcknowledgementNotFound
)

// SnapshotAcknowledgementRequest contains request-only bearer material for
// the HTTP acknowledgement boundary. PresentedCredential is never persisted.
type SnapshotAcknowledgementRequest struct {
	NodeID              string
	ContentHash         string
	AppliedAt           time.Time
	ReceivedAt          time.Time
	CredentialID        string
	PresentedCredential string
	Authenticate        SnapshotPublicationCredentialAuthenticator
}

func (r SnapshotAcknowledgementRequest) String() string {
	return "[snapshot acknowledgement request redacted]"
}

// SnapshotAcknowledgementStore is the durable acknowledgement write port.
type SnapshotAcknowledgementStore interface {
	RecordSnapshotAcknowledgement(context.Context, SnapshotAcknowledgement) error
}

// SnapshotAcknowledgementAuthorizer is implemented by the production store so
// the credential check and acknowledgement insert share one transaction.
type SnapshotAcknowledgementAuthorizer interface {
	RecordSnapshotAcknowledgementForCredential(context.Context, SnapshotAcknowledgementRequest) error
}

type snapshotAcknowledgementCredential struct {
	ID                  string
	NodeID              string
	CredentialHash      string
	HashVersion         string
	CredentialRevokedAt *time.Time
	NodeRevokedAt       *time.Time
	TokenRevokedAt      *time.Time
	TokenConsumedAt     *time.Time
	PrimaryID           string
	PrimaryGeneration   int64
	LineageClusterKeyID *uuid.UUID
}

// RecordSnapshotAcknowledgement records a complete, already-authenticated
// acknowledgement tuple. The method still reloads the credential, enrollment
// token, current PRIMARY identity, active KEK, and applied proof in one
// transaction so callers cannot turn a stale authorization decision into a
// durable acknowledgement.
func (s *PgPhase2Store) RecordSnapshotAcknowledgement(ctx context.Context, ack SnapshotAcknowledgement) error {
	if ctx == nil || !validSnapshotAcknowledgement(ack) {
		return ErrSnapshotAcknowledgementDenied
	}
	if s == nil || s.pool == nil {
		return ErrSnapshotAcknowledgementUnavailable
	}
	if ack.ReceivedAt.IsZero() {
		ack.ReceivedAt = time.Now().UTC()
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return ErrSnapshotAcknowledgementUnavailable
	}
	defer tx.Rollback(ctx)

	credential, err := lockSnapshotAcknowledgementCredential(ctx, tx, ack.NodeID, "")
	if err != nil {
		return mapSnapshotAcknowledgementCredentialError(err)
	}
	if err := validateSnapshotAcknowledgementCredential(credential); err != nil {
		return err
	}
	if err := recordSnapshotAcknowledgementTx(ctx, tx, s.clusterKeyStore, ack, credential); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return ErrSnapshotAcknowledgementUnavailable
	}
	return nil
}

// RecordSnapshotAcknowledgementForCredential is the production HTTP boundary.
// It authenticates the presented pcnode1 bearer before deriving the immutable
// version/revision tuple and inserting the acknowledgement on the same
// transaction.
func (s *PgPhase2Store) RecordSnapshotAcknowledgementForCredential(ctx context.Context, request SnapshotAcknowledgementRequest) error {
	if ctx == nil || request.Authenticate == nil || request.PresentedCredential == "" ||
		!validSnapshotAcknowledgementInput(request.NodeID, request.ContentHash, request.AppliedAt) ||
		!validPublicationUUID(request.CredentialID) {
		return ErrSnapshotAcknowledgementDenied
	}
	if s == nil || s.pool == nil {
		return ErrSnapshotAcknowledgementUnavailable
	}
	if request.ReceivedAt.IsZero() {
		request.ReceivedAt = time.Now().UTC()
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return ErrSnapshotAcknowledgementUnavailable
	}
	defer tx.Rollback(ctx)

	credential, err := lockSnapshotAcknowledgementCredential(ctx, tx, request.NodeID, request.CredentialID)
	if err != nil {
		return mapSnapshotAcknowledgementCredentialError(err)
	}
	if err := validateSnapshotAcknowledgementCredential(credential); err != nil {
		return err
	}
	principal, err := request.Authenticate(request.PresentedCredential, SnapshotPublicationCredentialRecord{
		ID:                  credential.ID,
		NodeID:              credential.NodeID,
		CredentialHash:      credential.CredentialHash,
		HashVersion:         credential.HashVersion,
		CredentialRevokedAt: credential.CredentialRevokedAt,
		NodeRevokedAt:       credential.NodeRevokedAt,
		PrimaryID:           credential.PrimaryID,
	})
	if err != nil || principal.CredentialID != credential.ID || principal.NodeID != credential.NodeID {
		return ErrSnapshotAcknowledgementDenied
	}

	ack, err := deriveSnapshotAcknowledgement(ctx, tx, s.clusterKeyStore, SnapshotAcknowledgementInput{
		NodeID:      request.NodeID,
		ContentHash: request.ContentHash,
		AppliedAt:   request.AppliedAt,
		ReceivedAt:  request.ReceivedAt,
	}, credential)
	if err != nil {
		return err
	}
	if err := insertSnapshotAcknowledgement(ctx, tx, ack); err != nil {
		return ErrSnapshotAcknowledgementUnavailable
	}
	if err := tx.Commit(ctx); err != nil {
		return ErrSnapshotAcknowledgementUnavailable
	}
	return nil
}

// GetSnapshotAcknowledgement returns a caller-owned value read from the
// durable table. It never returns a mutable database-backed reference.
func (s *PgPhase2Store) GetSnapshotAcknowledgement(ctx context.Context, nodeID, contentHash string) (SnapshotAcknowledgement, error) {
	if ctx == nil || !validSnapshotAcknowledgementInput(nodeID, contentHash, time.Unix(1, 0)) {
		return SnapshotAcknowledgement{}, ErrSnapshotAcknowledgementDenied
	}
	if s == nil || s.pool == nil {
		return SnapshotAcknowledgement{}, ErrSnapshotAcknowledgementUnavailable
	}
	var ack SnapshotAcknowledgement
	err := s.pool.QueryRow(ctx, `
		select node_id::text, content_hash, snapshot_version, replication_version,
			revision_id::text, leadership_generation, applied_at, received_at
		from node_snapshot_acks
		where node_id = $1 and content_hash = $2
	`, nodeID, contentHash).Scan(
		&ack.NodeID, &ack.ContentHash, &ack.SnapshotVersion, &ack.ReplicationVersion,
		&ack.RevisionID, &ack.LeadershipGeneration, &ack.AppliedAt, &ack.ReceivedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return SnapshotAcknowledgement{}, ErrSnapshotAcknowledgementNotFound
	}
	if err != nil {
		return SnapshotAcknowledgement{}, ErrSnapshotAcknowledgementUnavailable
	}
	return ack, nil
}

type SnapshotAcknowledgementInput struct {
	NodeID      string
	ContentHash string
	AppliedAt   time.Time
	ReceivedAt  time.Time
}

func validSnapshotAcknowledgement(ack SnapshotAcknowledgement) bool {
	return validSnapshotAcknowledgementInput(ack.NodeID, ack.ContentHash, ack.AppliedAt) &&
		ack.SnapshotVersion > 0 && ack.ReplicationVersion > 0 &&
		validPublicationUUID(ack.RevisionID) && ack.LeadershipGeneration > 0
}

func validSnapshotAcknowledgementInput(nodeID, contentHash string, appliedAt time.Time) bool {
	return validPublicationUUID(nodeID) && validSnapshotPublicationHash(contentHash) && !appliedAt.IsZero()
}

func lockSnapshotAcknowledgementCredential(ctx context.Context, tx pgx.Tx, nodeID, credentialID string) (snapshotAcknowledgementCredential, error) {
	var record snapshotAcknowledgementCredential
	query := `
		select c.id::text, c.node_id::text, c.credential_hash, c.hash_version,
			c.revoked_at, n.revoked_at, t.revoked_at, t.consumed_at,
			n.primary_id::text, g.primary_generation, a.cluster_key_id
		from node_credentials c
		join enrolled_nodes n on n.credential_id = c.id and n.node_id = c.node_id
		join enrollment_grants g on g.attempt_id = n.created_by_attempt_id and
			g.node_id = n.node_id and g.installation_id = n.installation_id and g.primary_id = n.primary_id
		join enrollment_attempts a on a.id = g.attempt_id
		join enrollment_tokens t on t.id = g.token_id
		where c.node_id = $1
	`
	args := []any{nodeID}
	if credentialID != "" {
		query += " and c.id = $2\n"
		args = append(args, credentialID)
	}
	query += " for update of c, n, g, t, a"
	if err := tx.QueryRow(ctx, query, args...).Scan(
		&record.ID, &record.NodeID, &record.CredentialHash, &record.HashVersion,
		&record.CredentialRevokedAt, &record.NodeRevokedAt, &record.TokenRevokedAt,
		&record.TokenConsumedAt, &record.PrimaryID, &record.PrimaryGeneration,
		&record.LineageClusterKeyID,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return snapshotAcknowledgementCredential{}, ErrSnapshotAcknowledgementDenied
		}
		return snapshotAcknowledgementCredential{}, ErrSnapshotAcknowledgementUnavailable
	}
	return record, nil
}

func mapSnapshotAcknowledgementCredentialError(err error) error {
	switch {
	case errors.Is(err, ErrSnapshotAcknowledgementDenied), errors.Is(err, ErrSnapshotAcknowledgementRevoked), errors.Is(err, ErrSnapshotAcknowledgementUnavailable):
		return err
	default:
		return ErrSnapshotAcknowledgementUnavailable
	}
}

func validateSnapshotAcknowledgementCredential(record snapshotAcknowledgementCredential) error {
	if record.CredentialRevokedAt != nil || record.NodeRevokedAt != nil || record.TokenRevokedAt != nil {
		return ErrSnapshotAcknowledgementRevoked
	}
	// A consumed enrollment token is expected after a successful credential
	// mint. Consumption is token single-use state, not credential revocation;
	// only an explicit token revocation invalidates later bearer use.
	return nil
}

func deriveSnapshotAcknowledgement(
	ctx context.Context,
	tx pgx.Tx,
	keyStore cluster.KeyStore,
	input SnapshotAcknowledgementInput,
	credential snapshotAcknowledgementCredential,
) (SnapshotAcknowledgement, error) {
	identityRecord, err := lockSnapshotPublicationIdentity(ctx, tx)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return SnapshotAcknowledgement{}, ErrSnapshotAcknowledgementDenied
		}
		return SnapshotAcknowledgement{}, ErrSnapshotAcknowledgementUnavailable
	}
	if !readySnapshotPublicationIdentity(identityRecord) ||
		credential.PrimaryID != identityRecord.InstallationID ||
		credential.PrimaryGeneration != identityRecord.LeadershipGeneration ||
		identityRecord.ClusterKeyID == nil {
		return SnapshotAcknowledgement{}, ErrSnapshotAcknowledgementDenied
	}
	if credential.LineageClusterKeyID == nil || *credential.LineageClusterKeyID != *identityRecord.ClusterKeyID {
		return SnapshotAcknowledgement{}, ErrSnapshotAcknowledgementDenied
	}
	if keyStore == nil {
		return SnapshotAcknowledgement{}, ErrSnapshotAcknowledgementUnavailable
	}
	keyExists, err := activeSnapshotAcknowledgementKey(ctx, tx, *identityRecord.ClusterKeyID)
	if err != nil {
		return SnapshotAcknowledgement{}, ErrSnapshotAcknowledgementUnavailable
	}
	if !keyExists {
		return SnapshotAcknowledgement{}, ErrSnapshotAcknowledgementDenied
	}
	material, err := keyStore.LoadOrCreate(ctx, tx, identityRecord.ClusterKeyID)
	if err != nil {
		return SnapshotAcknowledgement{}, ErrSnapshotAcknowledgementUnavailable
	}
	materialID := material.ID
	keyBytes := material.CopyBytes()
	material.Destroy()
	defer zeroGrantBytes(keyBytes)
	if materialID == uuid.Nil || materialID != *identityRecord.ClusterKeyID {
		return SnapshotAcknowledgement{}, ErrSnapshotAcknowledgementUnavailable
	}
	kek, err := cluster.NewKEK(keyBytes)
	if err != nil {
		return SnapshotAcknowledgement{}, ErrSnapshotAcknowledgementUnavailable
	}
	kek.Destroy()

	candidate, found, err := findSnapshotAcknowledgementCandidate(ctx, tx, input.ContentHash, input.AppliedAt, identityRecord)
	if err != nil {
		return SnapshotAcknowledgement{}, ErrSnapshotAcknowledgementUnavailable
	}
	if !found {
		return SnapshotAcknowledgement{}, ErrSnapshotAcknowledgementDenied
	}
	candidate.NodeID = input.NodeID
	candidate.ReceivedAt = input.ReceivedAt
	if candidate.NodeID != credential.NodeID || candidate.LeadershipGeneration != identityRecord.LeadershipGeneration {
		return SnapshotAcknowledgement{}, ErrSnapshotAcknowledgementDenied
	}
	return candidate, nil
}

var _ SnapshotAcknowledgementStore = (*PgPhase2Store)(nil)
var _ SnapshotAcknowledgementAuthorizer = (*PgPhase2Store)(nil)
