package sync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/identity"
	replicationsnapshot "github.com/zerkc/ProxyCore/apps/api/internal/snapshot"
)

const (
	// DefaultSnapshotPublicationMaxBytes bounds the immutable response before
	// the PostgreSQL store reads its JSON bytes. HTTP transport limits remain a
	// separate concern for PNE-6.
	DefaultSnapshotPublicationMaxBytes = configuration.SnapshotPublicationMaxBytes
	SnapshotContentHashHexLength       = sha256.Size * 2
)

var ErrSnapshotDenied = errors.New("snapshot denied")

// SnapshotRequest is the request-only publisher contract. Credential is a
// pcnode1 bearer and is never included in a response or error.
type SnapshotRequest struct {
	Credential string
	After      string
}

// SnapshotMetadata is the non-secret identity of one fully applied snapshot.
type SnapshotMetadata struct {
	SnapshotID           string
	ContentHash          string
	SnapshotVersion      int
	ReplicationVersion   int
	RevisionID           string
	RevisionNumber       int
	SourcePrimaryID      string
	LeadershipGeneration int64
	ApplyJobID           string
	AppliedAt            time.Time
}

// SnapshotResult distinguishes an authorized current caller from a caller
// that must receive the latest immutable bytes. HTTP maps Current to 204 in
// PNE-6; the service itself does not own an HTTP status code.
type SnapshotResult struct {
	Current  bool
	Metadata SnapshotMetadata
	Bytes    []byte
}

// SnapshotIdentityReader exposes only the loaded identity needed by the
// publication boundary. identity.Service implements this without allowing an
// unloaded cache to panic the publisher.
type SnapshotIdentityReader interface {
	SnapshotPublicationIdentity() (identity.Identity, error)
}

var _ SnapshotIdentityReader = (*identity.Service)(nil)

type SnapshotPublisherOptions struct {
	MaxBytes int
}

type SnapshotPublisher struct {
	store    configuration.SnapshotPublicationStore
	identity SnapshotIdentityReader
	maxBytes int
}

func NewSnapshotPublisher(
	store configuration.SnapshotPublicationStore,
	identityReader SnapshotIdentityReader,
	opts SnapshotPublisherOptions,
) *SnapshotPublisher {
	maxBytes := opts.MaxBytes
	if maxBytes == 0 {
		maxBytes = DefaultSnapshotPublicationMaxBytes
	}
	return &SnapshotPublisher{store: store, identity: identityReader, maxBytes: maxBytes}
}

// Publish authenticates the current node credential and returns only the
// latest complete applied candidate. It performs no history walk and maps all
// authorization, readiness, integrity, and persistence failures to the same
// generic denial sentinel.
func (p *SnapshotPublisher) Publish(ctx context.Context, request SnapshotRequest) (SnapshotResult, error) {
	if p == nil || p.store == nil || p.identity == nil || ctx == nil || request.Credential == "" {
		return SnapshotResult{}, ErrSnapshotDenied
	}
	if p.maxBytes <= 0 || p.maxBytes > DefaultSnapshotPublicationMaxBytes {
		return SnapshotResult{}, ErrSnapshotDenied
	}
	currentIdentity, err := p.identity.SnapshotPublicationIdentity()
	if err != nil || !readyPrimaryIdentity(currentIdentity) {
		return SnapshotResult{}, ErrSnapshotDenied
	}
	credential, err := ParseNodeCredential(request.Credential)
	if err != nil {
		return SnapshotResult{}, ErrSnapshotDenied
	}
	credentialID := credential.ID()
	credential.Destroy()

	publication, err := p.store.ReadSnapshotPublication(ctx, configuration.SnapshotPublicationRequest{
		CredentialID:        credentialID,
		PresentedCredential: request.Credential,
		AfterContentHash:    request.After,
		MaxBytes:            p.maxBytes,
		Authenticate:        authenticateSnapshotCredential,
	})
	if err != nil {
		return SnapshotResult{}, ErrSnapshotDenied
	}
	if request.After != "" && !validSnapshotHash(request.After) {
		return SnapshotResult{}, ErrSnapshotDenied
	}
	if publication.Principal.CredentialID != credentialID ||
		!validCanonicalUUID(publication.Principal.NodeID) {
		return SnapshotResult{}, ErrSnapshotDenied
	}
	if !matchingPrimaryIdentity(currentIdentity, publication.Identity) || !publication.Identity.ClusterKeyUsable {
		return SnapshotResult{}, ErrSnapshotDenied
	}
	if !validSnapshotMetadata(publication.Snapshot) ||
		publication.Snapshot.SourcePrimaryID != string(currentIdentity.InstallationID) ||
		publication.Snapshot.LeadershipGeneration != int64(currentIdentity.LeadershipGeneration) {
		return SnapshotResult{}, ErrSnapshotDenied
	}
	if publication.Current {
		if request.After == "" || request.After != publication.Snapshot.ContentHash || len(publication.Snapshot.Bytes) != 0 {
			return SnapshotResult{}, ErrSnapshotDenied
		}
		return SnapshotResult{Current: true, Metadata: toSnapshotMetadata(publication.Snapshot)}, nil
	}
	if request.After != "" && request.After == publication.Snapshot.ContentHash {
		return SnapshotResult{}, ErrSnapshotDenied
	}
	if len(publication.Snapshot.Bytes) == 0 || len(publication.Snapshot.Bytes) > p.maxBytes ||
		!validSnapshotEnvelope(publication.Snapshot.Bytes, publication.Snapshot, currentIdentity) {
		return SnapshotResult{}, ErrSnapshotDenied
	}
	return SnapshotResult{
		Metadata: toSnapshotMetadata(publication.Snapshot),
		Bytes:    append([]byte(nil), publication.Snapshot.Bytes...),
	}, nil
}

func readyPrimaryIdentity(current identity.Identity) bool {
	if !current.InstallationID.IsValid() || !current.NodeID.IsValid() || current.IsStalePrimary() ||
		current.LeadershipGeneration <= 0 || current.LatestKnownGeneration != current.LeadershipGeneration {
		return false
	}
	return current.Role == domain.TopologyRolePrimary || current.Role == domain.TopologyRolePrimaryWithNodes
}

func matchingPrimaryIdentity(current identity.Identity, stored configuration.SnapshotPublicationIdentityRecord) bool {
	return stored.InstallationID == string(current.InstallationID) &&
		stored.NodeID == string(current.NodeID) &&
		stored.Role == current.Role &&
		stored.LeadershipGeneration == int64(current.LeadershipGeneration) &&
		stored.LatestKnownGeneration == int64(current.LatestKnownGeneration) &&
		equalClusterKeyID(current.ClusterKeyID, stored.ClusterKeyID)
}

func equalClusterKeyID(current, stored *uuid.UUID) bool {
	if current == nil || stored == nil {
		return current == nil && stored == nil
	}
	return *current == *stored
}

func validSnapshotMetadata(snapshot configuration.SnapshotPublicationRecord) bool {
	return snapshot.ProofComplete && snapshot.DiscardedAt == nil &&
		validCanonicalUUID(snapshot.SnapshotID) &&
		validSnapshotHash(snapshot.ContentHash) &&
		snapshot.SnapshotVersion > 0 && snapshot.ReplicationVersion > 0 &&
		validCanonicalUUID(snapshot.RevisionID) && snapshot.RevisionNumber > 0 &&
		validCanonicalUUID(snapshot.SourcePrimaryID) && snapshot.LeadershipGeneration > 0 &&
		validCanonicalUUID(snapshot.ApplyJobID) && !snapshot.AppliedAt.IsZero()
}

func validSnapshotEnvelope(
	value []byte,
	publication configuration.SnapshotPublicationRecord,
	current identity.Identity,
) bool {
	envelope, err := replicationsnapshot.Unmarshal(value)
	if err != nil {
		return false
	}
	if err := envelope.Validate(); err != nil {
		return false
	}
	if err := replicationsnapshot.CheckCompatibility(envelope); err != nil {
		return false
	}
	expectedHash, err := envelope.ExpectedHash()
	if err != nil || envelope.ContentHash != expectedHash || envelope.ContentHash != publication.ContentHash {
		return false
	}
	if envelope.Transient.SnapshotVersion != domain.SnapshotVersion(publication.SnapshotVersion) ||
		envelope.Transient.ReplicationVersion != domain.ReplicationVersion(publication.ReplicationVersion) ||
		envelope.Transient.SourcePrimaryID.String() != publication.SourcePrimaryID ||
		envelope.Transient.LeadershipGeneration != domain.LeadershipGeneration(publication.LeadershipGeneration) {
		return false
	}
	return envelope.NodeLocal.NodeID == current.NodeID &&
		envelope.NodeLocal.Role == current.Role &&
		envelope.NodeLocal.LeadershipGeneration == current.LeadershipGeneration &&
		equalClusterKeyID(current.ClusterKeyID, envelope.NodeLocal.ClusterKeyRef)
}

func validSnapshotHash(value string) bool {
	if len(value) != SnapshotContentHashHexLength || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validCanonicalUUID(value string) bool {
	parsed, err := uuid.Parse(value)
	return err == nil && parsed != uuid.Nil && parsed.String() == value
}

func toSnapshotMetadata(snapshot configuration.SnapshotPublicationRecord) SnapshotMetadata {
	return SnapshotMetadata{
		SnapshotID:           snapshot.SnapshotID,
		ContentHash:          snapshot.ContentHash,
		SnapshotVersion:      snapshot.SnapshotVersion,
		ReplicationVersion:   snapshot.ReplicationVersion,
		RevisionID:           snapshot.RevisionID,
		RevisionNumber:       snapshot.RevisionNumber,
		SourcePrimaryID:      snapshot.SourcePrimaryID,
		LeadershipGeneration: snapshot.LeadershipGeneration,
		ApplyJobID:           snapshot.ApplyJobID,
		AppliedAt:            snapshot.AppliedAt,
	}
}

func authenticateSnapshotCredential(
	presented string,
	record configuration.SnapshotPublicationCredentialRecord,
) (configuration.SnapshotPublicationPrincipal, error) {
	revokedAt := record.CredentialRevokedAt
	if revokedAt == nil {
		revokedAt = record.NodeRevokedAt
	}
	principal, err := AuthenticateNodeCredential(presented, &NodeCredentialRecord{
		ID:          record.ID,
		NodeID:      record.NodeID,
		Hash:        record.CredentialHash,
		HashVersion: record.HashVersion,
		RevokedAt:   revokedAt,
	})
	if err != nil {
		return configuration.SnapshotPublicationPrincipal{}, ErrSnapshotDenied
	}
	return configuration.SnapshotPublicationPrincipal{
		NodeID:       principal.NodeID,
		CredentialID: principal.CredentialID,
	}, nil
}

func (r SnapshotRequest) String() string { return "[snapshot request credential redacted]" }
