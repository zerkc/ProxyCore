package configuration

import (
	"encoding/json"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
)

// JobRecord is an apply job row.
type JobRecord struct {
	ID               string          `json:"id"`
	RevisionID       string          `json:"revisionId"`
	ActorUserID      *string         `json:"actorUserId,omitempty"`
	Target           string          `json:"target"`
	Status           string          `json:"status"`
	CorrelationID    string          `json:"correlationId"`
	CreatedAt        time.Time       `json:"createdAt"`
	ClaimedAt        *time.Time      `json:"claimedAt,omitempty"`
	StartedAt        *time.Time      `json:"startedAt,omitempty"`
	FinishedAt       *time.Time      `json:"finishedAt,omitempty"`
	ValidationOutput json.RawMessage `json:"validationOutput,omitempty"`
	ApplyOutput      json.RawMessage `json:"applyOutput,omitempty"`
	HealthOutput     json.RawMessage `json:"healthOutput,omitempty"`
	ErrorMessage     *string         `json:"errorMessage,omitempty"`
}

// RevisionRecord is a config revision row.
type RevisionRecord struct {
	ID             string     `json:"id"`
	RevisionNumber int        `json:"revisionNumber"`
	Checksum       string     `json:"checksum"`
	Snapshot       any        `json:"snapshot"`
	ActorUserID    *string    `json:"actorUserId,omitempty"`
	CreatedAt      time.Time  `json:"createdAt"`
	AppliedAt      *time.Time `json:"appliedAt,omitempty"`
}

// ApplyResult is the {revisionId, job} returned by an enqueue.
type ApplyResult struct {
	RevisionID string    `json:"revisionId"`
	Job        JobRecord `json:"job"`
}

// AutoApplyResult wraps a mutated value with its enqueued apply job.
type AutoApplyResult[T any] struct {
	Value T
	Apply ApplyResult
}

// CloudflareInput carries Cloudflare DNS-01 credentials.
type CloudflareInput struct {
	APIToken string
	ZoneID   string
	ZoneName string
}

// CertificateIssueInput mirrors the Node CertificateIssueInput.
type CertificateIssueInput struct {
	Hostnames          []string
	Issuer             string
	Challenge          string
	Environment        string
	Email              string
	KeyType            string
	PropagationSeconds *int
	DirectoryURL       string
	CertificatePEM     string
	PrivateKeyPEM      string
	Cloudflare         *CloudflareInput
}

// StatusResult is the aggregate returned by status().
type StatusResult struct {
	DesiredRevision *RevisionRecord            `json:"desiredRevision"`
	AppliedRevision *RevisionRecord            `json:"appliedRevision"`
	Jobs            []JobRecord                `json:"jobs"`
	Settings        domain.Settings            `json:"settings"`
	Zones           []domain.ZoneState         `json:"zones"`
	Streams         []domain.StreamRoute       `json:"streams"`
	Certificates    []domain.CertificateStatus `json:"certificates"`
}
