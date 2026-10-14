package configuration

import (
	"context"
	"time"
)

// NodeSyncBinding is the durable relationship a NODE needs to keep pulling the
// primary's snapshots: where the primary is, which credential authenticates
// this node and which primary installation issued it.
type NodeSyncBinding struct {
	PrimaryURL            string
	CredentialID          string
	PrimaryInstallationID string
}

// NodeSyncProgress is the loop's bookkeeping. Hashes are the primary's snapshot
// contentHash exactly as received. A zero time means "never".
type NodeSyncProgress struct {
	// AppliedHash is the last primary hash this node imported and applied.
	AppliedHash string
	// PrimaryHash is the latest primary hash this node has observed.
	PrimaryHash string
	// AckedHash is the last hash the primary confirmed it recorded.
	AckedHash string
	// PrimaryAppliedAt is the primary's applied_at for PrimaryHash; the
	// acknowledgement must echo it back.
	PrimaryAppliedAt    time.Time
	LastAttemptAt       time.Time
	LastSuccessAt       time.Time
	NextAttemptAt       time.Time
	ConsecutiveFailures int
	// LastError is a sanitized, fixed-vocabulary message, never raw error text.
	LastError string
}

// NodeSyncState is binding plus progress.
type NodeSyncState struct {
	NodeSyncBinding
	NodeSyncProgress
}

// Outdated reports whether the node has not applied the primary's latest
// observed snapshot. A node that never observed the primary is outdated too.
func (s NodeSyncProgress) Outdated() bool {
	return s.AppliedHash == "" || s.AppliedHash != s.PrimaryHash
}

// NodeSyncStore persists the node sync binding and progress.
type NodeSyncStore interface {
	// BindNodeSync records the binding after a successful enrollment, with the
	// enrollment snapshot as the applied (and observed) hash.
	BindNodeSync(ctx context.Context, binding NodeSyncBinding, appliedHash string, now time.Time) error
	// LoadNodeSyncState returns bound=false when the installation has no
	// binding (never enrolled, promoted or demoted).
	LoadNodeSyncState(ctx context.Context) (state NodeSyncState, bound bool, err error)
	SaveNodeSyncProgress(ctx context.Context, progress NodeSyncProgress) error
}
