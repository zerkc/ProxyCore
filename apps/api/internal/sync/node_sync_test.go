package sync

import (
	"context"
	"errors"
	"strings"
	gosync "sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	replicationsnapshot "github.com/zerkc/ProxyCore/apps/api/internal/snapshot"
)

type memoryNodeSyncStore struct {
	mu       gosync.Mutex
	bound    bool
	state    configuration.NodeSyncState
	saveErr  error
	saveHits int
}

func (s *memoryNodeSyncStore) BindNodeSync(_ context.Context, b configuration.NodeSyncBinding, applied string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bound = true
	s.state = configuration.NodeSyncState{NodeSyncBinding: b, NodeSyncProgress: configuration.NodeSyncProgress{
		AppliedHash: applied, PrimaryHash: applied, LastSuccessAt: now,
	}}
	return nil
}

func (s *memoryNodeSyncStore) LoadNodeSyncState(context.Context) (configuration.NodeSyncState, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state, s.bound, nil
}

func (s *memoryNodeSyncStore) SaveNodeSyncProgress(_ context.Context, p configuration.NodeSyncProgress) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saveHits++
	if s.saveErr != nil {
		return s.saveErr
	}
	s.state.NodeSyncProgress = p
	return nil
}

func syncTestEnvelope(t *testing.T, marker string) (replicationsnapshot.Envelope, []byte) {
	t.Helper()
	envelope := replicationsnapshot.Envelope{
		Transient: replicationsnapshot.TransientFields{
			SnapshotVersion: 1, ReplicationVersion: 1, SourcePrimaryID: uuid.New(),
			LeadershipGeneration: 2, CapturedAt: time.Unix(1_700_000_000, 0).UTC(),
		},
		NodeLocal:  replicationsnapshot.NodeLocalFields{NodeID: domain.NodeID(uuid.NewString()), Role: domain.TopologyRoleNode},
		Replicated: replicationsnapshot.ReplicatedFields{Configuration: map[string]any{"marker": marker}},
	}
	hash, err := envelope.ExpectedHash()
	if err != nil {
		t.Fatalf("ExpectedHash: %v", err)
	}
	envelope.ContentHash = hash
	body, err := replicationsnapshot.Marshal(envelope)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return envelope, body
}

type nodeSyncHarness struct {
	t       *testing.T
	store   *memoryNodeSyncStore
	role    domain.TopologyRole
	now     time.Time
	syncer  *NodeSyncer
	fetches []string // `after` values seen
	applied []string
	acked   []string

	fetchFn func(after string) (SnapshotFetchResult, error)
	applyFn func(*replicationsnapshot.Envelope) error
	ackFn   func(hash string) error
}

func newNodeSyncHarness(t *testing.T, appliedHash string) *nodeSyncHarness {
	t.Helper()
	h := &nodeSyncHarness{t: t, store: &memoryNodeSyncStore{}, role: domain.TopologyRoleNode, now: time.Unix(1_800_000_000, 0).UTC()}
	if appliedHash != "" {
		_ = h.store.BindNodeSync(context.Background(), configuration.NodeSyncBinding{
			PrimaryURL: "https://primary.example:3443", CredentialID: uuid.NewString(),
		}, appliedHash, h.now)
		h.store.state.AckedHash = appliedHash
	}
	h.syncer = NewNodeSyncer(NodeSyncOptions{
		Store: h.store,
		Role:  func() domain.TopologyRole { return h.role },
		Fetch: func(_ context.Context, _ configuration.NodeSyncState, after string) (SnapshotFetchResult, error) {
			h.fetches = append(h.fetches, after)
			return h.fetchFn(after)
		},
		Apply: func(_ context.Context, e *replicationsnapshot.Envelope) error {
			h.applied = append(h.applied, e.ContentHash)
			if h.applyFn != nil {
				return h.applyFn(e)
			}
			return nil
		},
		Acknowledge: func(_ context.Context, _ configuration.NodeSyncState, hash string, _ time.Time) error {
			h.acked = append(h.acked, hash)
			if h.ackFn != nil {
				return h.ackFn(hash)
			}
			return nil
		},
		Interval:   30 * time.Second,
		MaxBackoff: 5 * time.Minute,
		Now:        func() time.Time { return h.now },
		Jitter:     func() float64 { return 0.5 }, // factor 1.0
	})
	return h
}

func TestNodeSyncAppliesNewSnapshotAndAcknowledges(t *testing.T) {
	envelope, body := syncTestEnvelope(t, "v2")
	h := newNodeSyncHarness(t, strings.Repeat("a", 64))
	appliedAt := h.now.Add(-time.Minute)
	h.fetchFn = func(string) (SnapshotFetchResult, error) {
		return SnapshotFetchResult{Body: body, ContentHash: envelope.ContentHash, AppliedAt: appliedAt}, nil
	}
	if err := h.syncer.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(h.fetches) != 1 || h.fetches[0] != strings.Repeat("a", 64) {
		t.Fatalf("fetch after = %v, want the node's last applied hash", h.fetches)
	}
	if len(h.applied) != 1 || h.applied[0] != envelope.ContentHash {
		t.Fatalf("applied = %v", h.applied)
	}
	if len(h.acked) != 1 || h.acked[0] != envelope.ContentHash {
		t.Fatalf("acked = %v", h.acked)
	}
	st := h.store.state.NodeSyncProgress
	if st.AppliedHash != envelope.ContentHash || st.PrimaryHash != envelope.ContentHash || st.AckedHash != envelope.ContentHash {
		t.Fatalf("state hashes = %+v", st)
	}
	if st.Outdated() || st.LastError != "" || st.ConsecutiveFailures != 0 || !st.LastSuccessAt.Equal(h.now) || !st.LastAttemptAt.Equal(h.now) {
		t.Fatalf("state = %+v", st)
	}
	if want := h.now.Add(30 * time.Second); !st.NextAttemptAt.Equal(want) {
		t.Fatalf("next attempt = %v, want %v", st.NextAttemptAt, want)
	}
}

func TestNodeSyncSkipsApplyWhenNotModified(t *testing.T) {
	hash := strings.Repeat("b", 64)
	h := newNodeSyncHarness(t, hash)
	h.fetchFn = func(string) (SnapshotFetchResult, error) {
		return SnapshotFetchResult{NotModified: true, ContentHash: hash, AppliedAt: h.now}, nil
	}
	if err := h.syncer.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(h.applied) != 0 || len(h.acked) != 0 {
		t.Fatalf("applied=%v acked=%v, want none (already applied and acknowledged)", h.applied, h.acked)
	}
	if st := h.store.state.NodeSyncProgress; st.PrimaryHash != hash || st.Outdated() || !st.LastSuccessAt.Equal(h.now) {
		t.Fatalf("state = %+v", st)
	}
}

func TestNodeSyncAcknowledgesEnrollmentSnapshotOnFirstCycle(t *testing.T) {
	hash := strings.Repeat("c", 64)
	h := newNodeSyncHarness(t, hash)
	h.store.state.AckedHash = "" // enrollment never acknowledged
	h.fetchFn = func(string) (SnapshotFetchResult, error) {
		return SnapshotFetchResult{NotModified: true, ContentHash: hash, AppliedAt: h.now}, nil
	}
	if err := h.syncer.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(h.acked) != 1 || h.acked[0] != hash || h.store.state.AckedHash != hash {
		t.Fatalf("acked=%v state=%+v", h.acked, h.store.state.NodeSyncProgress)
	}
}

func TestNodeSyncBacksOffAfterFailureAndResetsAfterSuccess(t *testing.T) {
	hash := strings.Repeat("d", 64)
	h := newNodeSyncHarness(t, hash)
	secret := "pcnode1_SUPER-SECRET-TOKEN"
	h.fetchFn = func(string) (SnapshotFetchResult, error) {
		return SnapshotFetchResult{}, errors.New("dial https://primary: " + secret)
	}
	var delays []time.Duration
	for i := 0; i < 4; i++ {
		err := h.syncer.RunOnce(context.Background())
		var failure *NodeSyncFailure
		if !errors.As(err, &failure) {
			t.Fatalf("RunOnce #%d error = %v, want *NodeSyncFailure", i, err)
		}
		st := h.store.state.NodeSyncProgress
		if strings.Contains(st.LastError, secret) || strings.Contains(failure.Error(), secret) || st.LastError == "" {
			t.Fatalf("last error not sanitized: %q / %q", st.LastError, failure.Error())
		}
		if st.ConsecutiveFailures != i+1 {
			t.Fatalf("failures = %d, want %d", st.ConsecutiveFailures, i+1)
		}
		delays = append(delays, st.NextAttemptAt.Sub(h.now))
		h.now = st.NextAttemptAt
	}
	want := []time.Duration{30 * time.Second, 60 * time.Second, 120 * time.Second, 240 * time.Second}
	for i := range want {
		if delays[i] != want[i] {
			t.Fatalf("delay[%d] = %v, want %v (all %v)", i, delays[i], want[i], delays)
		}
	}
	if err := h.syncer.RunOnce(context.Background()); err == nil {
		t.Fatal("fifth failure expected")
	}
	if got := h.store.state.NextAttemptAt.Sub(h.now); got != 5*time.Minute {
		t.Fatalf("delay not capped: %v", got)
	}
	h.fetchFn = func(string) (SnapshotFetchResult, error) {
		return SnapshotFetchResult{NotModified: true, ContentHash: hash, AppliedAt: h.now}, nil
	}
	if err := h.syncer.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce after recovery: %v", err)
	}
	st := h.store.state.NodeSyncProgress
	if st.ConsecutiveFailures != 0 || st.LastError != "" || st.NextAttemptAt.Sub(h.now) != 30*time.Second {
		t.Fatalf("backoff not reset: %+v", st)
	}
}

func TestNodeSyncIgnoresWhenRoleIsNotNode(t *testing.T) {
	h := newNodeSyncHarness(t, strings.Repeat("e", 64))
	h.role = domain.TopologyRolePrimary
	h.fetchFn = func(string) (SnapshotFetchResult, error) {
		t.Fatal("fetch must not run for a non-node role")
		return SnapshotFetchResult{}, nil
	}
	if err := h.syncer.RunOnce(context.Background()); !errors.Is(err, ErrNodeSyncNotNode) {
		t.Fatalf("RunOnce = %v, want ErrNodeSyncNotNode", err)
	}
	if h.store.saveHits != 0 {
		t.Fatalf("state written %d times for a non-node", h.store.saveHits)
	}
}

func TestNodeSyncUnboundNodeDoesNothing(t *testing.T) {
	h := newNodeSyncHarness(t, "")
	h.fetchFn = func(string) (SnapshotFetchResult, error) {
		t.Fatal("fetch must not run without a binding")
		return SnapshotFetchResult{}, nil
	}
	if err := h.syncer.RunOnce(context.Background()); !errors.Is(err, ErrNodeSyncUnbound) {
		t.Fatalf("RunOnce = %v, want ErrNodeSyncUnbound", err)
	}
}

func TestNodeSyncRejectsTamperedSnapshotWithoutApplying(t *testing.T) {
	envelope, _ := syncTestEnvelope(t, "v2")
	envelope.ContentHash = strings.Repeat("0", 64)
	body, err := replicationsnapshot.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	h := newNodeSyncHarness(t, strings.Repeat("a", 64))
	h.fetchFn = func(string) (SnapshotFetchResult, error) {
		return SnapshotFetchResult{Body: body, ContentHash: envelope.ContentHash, AppliedAt: h.now}, nil
	}
	if err := h.syncer.RunOnce(context.Background()); err == nil {
		t.Fatal("tampered snapshot must fail")
	}
	if len(h.applied) != 0 || h.store.state.AppliedHash != strings.Repeat("a", 64) {
		t.Fatalf("applied=%v state=%+v", h.applied, h.store.state.NodeSyncProgress)
	}
}

func TestNodeSyncApplyFailureKeepsAppliedHashAndMarksOutdated(t *testing.T) {
	envelope, body := syncTestEnvelope(t, "v3")
	old := strings.Repeat("a", 64)
	h := newNodeSyncHarness(t, old)
	h.fetchFn = func(string) (SnapshotFetchResult, error) {
		return SnapshotFetchResult{Body: body, ContentHash: envelope.ContentHash, AppliedAt: h.now}, nil
	}
	h.applyFn = func(*replicationsnapshot.Envelope) error { return errors.New("apply exploded") }
	if err := h.syncer.RunOnce(context.Background()); err == nil {
		t.Fatal("apply failure expected")
	}
	st := h.store.state.NodeSyncProgress
	if st.AppliedHash != old || st.PrimaryHash != envelope.ContentHash || !st.Outdated() || len(h.acked) != 0 {
		t.Fatalf("state=%+v acked=%v", st, h.acked)
	}
}

func TestNodeSyncAckFailureKeepsApplyAndRetriesAckOnly(t *testing.T) {
	envelope, body := syncTestEnvelope(t, "v4")
	h := newNodeSyncHarness(t, strings.Repeat("a", 64))
	h.fetchFn = func(after string) (SnapshotFetchResult, error) {
		if after == envelope.ContentHash {
			return SnapshotFetchResult{NotModified: true, ContentHash: after, AppliedAt: h.now}, nil
		}
		return SnapshotFetchResult{Body: body, ContentHash: envelope.ContentHash, AppliedAt: h.now}, nil
	}
	h.ackFn = func(string) error { return errors.New("ack down") }
	if err := h.syncer.RunOnce(context.Background()); err == nil {
		t.Fatal("ack failure expected")
	}
	if st := h.store.state.NodeSyncProgress; st.AppliedHash != envelope.ContentHash || st.AckedHash != "" && st.AckedHash == envelope.ContentHash {
		t.Fatalf("state=%+v", st)
	}
	h.ackFn = nil
	h.now = h.store.state.NextAttemptAt
	if err := h.syncer.RunOnce(context.Background()); err != nil {
		t.Fatalf("second cycle: %v", err)
	}
	if len(h.applied) != 1 {
		t.Fatalf("snapshot re-applied: %v", h.applied)
	}
	if h.store.state.AckedHash != envelope.ContentHash {
		t.Fatalf("ack not retried: %+v", h.store.state.NodeSyncProgress)
	}
}

func TestNodeSyncNeverOverlaps(t *testing.T) {
	hash := strings.Repeat("f", 64)
	h := newNodeSyncHarness(t, hash)
	entered, release := make(chan struct{}), make(chan struct{})
	h.fetchFn = func(string) (SnapshotFetchResult, error) {
		close(entered)
		<-release
		return SnapshotFetchResult{NotModified: true, ContentHash: hash, AppliedAt: h.now}, nil
	}
	done := make(chan error, 1)
	go func() { done <- h.syncer.RunOnce(context.Background()) }()
	<-entered
	if err := h.syncer.Retry(context.Background()); !errors.Is(err, ErrNodeSyncBusy) {
		t.Fatalf("overlapping Retry = %v, want ErrNodeSyncBusy", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("first cycle: %v", err)
	}
}

func TestNodeSyncRunStopsOnContextCancel(t *testing.T) {
	hash := strings.Repeat("9", 64)
	h := newNodeSyncHarness(t, hash)
	h.fetchFn = func(string) (SnapshotFetchResult, error) {
		return SnapshotFetchResult{NotModified: true, ContentHash: hash, AppliedAt: h.now}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	waits := make(chan time.Duration, 4)
	h.syncer.wait = func(ctx context.Context, d time.Duration) error {
		waits <- d
		<-ctx.Done()
		return ctx.Err()
	}
	stopped := make(chan struct{})
	go func() { h.syncer.Run(ctx); close(stopped) }()
	if d := <-waits; d != 30*time.Second {
		t.Fatalf("first wait = %v", d)
	}
	cancel()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop on cancel")
	}
	if len(h.fetches) != 1 {
		t.Fatalf("fetches = %v", h.fetches)
	}
}
