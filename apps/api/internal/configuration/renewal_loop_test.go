package configuration

import (
	"bytes"
	"context"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/identity"
)

type renewalLoopTestTimer struct {
	events  chan time.Time
	resets  chan time.Duration
	stopped chan struct{}
	once    sync.Once
}

func newRenewalLoopTestTimer() *renewalLoopTestTimer {
	return &renewalLoopTestTimer{
		events:  make(chan time.Time, 8),
		resets:  make(chan time.Duration, 8),
		stopped: make(chan struct{}),
	}
}

func (t *renewalLoopTestTimer) C() <-chan time.Time { return t.events }
func (t *renewalLoopTestTimer) Stop() bool {
	t.once.Do(func() { close(t.stopped) })
	return true
}
func (t *renewalLoopTestTimer) Reset(delay time.Duration) bool {
	t.resets <- delay
	return true
}
func (t *renewalLoopTestTimer) fire() { t.events <- time.Now() }

// renewalLoopPolicyFixture models the live cached identity without racing the
// renewal goroutine when a role transition happens between cycles.
type renewalLoopPolicyFixture struct {
	mu     sync.RWMutex
	role   domain.TopologyRole
	loaded bool
}

func (f *renewalLoopPolicyFixture) current(context.Context) (identity.Identity, bool, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return identity.Identity{Role: f.role, LeadershipGeneration: 1, LatestKnownGeneration: 1}, f.loaded, nil
}

func (f *renewalLoopPolicyFixture) setRole(role domain.TopologyRole) {
	f.mu.Lock()
	f.role = role
	f.mu.Unlock()
}

func awaitRenewalTimer(t *testing.T, created <-chan *renewalLoopTestTimer) *renewalLoopTestTimer {
	t.Helper()
	select {
	case timer := <-created:
		return timer
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for renewal timer")
		return nil
	}
}

func awaitRenewalReset(t *testing.T, timer *renewalLoopTestTimer) {
	t.Helper()
	select {
	case <-timer.resets:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for renewal cycle reset")
	}
}

func awaitRenewalCall(t *testing.T, calls <-chan struct{}) {
	t.Helper()
	select {
	case <-calls:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for allowed renewal call")
	}
}

func assertNoRenewalCall(t *testing.T, calls <-chan struct{}) {
	t.Helper()
	select {
	case <-calls:
		t.Fatal("renewal call ran while policy was forbidden")
	case <-time.After(20 * time.Millisecond):
	}
}

func TestRenewalLoopStopsAndResumesAcrossLiveRoleTransitions(t *testing.T) {
	fixture := &renewalLoopPolicyFixture{role: domain.TopologyRolePrimary, loaded: true}
	created := make(chan *renewalLoopTestTimer, 1)
	calls := make(chan struct{}, 8)
	var logs bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runRenewalLoop(ctx, &Store{}, RenewalOptions{
			Identity: fixture.current,
			Log:      log.New(&logs, "", 0),
		}, time.Minute, renewalLoopRuntime{
			InitialDelay: time.Second,
			NewTimer: func(time.Duration) renewalLoopTimer {
				timer := newRenewalLoopTestTimer()
				created <- timer
				return timer
			},
			Renew: func(context.Context) (int, int, error) {
				calls <- struct{}{}
				return 1, 0, nil
			},
		})
		close(done)
	}()
	timer := awaitRenewalTimer(t, created)

	timer.fire()
	awaitRenewalCall(t, calls)
	awaitRenewalReset(t, timer)

	fixture.setRole(domain.TopologyRoleNode)
	timer.fire()
	awaitRenewalReset(t, timer)
	assertNoRenewalCall(t, calls)
	timer.fire()
	awaitRenewalReset(t, timer)
	assertNoRenewalCall(t, calls)

	fixture.setRole(domain.TopologyRoleStalePrimary)
	timer.fire()
	awaitRenewalReset(t, timer)
	assertNoRenewalCall(t, calls)

	fixture.setRole(domain.TopologyRolePrimary)
	timer.fire()
	awaitRenewalCall(t, calls)
	awaitRenewalReset(t, timer)

	logsText := logs.String()
	if strings.Count(logsText, "reason=role-not-writable role=node") != 1 {
		t.Fatalf("node skip log count=%d logs=%q", strings.Count(logsText, "reason=role-not-writable role=node"), logsText)
	}
	if strings.Count(logsText, "reason=stale-primary role=stale-primary") != 1 {
		t.Fatalf("stale skip log count=%d logs=%q", strings.Count(logsText, "reason=stale-primary role=stale-primary"), logsText)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("renewal loop did not stop after cancellation")
	}
}

func TestRenewalLoopSkipsUnloadedIdentity(t *testing.T) {
	fixture := &renewalLoopPolicyFixture{role: domain.TopologyRolePrimary, loaded: false}
	created := make(chan *renewalLoopTestTimer, 1)
	calls := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		runRenewalLoop(ctx, &Store{}, RenewalOptions{Identity: fixture.current}, time.Minute, renewalLoopRuntime{
			InitialDelay: time.Second,
			NewTimer: func(time.Duration) renewalLoopTimer {
				timer := newRenewalLoopTestTimer()
				created <- timer
				return timer
			},
			Renew: func(context.Context) (int, int, error) {
				calls <- struct{}{}
				return 1, 0, nil
			},
		})
		close(done)
	}()
	timer := awaitRenewalTimer(t, created)
	timer.fire()
	awaitRenewalReset(t, timer)
	assertNoRenewalCall(t, calls)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("renewal loop did not stop after unloaded identity cancellation")
	}
}

func TestRenewalLoopDoesNotRenewAfterContextCancellation(t *testing.T) {
	fixture := &renewalLoopPolicyFixture{role: domain.TopologyRoleStandalone, loaded: true}
	created := make(chan *renewalLoopTestTimer, 1)
	calls := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runRenewalLoop(ctx, &Store{}, RenewalOptions{Identity: fixture.current}, time.Minute, renewalLoopRuntime{
			InitialDelay: time.Second,
			NewTimer: func(time.Duration) renewalLoopTimer {
				timer := newRenewalLoopTestTimer()
				created <- timer
				return timer
			},
			Renew: func(context.Context) (int, int, error) {
				calls <- struct{}{}
				return 1, 0, nil
			},
		})
		close(done)
	}()
	awaitRenewalTimer(t, created)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("renewal loop did not stop after context cancellation")
	}
	assertNoRenewalCall(t, calls)
}
