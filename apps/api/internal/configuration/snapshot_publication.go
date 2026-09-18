package configuration

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	replicationsnapshot "github.com/zerkc/ProxyCore/apps/api/internal/snapshot"
)

const SnapshotPublicationMaxBytes = 8 << 20

var (
	ErrSnapshotPublicationStore  = errors.New("snapshot publication store denied")
	ErrCanonicalSnapshotNotFound = errors.New("canonical snapshot not found")
	ErrCanonicalSnapshotConflict = errors.New("canonical snapshot conflict")
)

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

// CanonicalSnapshotApplyRef identifies a terminal worker apply that still
// needs producer reconciliation.
type CanonicalSnapshotApplyRef struct {
	JobID string
}

// CanonicalSnapshotApply is the locked desired-config and proof projection for
// one worker job. DesiredSnapshot remains the ordinary revision body; it is
// never replaced by the publication envelope.
type CanonicalSnapshotApply struct {
	JobID                    string
	RevisionID               string
	RevisionNumber           int
	DesiredSnapshot          []byte
	Status                   string
	Target                   string
	Source                   PersistenceSource
	SourcePrimaryID          *string
	SourceNodeID             *string
	SourceRevisionID         *string
	SnapshotContentHash      *string
	SnapshotVersion          *int
	ReplicationVersion       *int
	LeadershipGeneration     *int64
	FinishedAt               *time.Time
	RevisionSource           PersistenceSource
	RevisionSourcePrimaryID  *string
	RevisionSourceNodeID     *string
	RevisionSourceRevisionID *string
	RevisionSnapshotHash     *string
	RevisionSnapshotVersion  *int
	RevisionReplication      *int
	RevisionGeneration       *int64
	RevisionAppliedAt        *time.Time
}

// CanonicalSnapshotRecord is the immutable applied row plus its exact body.
// SnapshotBody is nil for legacy rows and must never be treated as publishable.
type CanonicalSnapshotRecord struct {
	AppliedSnapshotRecord
}

// CanonicalSnapshotWrite carries the producer's complete terminal proof and
// exact canonical envelope bytes. The desired revision body is not included.
type CanonicalSnapshotWrite struct {
	ID                   string
	SnapshotBody         []byte
	SourcePrimaryID      string
	LeadershipGeneration int64
	SnapshotVersion      int
	ReplicationVersion   int
	ContentHash          string
	RevisionID           string
	ApplyJobID           string
	AppliedAt            time.Time
}

// CanonicalSnapshotTransaction is the atomic producer boundary. Implementors
// must execute every method on one transaction and lock identity, job, and
// revision rows before exporting or publishing.
type CanonicalSnapshotTransaction interface {
	LockPrimaryIdentity(context.Context) (PrimaryIdentityRecord, error)
	LoadOrCreateClusterKEK(context.Context, string) (ClusterKeyMaterial, error)
	GetCanonicalSnapshotApply(context.Context, string) (CanonicalSnapshotApply, error)
	FindCanonicalSnapshotForJob(context.Context, string) (*CanonicalSnapshotRecord, error)
	replicationsnapshot.SecretLister
	replicationsnapshot.OwnerLister
	PublishCanonicalSnapshot(context.Context, CanonicalSnapshotWrite) error
}

// CanonicalSnapshotStore owns the producer transaction and candidate scan.
type CanonicalSnapshotStore interface {
	ListCanonicalSnapshotCandidates(context.Context) ([]CanonicalSnapshotApplyRef, error)
	WithCanonicalSnapshot(context.Context, func(CanonicalSnapshotTransaction) error) error
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
