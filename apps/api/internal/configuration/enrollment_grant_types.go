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

// PgPhase2Store is the PostgreSQL implementation of the continuity
// transaction port. It intentionally exposes only typed operations; callers
// cannot bypass the row-lock/compare-and-set boundary with arbitrary SQL.
const EnrollmentTokenHashVersion = "sha256-v1"

var errEnrollmentTokenDenied = errors.New("enrollment token denied")
