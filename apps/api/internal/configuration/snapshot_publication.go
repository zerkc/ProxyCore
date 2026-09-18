package configuration

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
)

const SnapshotPublicationMaxBytes = 8 << 20

var ErrSnapshotPublicationStore = errors.New("snapshot publication store denied")

// SnapshotCredentialRecord is the current database projection used for one
// node-credential authorization check. It is loaded inside the same read
// transaction as the publication candidate; callers must not cache it.
type SnapshotPublicationCredentialRecord struct {
	ID                  string
	NodeID              string
	CredentialHash      string
	HashVersion         string
	CredentialRevokedAt *time.Time
	NodeRevokedAt       *time.Time
	PrimaryID           string
}

// SnapshotPublicationPrincipal contains only the non-secret identity granted
// by a successful node-credential check.
type SnapshotPublicationPrincipal struct {
	NodeID       string
	CredentialID string
}

// SnapshotPublicationCredentialAuthenticator adapts the sync package's
// credential primitive without making configuration depend on sync.
type SnapshotPublicationCredentialAuthenticator func(
	presented string,
	record SnapshotPublicationCredentialRecord,
) (SnapshotPublicationPrincipal, error)

// SnapshotPublicationRequest carries request-only authentication material and
// bounded selection controls. PresentedCredential is never persisted or
// serialized by the store.
type SnapshotPublicationRequest struct {
	CredentialID        string
	PresentedCredential string
	AfterContentHash    string
	MaxBytes            int
	Authenticate        SnapshotPublicationCredentialAuthenticator
}

// SnapshotPublicationIdentityRecord is the authoritative identity projection
// read in the publication transaction.
type SnapshotPublicationIdentityRecord struct {
	InstallationID        string
	NodeID                string
	Role                  domain.TopologyRole
	LeadershipGeneration  int64
	LatestKnownGeneration int64
	ClusterKeyID          *uuid.UUID
	ClusterKeyUsable      bool
}

// SnapshotPublicationRecord is the complete immutable publication candidate
// assembled from applied_snapshots, config_revisions, and apply_jobs. ContentHash
// is the canonical envelope hash, not a digest of the raw JSON bytes.
type SnapshotPublicationRecord struct {
	SnapshotID           string
	Bytes                []byte
	ContentHash          string
	SnapshotVersion      int
	ReplicationVersion   int
	RevisionID           string
	RevisionNumber       int
	SourcePrimaryID      string
	LeadershipGeneration int64
	ApplyJobID           string
	AppliedAt            time.Time
	DiscardedAt          *time.Time
	ProofComplete        bool
}

// SnapshotPublicationResult is returned only after current credential,
// topology, key-readiness, and terminal-apply checks pass.
type SnapshotPublicationResult struct {
	Current   bool
	Principal SnapshotPublicationPrincipal
	Identity  SnapshotPublicationIdentityRecord
	Snapshot  SnapshotPublicationRecord
}

// SnapshotPublicationHooks provides a narrow synchronization seam for tests
// that prove row-lock ordering. Production callers normally leave it empty.
type SnapshotPublicationHooks struct {
	AfterCredentialAuthorization func()
}

// SnapshotPublicationStore is the persistence boundary for the publisher. An
// implementation must authenticate and select the candidate in one consistent
// transaction, and must not return body bytes for a Current result.
type SnapshotPublicationStore interface {
	ReadSnapshotPublication(context.Context, SnapshotPublicationRequest) (SnapshotPublicationResult, error)
}

func validSnapshotPublicationHash(value string) bool {
	if len(value) != 64 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
