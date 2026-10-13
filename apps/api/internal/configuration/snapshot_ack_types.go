package configuration

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
)

var // ErrSnapshotAcknowledgementDenied is the generic authorization or tuple
// rejection returned by the acknowledgement persistence boundary.
ErrSnapshotAcknowledgementDenied = errors.New("snapshot acknowledgement denied")

var // ErrSnapshotAcknowledgementRevoked distinguishes a currently revoked
// credential or its enrollment authority without revealing which one.
ErrSnapshotAcknowledgementRevoked = errors.New("snapshot acknowledgement revoked")

var // ErrSnapshotAcknowledgementUnavailable covers database and key-store
// failures. Callers must not retry a denied or revoked acknowledgement as if
// it were a transient persistence failure.
ErrSnapshotAcknowledgementUnavailable = errors.New("snapshot acknowledgement unavailable")

var ErrSnapshotAcknowledgementNotFound = errors.New("snapshot acknowledgement not found")

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

func validSnapshotAcknowledgement(ack SnapshotAcknowledgement) bool {
	return validSnapshotAcknowledgementInput(ack.NodeID, ack.ContentHash, ack.AppliedAt) &&
		ack.SnapshotVersion > 0 && ack.ReplicationVersion > 0 &&
		validPublicationUUID(ack.RevisionID) && ack.LeadershipGeneration > 0
}

func validSnapshotAcknowledgementInput(nodeID, contentHash string, appliedAt time.Time) bool {
	return validPublicationUUID(nodeID) && validSnapshotPublicationHash(contentHash) && !appliedAt.IsZero()
}

func mapSnapshotAcknowledgementCredentialError(err error) error {
	switch {
	case errors.Is(err, ErrSnapshotAcknowledgementDenied), errors.Is(err, ErrSnapshotAcknowledgementRevoked), errors.Is(err, ErrSnapshotAcknowledgementUnavailable):
		return err
	default:
		return ErrSnapshotAcknowledgementUnavailable
	}
}

var ErrSnapshotByTokenUnauthenticated = errors.New("snapshot by token unauthenticated")

var ErrSnapshotByTokenGone = errors.New("snapshot by token gone")

var ErrSnapshotByTokenDenied = errors.New("snapshot by token denied")

var ErrSnapshotByTokenUnavailable = errors.New("snapshot by token unavailable")

var ErrNoPublishableSnapshot = errors.New("snapshot by token has no publishable snapshot")

func validSnapshotByTokenIdentity(identity SnapshotPublicationIdentityRecord) bool {
	return validPublicationUUID(identity.InstallationID) &&
		validPublicationUUID(identity.NodeID) &&
		(identity.Role == domain.TopologyRolePrimary || identity.Role == domain.TopologyRolePrimaryWithNodes) &&
		identity.LeadershipGeneration > 0 &&
		identity.LeadershipGeneration == identity.LatestKnownGeneration &&
		identity.ClusterKeyID != nil && *identity.ClusterKeyID != uuid.Nil
}

func readySnapshotPublicationIdentity(record SnapshotPublicationIdentityRecord) bool {
	return validPublicationUUID(record.InstallationID) && validPublicationUUID(record.NodeID) &&
		(record.Role == domain.TopologyRolePrimary || record.Role == domain.TopologyRolePrimaryWithNodes) &&
		record.LeadershipGeneration > 0 && record.LeadershipGeneration == record.LatestKnownGeneration
}
