package configuration

import "time"

// PersistenceSource identifies who created a revision or apply job. It is
// deliberately non-secret and is shared with the worker persistence contract.
type PersistenceSource string

const (
	PersistenceSourceOrdinary PersistenceSource = "ordinary"
	PersistenceSourceImport   PersistenceSource = "import"
	PersistenceSourceSync     PersistenceSource = "sync"
)

func (s PersistenceSource) IsValid() bool {
	return s == PersistenceSourceOrdinary || s == PersistenceSourceImport || s == PersistenceSourceSync
}

// EnrollmentAttemptState is the durable local enrollment state machine.
type EnrollmentAttemptState string

const (
	EnrollmentAttemptDraft               EnrollmentAttemptState = "draft"
	EnrollmentAttemptVerified            EnrollmentAttemptState = "verified"
	EnrollmentAttemptConfirmed           EnrollmentAttemptState = "confirmed"
	EnrollmentAttemptExchanged           EnrollmentAttemptState = "exchanged"
	EnrollmentAttemptArchived            EnrollmentAttemptState = "archived"
	EnrollmentAttemptInitialApplyPending EnrollmentAttemptState = "initial-apply-pending"
	EnrollmentAttemptCommitted           EnrollmentAttemptState = "committed"
	EnrollmentAttemptCancelled           EnrollmentAttemptState = "cancelled"
	EnrollmentAttemptRecoverable         EnrollmentAttemptState = "recoverable"
	EnrollmentAttemptFailed              EnrollmentAttemptState = "failed"
)

func (s EnrollmentAttemptState) IsTerminal() bool {
	return s == EnrollmentAttemptCommitted || s == EnrollmentAttemptCancelled || s == EnrollmentAttemptFailed
}

func (s EnrollmentAttemptState) IsValid() bool {
	switch s {
	case EnrollmentAttemptDraft, EnrollmentAttemptVerified, EnrollmentAttemptConfirmed,
		EnrollmentAttemptExchanged, EnrollmentAttemptArchived, EnrollmentAttemptInitialApplyPending,
		EnrollmentAttemptCommitted, EnrollmentAttemptCancelled, EnrollmentAttemptRecoverable,
		EnrollmentAttemptFailed:
		return true
	default:
		return false
	}
}

// SyncTrigger identifies the bounded source of a synchronization attempt.
type SyncTrigger string

const (
	SyncTriggerEnrollment SyncTrigger = "enrollment"
	SyncTriggerPeriodic   SyncTrigger = "periodic"
	SyncTriggerManual     SyncTrigger = "manual"
	SyncTriggerRestart    SyncTrigger = "restart"
)

func (t SyncTrigger) IsValid() bool {
	switch t {
	case SyncTriggerEnrollment, SyncTriggerPeriodic, SyncTriggerManual, SyncTriggerRestart:
		return true
	default:
		return false
	}
}

// SyncAttemptStatus is the terminal/non-terminal result persisted for a sync.
type SyncAttemptStatus string

const (
	SyncAttemptQueued     SyncAttemptStatus = "queued"
	SyncAttemptRunning    SyncAttemptStatus = "running"
	SyncAttemptCurrent    SyncAttemptStatus = "current"
	SyncAttemptApplied    SyncAttemptStatus = "applied"
	SyncAttemptFailed     SyncAttemptStatus = "failed"
	SyncAttemptPendingAck SyncAttemptStatus = "pending-ack"
)

func (s SyncAttemptStatus) IsValid() bool {
	switch s {
	case SyncAttemptQueued, SyncAttemptRunning, SyncAttemptCurrent, SyncAttemptApplied, SyncAttemptFailed, SyncAttemptPendingAck:
		return true
	default:
		return false
	}
}

// AppliedSnapshotStatus records the candidate lifecycle independently from the
// worker's terminal apply status.
type AppliedSnapshotStatus string

const (
	AppliedSnapshotPending    AppliedSnapshotStatus = "pending"
	AppliedSnapshotApplied    AppliedSnapshotStatus = "applied"
	AppliedSnapshotRejected   AppliedSnapshotStatus = "rejected"
	AppliedSnapshotRolledBack AppliedSnapshotStatus = "rolled-back"
	AppliedSnapshotArchived   AppliedSnapshotStatus = "archived"
)

func (s AppliedSnapshotStatus) IsValid() bool {
	switch s {
	case AppliedSnapshotPending, AppliedSnapshotApplied, AppliedSnapshotRejected, AppliedSnapshotRolledBack, AppliedSnapshotArchived:
		return true
	default:
		return false
	}
}

// NodeStateRecord is the singleton cross-process state used by the API and the
// scheduler. Legacy EnrollmentTokenHash remains readable for rollback
// compatibility but new code does not populate it.
type NodeStateRecord struct {
	ID                    string     `json:"id"`
	EnrolledAt            *time.Time `json:"enrolledAt,omitempty"`
	EnrollmentTokenHash   *string    `json:"-"`
	EnrollmentPrimaryID   *string    `json:"enrollmentPrimaryId,omitempty"`
	LastSeenAt            *time.Time `json:"lastSeenAt,omitempty"`
	LastAppliedSnapshotID *string    `json:"lastAppliedSnapshotId,omitempty"`
	EnrollmentAttemptID   *string    `json:"enrollmentAttemptId,omitempty"`
	PrimaryURL            *string    `json:"primaryUrl,omitempty"`
	PrimaryInstallationID *string    `json:"primaryInstallationId,omitempty"`
	PrimaryTLSSPKISHA256  *string    `json:"primaryTlsSpkiSha256,omitempty"`
	CredentialID          *string    `json:"credentialId,omitempty"`
	SyncEnabled           bool       `json:"syncEnabled"`
	LastAttemptAt         *time.Time `json:"lastAttemptAt,omitempty"`
	LastSuccessAt         *time.Time `json:"lastSuccessAt,omitempty"`
	ConsecutiveFailures   int        `json:"consecutiveFailures"`
	NextAttemptAt         *time.Time `json:"nextAttemptAt,omitempty"`
	LastErrorCode         *string    `json:"lastErrorCode,omitempty"`
	UpdatedAt             time.Time  `json:"updatedAt"`
}

// EnrollmentAttemptRecord contains no plaintext token. Ephemeral private keys
// and bootstrap payloads are already locally wrapped/sealed before persistence.
type EnrollmentAttemptRecord struct {
	ID                           string                 `json:"id"`
	State                        EnrollmentAttemptState `json:"state"`
	PrimaryURL                   string                 `json:"primaryUrl"`
	ExpectedPrimaryID            *string                `json:"expectedPrimaryId,omitempty"`
	VerifiedPrimaryID            *string                `json:"verifiedPrimaryId,omitempty"`
	VerifiedPrimaryNodeID        *string                `json:"verifiedPrimaryNodeId,omitempty"`
	VerifiedLeadershipGeneration *int64                 `json:"verifiedLeadershipGeneration,omitempty"`
	VerifiedPrimaryTLSSPKISHA256 *string                `json:"verifiedPrimaryTlsSpkiSha256,omitempty"`
	VerifiedPrimaryCAFingerprint *string                `json:"verifiedPrimaryCaFingerprint,omitempty"`
	PreviewDigest                *string                `json:"previewDigest,omitempty"`
	LocalNodeIP                  string                 `json:"localNodeIp"`
	ArchiveID                    *string                `json:"archiveId,omitempty"`
	EphemeralPrivateKeyWrapped   string                 `json:"-"`
	BootstrapPayload             []byte                 `json:"-"`
	NodeCredentialSecretID       *string                `json:"nodeCredentialSecretId,omitempty"`
	ClusterKeyID                 *string                `json:"clusterKeyId,omitempty"`
	InitialSnapshotHash          *string                `json:"initialSnapshotHash,omitempty"`
	InitialSnapshotRevisionID    *string                `json:"initialSnapshotRevisionId,omitempty"`
	InitialApplyJobID            *string                `json:"initialApplyJobId,omitempty"`
	FailureCode                  *string                `json:"failureCode,omitempty"`
	ConfirmedAt                  *time.Time             `json:"confirmedAt,omitempty"`
	CreatedAt                    time.Time              `json:"createdAt"`
	UpdatedAt                    time.Time              `json:"updatedAt"`
}

// SyncAttemptRecord is bounded diagnostic history, not a mutation queue.
type SyncAttemptRecord struct {
	ID                   string            `json:"id"`
	NodeID               string            `json:"nodeId"`
	Trigger              SyncTrigger       `json:"trigger"`
	Status               SyncAttemptStatus `json:"status"`
	SourcePrimaryID      *string           `json:"sourcePrimaryId,omitempty"`
	LeadershipGeneration *int64            `json:"leadershipGeneration,omitempty"`
	SnapshotVersion      *int              `json:"snapshotVersion,omitempty"`
	ReplicationVersion   *int              `json:"replicationVersion,omitempty"`
	ContentHash          *string           `json:"contentHash,omitempty"`
	RevisionID           *string           `json:"revisionId,omitempty"`
	ApplyJobID           *string           `json:"applyJobId,omitempty"`
	ResultCode           *string           `json:"resultCode,omitempty"`
	StartedAt            *time.Time        `json:"startedAt,omitempty"`
	FinishedAt           *time.Time        `json:"finishedAt,omitempty"`
	CreatedAt            time.Time         `json:"createdAt"`
	UpdatedAt            time.Time         `json:"updatedAt"`
}

// AppliedSnapshotRecord proves which snapshot candidate reached a terminal
// local state and links it to the worker job when one exists.
type AppliedSnapshotRecord struct {
	ID                   string                `json:"id"`
	SourcePrimaryID      string                `json:"sourcePrimaryId"`
	LeadershipGeneration int64                 `json:"leadershipGeneration"`
	SnapshotVersion      int                   `json:"snapshotVersion"`
	ReplicationVersion   int                   `json:"replicationVersion"`
	ContentHash          string                `json:"contentHash"`
	RevisionID           *string               `json:"revisionId,omitempty"`
	Status               AppliedSnapshotStatus `json:"status"`
	ApplyJobID           *string               `json:"applyJobId,omitempty"`
	FailureCode          *string               `json:"failureCode,omitempty"`
	AppliedAt            time.Time             `json:"appliedAt"`
	DiscardedAt          *time.Time            `json:"discardedAt,omitempty"`
}

// SnapshotAcknowledgement is the durable outbox tuple sent only after the
// linked apply job is terminal applied.
type SnapshotAcknowledgement struct {
	NodeID               string    `json:"nodeId"`
	ContentHash          string    `json:"contentHash"`
	SnapshotVersion      int       `json:"snapshotVersion"`
	ReplicationVersion   int       `json:"replicationVersion"`
	RevisionID           string    `json:"revisionId"`
	LeadershipGeneration int64     `json:"leadershipGeneration"`
	AppliedAt            time.Time `json:"appliedAt"`
	ReceivedAt           time.Time `json:"receivedAt"`
}
