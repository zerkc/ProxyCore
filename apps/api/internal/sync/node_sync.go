package sync

import (
	"context"
	"errors"
	"math/rand/v2"
	gosync "sync"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	replicationsnapshot "github.com/zerkc/ProxyCore/apps/api/internal/snapshot"
)

const (
	// DefaultNodeSyncInterval is how often a NODE polls its primary.
	DefaultNodeSyncInterval = 30 * time.Second
	// DefaultNodeSyncMaxBackoff caps the failure backoff.
	DefaultNodeSyncMaxBackoff = 10 * time.Minute
)

var (
	// ErrNodeSyncNotNode means the installation is not currently a NODE, so a
	// cycle did nothing.
	ErrNodeSyncNotNode = errors.New("node sync: installation is not a node")
	// ErrNodeSyncUnbound means the node has no primary binding to pull from.
	ErrNodeSyncUnbound = errors.New("node sync: node is not bound to a primary")
	// ErrNodeSyncBusy means another cycle is already running.
	ErrNodeSyncBusy = errors.New("node sync: a cycle is already running")
)

// NodeSyncFailure is a sanitized cycle failure. Code and Message come from a
// fixed vocabulary; neither ever contains transport, credential or payload
// text, so both are safe to persist and to return over HTTP.
type NodeSyncFailure struct {
	Code    string
	Message string
}

func (f *NodeSyncFailure) Error() string { return f.Message }

var (
	failCredentialRejected = &NodeSyncFailure{"credential_rejected", "The primary rejected this node's credential; re-enroll this node against the primary."}
	failPrimaryUnavailable = &NodeSyncFailure{"primary_unavailable", "The primary could not be reached."}
	failNoSnapshot         = &NodeSyncFailure{"snapshot_unavailable", "The primary has no snapshot published for this node yet."}
	failPrimaryInvalid     = &NodeSyncFailure{"primary_invalid", "The primary's response was invalid."}
	failIntegrity          = &NodeSyncFailure{"integrity_failed", "The primary's snapshot failed its integrity check."}
	failApply              = &NodeSyncFailure{"apply_failed", "The primary's snapshot could not be applied."}
	failAcknowledge        = &NodeSyncFailure{"acknowledge_failed", "The primary did not accept the acknowledgement."}
	failStore              = &NodeSyncFailure{"state_unavailable", "The node's sync state could not be saved."}
	// failUnlinked is produced only from the primary's explicit NODE_UNLINKED
	// answer, or from the persisted unlinked state it created.
	failUnlinked = &NodeSyncFailure{"node_unlinked", "The primary unlinked this node; it keeps serving its last configuration. Re-enroll it or convert it to a primary."}
)

// NodeSyncFetch reads the primary's latest snapshot, conditional on after (the
// node's last applied primary hash). The credential never crosses this seam.
type NodeSyncFetch func(ctx context.Context, state configuration.NodeSyncState, after string) (SnapshotFetchResult, error)

// NodeSyncApply validates and applies one verified envelope exactly like the
// enrollment import path (materialize tables, secrets, owners, apply job) and
// returns only when the apply reached a terminal success.
type NodeSyncApply func(ctx context.Context, envelope *replicationsnapshot.Envelope) error

// NodeSyncAcknowledge reports an applied primary hash back to the primary.
type NodeSyncAcknowledge func(ctx context.Context, state configuration.NodeSyncState, contentHash string, appliedAt time.Time) error

type NodeSyncOptions struct {
	Store       configuration.NodeSyncStore
	Role        func() domain.TopologyRole
	Fetch       NodeSyncFetch
	Apply       NodeSyncApply
	Acknowledge NodeSyncAcknowledge
	// Interval is the poll period (default DefaultNodeSyncInterval); it is also
	// the base of the failure backoff, which doubles up to MaxBackoff.
	Interval   time.Duration
	MaxBackoff time.Duration
	Now        func() time.Time
	// Jitter returns a value in [0,1); the delay is scaled by 0.8 + 0.4*value.
	Jitter func() float64
	// OnUnlinked is called with true when the node is known unlinked: after
	// the explicit signal was persisted, or when persisted state says so (so a
	// restart restores it). It is never called with a value derived from an
	// error. Optional.
	OnUnlinked func(unlinked bool)
	// Logf receives non-sensitive operational lines.
	Logf func(format string, args ...any)
}

// NodeSyncer keeps a NODE a near-exact copy of its primary. One instance runs
// at most one cycle at a time.
type NodeSyncer struct {
	opts NodeSyncOptions
	run  gosync.Mutex
	wait func(context.Context, time.Duration) error

	mu        gosync.Mutex
	nextDelay time.Duration
}

func NewNodeSyncer(opts NodeSyncOptions) *NodeSyncer {
	if opts.Interval <= 0 {
		opts.Interval = DefaultNodeSyncInterval
	}
	if opts.MaxBackoff <= 0 {
		opts.MaxBackoff = DefaultNodeSyncMaxBackoff
	}
	if opts.MaxBackoff < opts.Interval {
		opts.MaxBackoff = opts.Interval
	}
	if opts.Now == nil {
		opts.Now = func() time.Time { return time.Now().UTC() }
	}
	if opts.Jitter == nil {
		opts.Jitter = rand.Float64
	}
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}
	return &NodeSyncer{opts: opts, wait: nodeSyncWait, nextDelay: opts.Interval}
}

func nodeSyncWait(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Run polls until ctx is cancelled. Cycles on an installation that is not (or
// no longer) a NODE are skipped, so a promoted or demoted installation goes
// quiet and resumes by itself after it enrolls again.
func (s *NodeSyncer) Run(ctx context.Context) {
	for ctx.Err() == nil {
		err := s.RunOnce(ctx)
		var failure *NodeSyncFailure
		switch {
		case err == nil:
		case errors.As(err, &failure):
			s.opts.Logf("node sync cycle failed: %s", failure.Code)
		}
		if s.wait(ctx, s.delay()) != nil {
			return
		}
	}
}

func (s *NodeSyncer) delay() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.nextDelay
}

func (s *NodeSyncer) setDelay(d time.Duration) {
	s.mu.Lock()
	s.nextDelay = d
	s.mu.Unlock()
}

// Retry runs one cycle now (manual trigger). It shares RunOnce's guarantees.
func (s *NodeSyncer) Retry(ctx context.Context) error { return s.RunOnce(ctx) }

// RunOnce runs one sync cycle. It returns nil on success, ErrNodeSyncNotNode,
// ErrNodeSyncUnbound or ErrNodeSyncBusy when it did nothing, a context error
// when cancelled, and a *NodeSyncFailure otherwise.
func (s *NodeSyncer) RunOnce(ctx context.Context) error {
	if s == nil || s.opts.Store == nil || s.opts.Role == nil || s.opts.Fetch == nil || s.opts.Apply == nil {
		return ErrNodeSyncUnbound
	}
	if !s.run.TryLock() {
		return ErrNodeSyncBusy
	}
	defer s.run.Unlock()
	s.setDelay(s.opts.Interval)

	if s.opts.Role() != domain.TopologyRoleNode {
		return ErrNodeSyncNotNode
	}
	state, bound, err := s.opts.Store.LoadNodeSyncState(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return failStore
	}
	if !bound {
		return ErrNodeSyncUnbound
	}

	if state.Unlinked {
		// Authoritative and persisted: make no primary request at all. The
		// loop only wakes every MaxBackoff to re-read local state, so a
		// re-enrollment (which clears the flag) resumes polling by itself.
		s.setDelay(s.opts.MaxBackoff)
		s.notifyUnlinked(true)
		return failUnlinked
	}

	now := s.opts.Now().UTC()
	state.LastAttemptAt = now
	failure := s.cycle(ctx, &state)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if failure == failUnlinked {
		state.Unlinked = true
		state.UnlinkedAt = now
		state.ConsecutiveFailures = 0
		state.LastError = failure.Message
		state.NextAttemptAt = now.Add(s.opts.MaxBackoff)
		s.setDelay(s.opts.MaxBackoff)
	} else if failure != nil {
		state.ConsecutiveFailures++
		state.LastError = failure.Message
		delay := s.backoff(state.ConsecutiveFailures)
		state.NextAttemptAt = now.Add(delay)
		s.setDelay(delay)
	} else {
		state.ConsecutiveFailures = 0
		state.LastError = ""
		state.LastSuccessAt = now
		state.NextAttemptAt = now.Add(s.opts.Interval)
	}
	if err := s.opts.Store.SaveNodeSyncProgress(ctx, state.NodeSyncProgress); err != nil {
		if errors.Is(err, configuration.ErrNodeSyncUnbound) {
			return ErrNodeSyncUnbound
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return failStore
	}
	if failure == failUnlinked {
		s.notifyUnlinked(true)
	}
	if failure != nil {
		return failure
	}
	return nil
}

func (s *NodeSyncer) notifyUnlinked(unlinked bool) {
	if s.opts.OnUnlinked != nil {
		s.opts.OnUnlinked(unlinked)
	}
}

// cycle mutates state in memory and returns the sanitized failure, if any.
func (s *NodeSyncer) cycle(ctx context.Context, state *configuration.NodeSyncState) *NodeSyncFailure {
	after := state.AppliedHash
	fetched, err := s.opts.Fetch(ctx, *state, after)
	if err != nil {
		return classifyNodeSyncFetchError(err)
	}
	if fetched.NotModified {
		hash := fetched.ContentHash
		if hash == "" {
			hash = after
		}
		if hash == "" || (after != "" && hash != after) {
			return failPrimaryInvalid
		}
		state.PrimaryHash = hash
		if !fetched.AppliedAt.IsZero() {
			state.PrimaryAppliedAt = fetched.AppliedAt
		}
	} else {
		envelope, err := replicationsnapshot.Unmarshal(fetched.Body)
		if err != nil || envelope.VerifyHash() != nil {
			return failIntegrity
		}
		if fetched.ContentHash != "" && fetched.ContentHash != envelope.ContentHash {
			return failIntegrity
		}
		state.PrimaryHash = envelope.ContentHash
		if !fetched.AppliedAt.IsZero() {
			state.PrimaryAppliedAt = fetched.AppliedAt
		}
		if envelope.ContentHash != state.AppliedHash {
			// Record what the primary offers before the (possibly slow) apply,
			// so the node reports itself outdated while it is catching up.
			if err := s.opts.Store.SaveNodeSyncProgress(ctx, state.NodeSyncProgress); err != nil && ctx.Err() == nil {
				return failStore
			}
			if err := s.opts.Apply(ctx, &envelope); err != nil {
				return failApply
			}
			state.AppliedHash = envelope.ContentHash
		}
	}

	if s.opts.Acknowledge != nil && state.AppliedHash != "" && state.AckedHash != state.AppliedHash &&
		state.PrimaryHash == state.AppliedHash && !state.PrimaryAppliedAt.IsZero() {
		if err := s.opts.Acknowledge(ctx, *state, state.AppliedHash, state.PrimaryAppliedAt); err != nil {
			if errors.Is(err, ErrSnapshotClientUnlinked) {
				return failUnlinked
			}
			return failAcknowledge
		}
		state.AckedHash = state.AppliedHash
	}
	return nil
}

func classifyNodeSyncFetchError(err error) *NodeSyncFailure {
	switch {
	case errors.Is(err, ErrSnapshotClientUnlinked):
		return failUnlinked
	case errors.Is(err, ErrSnapshotClientUnauthorized), errors.Is(err, ErrSnapshotClientGone):
		return failCredentialRejected
	case errors.Is(err, ErrSnapshotClientNotFound):
		return failNoSnapshot
	case errors.Is(err, ErrSnapshotClientInvalid):
		return failPrimaryInvalid
	default:
		return failPrimaryUnavailable
	}
}

// backoff returns Interval * 2^(failures-1), jittered and capped.
func (s *NodeSyncer) backoff(failures int) time.Duration {
	if failures < 1 {
		failures = 1
	}
	shift := failures - 1
	if shift > 20 {
		shift = 20
	}
	delay := s.opts.Interval << shift
	delay = time.Duration(float64(delay) * (0.8 + 0.4*s.opts.Jitter()))
	if delay > s.opts.MaxBackoff {
		delay = s.opts.MaxBackoff
	}
	if delay < s.opts.Interval/2 {
		delay = s.opts.Interval / 2
	}
	return delay
}
