package configuration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/zerkc/ProxyCore/apps/api/internal/snapshot"
)

// ApplyTerminalStatus is the small status projection needed by the NODE
// conversion waiter. The worker's error_message is projected as FailureCode
// without adding a second apply_jobs schema column.
type ApplyTerminalStatus string

const ApplyTerminalStatusApplied ApplyTerminalStatus = "applied"

const ApplyTerminalStatusFailed ApplyTerminalStatus = "failed"

const ApplyTerminalStatusRolledBack ApplyTerminalStatus = "rolled-back"

// ApplyJobTerminal is a read-only projection of an apply job's terminal state.
type ApplyJobTerminal struct {
	Status      ApplyTerminalStatus
	FinishedAt  *time.Time
	FailureCode *string
}

// TerminalApplyReader reads the current durable state of one apply job.
type TerminalApplyReader interface {
	GetApplyJobTerminal(context.Context, uuid.UUID) (ApplyJobTerminal, error)
}

// ApplyWaitOptions controls the bounded terminal apply poller. The sync
// package aliases this lower-level contract so configuration stores can
// implement the reader without importing the sync package back.
type ApplyWaitOptions struct {
	PollInterval time.Duration
	Timeout      time.Duration
	Clock        func() time.Time
}

const DefaultApplyWaitPollInterval = 250 * time.Millisecond

const DefaultApplyWaitTimeout = 60 * time.Second

var ErrApplyWaitTimeout = errors.New("apply wait timed out")

var ErrApplyWaitJobIDRequired = errors.New("apply wait job id is required")

var ErrApplyWaitStoreRequired = errors.New("apply wait store is required")

var ErrApplyWaitContextRequired = errors.New("apply wait context is required")

// ApplyWaitTimeoutError preserves the last durable observation while
// remaining compatible with errors.Is(err, ErrApplyWaitTimeout).
type ApplyWaitTimeoutError struct {
	Last ApplyJobTerminal
}

func (e *ApplyWaitTimeoutError) Error() string {
	if e == nil {
		return ErrApplyWaitTimeout.Error()
	}
	return fmt.Sprintf("%s: last status %q", ErrApplyWaitTimeout, e.Last.Status)
}

func (e *ApplyWaitTimeoutError) Unwrap() error { return ErrApplyWaitTimeout }

// WaitForTerminalApply polls immediately, then waits between observations
// with a timer. It never sleeps or hides reader/context errors.
func WaitForTerminalApply(ctx context.Context, store TerminalApplyReader, jobID uuid.UUID, opts ApplyWaitOptions) (ApplyJobTerminal, error) {
	if ctx == nil {
		return ApplyJobTerminal{}, ErrApplyWaitContextRequired
	}
	if store == nil {
		return ApplyJobTerminal{}, ErrApplyWaitStoreRequired
	}
	if jobID == uuid.Nil {
		return ApplyJobTerminal{}, ErrApplyWaitJobIDRequired
	}
	pollInterval := opts.PollInterval
	if pollInterval <= 0 {
		pollInterval = DefaultApplyWaitPollInterval
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultApplyWaitTimeout
	}
	clock := opts.Clock
	if clock == nil {
		clock = time.Now
	}
	deadline := clock().Add(timeout)
	var last ApplyJobTerminal
	for {
		if err := ctx.Err(); err != nil {
			return last, err
		}
		observed, err := store.GetApplyJobTerminal(ctx, jobID)
		if err != nil {
			return last, err
		}
		last = observed
		if isTerminalApplyStatus(observed.Status) {
			return observed, nil
		}
		remaining := deadline.Sub(clock())
		if remaining <= 0 {
			return last, &ApplyWaitTimeoutError{Last: last}
		}
		delay := pollInterval
		if remaining < delay {
			delay = remaining
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if err := ctx.Err(); err != nil {
				timer.Stop()
				return last, err
			}
		case <-timer.C:
		}
		timer.Stop()
	}
}

func isTerminalApplyStatus(status ApplyTerminalStatus) bool {
	switch status {
	case ApplyTerminalStatusApplied, ApplyTerminalStatusFailed, ApplyTerminalStatusRolledBack:
		return true
	default:
		return false
	}
}

type importedSnapshotFields struct {
	sourcePrimaryID      uuid.UUID
	sourceNodeID         uuid.UUID
	snapshotHash         string
	contentHash          string
	snapshotVersion      int
	replicationVersion   int
	leadershipGeneration int64
}

// importedSnapshotMetadata parses only the fields required for persistence.
// Cryptographic and content validation remains the caller/validator boundary.
func importedSnapshotMetadata(snapshotBytes []byte) (importedSnapshotFields, error) {
	if len(snapshotBytes) == 0 {
		return importedSnapshotFields{}, errors.New("import snapshot is empty")
	}
	env, err := snapshot.Unmarshal(snapshotBytes)
	if err != nil {
		return importedSnapshotFields{}, err
	}
	if env.Transient.SourcePrimaryID == uuid.Nil || !env.NodeLocal.NodeID.IsValid() {
		return importedSnapshotFields{}, errors.New("import snapshot has invalid source identity")
	}
	if !env.Transient.SnapshotVersion.IsValid() || !env.Transient.ReplicationVersion.IsValid() || env.Transient.LeadershipGeneration == 0 {
		return importedSnapshotFields{}, errors.New("import snapshot has invalid transient metadata")
	}
	const maxInt64 = uint64(^uint64(0) >> 1)
	if uint64(env.Transient.LeadershipGeneration) > maxInt64 {
		return importedSnapshotFields{}, errors.New("import snapshot leadership generation exceeds PostgreSQL range")
	}
	if env.ContentHash == "" {
		return importedSnapshotFields{}, errors.New("import snapshot content hash is empty")
	}
	sourceNodeID, err := uuid.Parse(env.NodeLocal.NodeID.String())
	if err != nil {
		return importedSnapshotFields{}, fmt.Errorf("parse import source node id: %w", err)
	}
	checksum := sha256.Sum256(snapshotBytes)
	return importedSnapshotFields{
		sourcePrimaryID:      env.Transient.SourcePrimaryID,
		sourceNodeID:         sourceNodeID,
		snapshotHash:         hex.EncodeToString(checksum[:]),
		contentHash:          env.ContentHash,
		snapshotVersion:      int(env.Transient.SnapshotVersion),
		replicationVersion:   int(env.Transient.ReplicationVersion),
		leadershipGeneration: int64(env.Transient.LeadershipGeneration),
	}, nil
}
