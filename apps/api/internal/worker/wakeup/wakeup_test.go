package wakeup_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/worker/testsupport"
	"github.com/zerkc/ProxyCore/apps/api/internal/worker/wakeup"
)

// manualClock parks every sleep until release is signalled, so a test advances
// the poll loop one step at a time without real delays.
type manualClock struct {
	sleeps  chan time.Duration
	release chan struct{}
}

func newClock() *manualClock {
	return &manualClock{sleeps: make(chan time.Duration), release: make(chan struct{})}
}

func (c *manualClock) sleep(ctx context.Context, d time.Duration) {
	select {
	case c.sleeps <- d:
	case <-ctx.Done():
		return
	}
	select {
	case <-c.release:
	case <-ctx.Done():
	}
}

// next waits for the loop to reach its next sleep and returns the duration.
func (c *manualClock) next(t *testing.T) time.Duration {
	t.Helper()
	select {
	case d := <-c.sleeps:
		return d
	case <-time.After(5 * time.Second):
		t.Fatal("poll loop did not reach its next sleep")
		return 0
	}
}

func (c *manualClock) advance(t *testing.T) { t.Helper(); c.release <- struct{}{} }

type harness struct {
	clock  *manualClock
	cancel context.CancelFunc
	done   chan error
	calls  atomic.Int32
	reads  atomic.Int32
	mu     sync.Mutex
	value  string
	fail   error
	errors []error
}

func start(t *testing.T, poll, errDelay time.Duration) *harness {
	t.Helper()
	h := &harness{clock: newClock(), value: "a", done: make(chan error, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go func() {
		h.done <- wakeup.Run(ctx, wakeup.Options{
			PollInterval: poll, ErrorDelay: errDelay, Sleep: h.clock.sleep,
			OnJob: func() { h.calls.Add(1) },
			OnError: func(err error) {
				h.mu.Lock()
				h.errors = append(h.errors, err)
				h.mu.Unlock()
			},
			ReadCursor: func(context.Context) (string, error) {
				h.reads.Add(1)
				h.mu.Lock()
				defer h.mu.Unlock()
				return h.value, h.fail
			},
		})
	}()
	t.Cleanup(func() { cancel() })
	return h
}

func (h *harness) set(value string, fail error) {
	h.mu.Lock()
	h.value, h.fail = value, fail
	h.mu.Unlock()
}

func (h *harness) stop(t *testing.T) {
	t.Helper()
	h.cancel()
	select {
	case err := <-h.done:
		if err != nil {
			t.Fatalf("Run returned %v after cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
}

func TestRunFiresOnFirstReadAndOnlyOnChange(t *testing.T) {
	h := start(t, 250*time.Millisecond, 0)
	if d := h.clock.next(t); d != 250*time.Millisecond || h.calls.Load() != 1 {
		t.Fatalf("after start: sleep=%v calls=%d, want 250ms and 1", d, h.calls.Load())
	}
	h.clock.advance(t)
	h.clock.next(t)
	h.clock.advance(t)
	h.clock.next(t)
	if h.calls.Load() != 1 || h.reads.Load() != 3 {
		t.Fatalf("unchanged cursor: calls=%d reads=%d, want 1 and 3", h.calls.Load(), h.reads.Load())
	}
	h.set("b", nil)
	h.clock.advance(t)
	h.clock.next(t)
	if h.calls.Load() != 2 {
		t.Fatalf("after change calls=%d, want 2", h.calls.Load())
	}
	h.clock.advance(t)
	h.clock.next(t)
	if h.calls.Load() != 2 {
		t.Fatalf("repeat of the new value fired again: calls=%d", h.calls.Load())
	}
	h.stop(t)
}

func TestRunBacksOffAfterReadErrorAndRefiresOnRecovery(t *testing.T) {
	boom := errors.New("database is locked")
	h := start(t, 250*time.Millisecond, time.Second)
	h.clock.next(t)
	h.set("a", boom)
	h.clock.advance(t)
	if d := h.clock.next(t); d != time.Second {
		t.Fatalf("error backoff = %v, want 1s", d)
	}
	h.mu.Lock()
	reported := append([]error(nil), h.errors...)
	h.mu.Unlock()
	if len(reported) != 1 || !errors.Is(reported[0], boom) {
		t.Fatalf("reported errors = %v", reported)
	}
	h.set("a", nil) // same cursor value as before the failure
	h.clock.advance(t)
	if d := h.clock.next(t); d != 250*time.Millisecond {
		t.Fatalf("sleep after recovery = %v, want the poll interval", d)
	}
	if h.calls.Load() != 2 {
		t.Fatalf("recovery calls = %d, want a refire even though the cursor is unchanged", h.calls.Load())
	}
	h.stop(t)
}

func TestRunErrorDelayIsAtLeastThePollInterval(t *testing.T) {
	h := start(t, 2*time.Second, 100*time.Millisecond)
	h.clock.next(t)
	h.set("a", errors.New("boom"))
	h.clock.advance(t)
	if d := h.clock.next(t); d != 2*time.Second {
		t.Fatalf("backoff = %v, want the larger poll interval", d)
	}
	h.mu.Lock()
	failures := len(h.errors)
	h.mu.Unlock()
	if failures != 1 {
		t.Fatalf("read failures reported = %d, want 1 (the sleep must follow a failed read)", failures)
	}
	h.stop(t)
}

func TestRunDefaultsAndValidation(t *testing.T) {
	read := func(context.Context) (string, error) { return "x", nil }
	for _, bad := range []time.Duration{-time.Second} {
		if err := wakeup.Run(context.Background(), wakeup.Options{PollInterval: bad, ReadCursor: read, OnJob: func() {}}); err == nil {
			t.Fatalf("poll interval %v accepted", bad)
		}
		if err := wakeup.Run(context.Background(), wakeup.Options{ErrorDelay: bad, ReadCursor: read, OnJob: func() {}}); err == nil {
			t.Fatalf("error delay %v accepted", bad)
		}
	}
	if err := wakeup.Run(context.Background(), wakeup.Options{OnJob: func() {}}); err == nil {
		t.Fatal("missing ReadCursor accepted")
	}
	h := start(t, 0, 0)
	if d := h.clock.next(t); d != wakeup.DefaultPollInterval {
		t.Fatalf("default poll interval = %v, want %v", d, wakeup.DefaultPollInterval)
	}
	h.stop(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var touched atomic.Int32
	err := wakeup.Run(ctx, wakeup.Options{
		OnJob:      func() { touched.Add(1) },
		ReadCursor: func(context.Context) (string, error) { touched.Add(1); return "x", nil },
	})
	if err != nil || touched.Load() != 0 {
		t.Fatalf("pre-cancelled Run = %v touched=%d", err, touched.Load())
	}
}

func TestRunDoesNotReportErrorsAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var reported atomic.Int32
	err := wakeup.Run(ctx, wakeup.Options{
		OnJob:   func() {},
		OnError: func(error) { reported.Add(1) },
		ReadCursor: func(ctx context.Context) (string, error) {
			cancel()
			return "", ctx.Err()
		},
	})
	if err != nil || reported.Load() != 0 {
		t.Fatalf("Run = %v, errors reported after cancel = %d", err, reported.Load())
	}
}

func TestWakeupPendingNotifyIsConsumedOnce(t *testing.T) {
	w := wakeup.NewWakeup()
	w.Notify()
	w.Notify() // coalesces into one pending flag
	start := time.Now()
	if !w.Wait(context.Background(), time.Minute) || time.Since(start) > time.Second {
		t.Fatal("a pending notification must satisfy the next Wait immediately")
	}
	if w.Wait(context.Background(), 20*time.Millisecond) {
		t.Fatal("the pending flag was not consumed")
	}
}

func TestWakeupNotifyReleasesWaiterAndContextCancels(t *testing.T) {
	w := wakeup.NewWakeup()
	got := make(chan bool, 1)
	go func() { got <- w.Wait(context.Background(), time.Minute) }()
	time.Sleep(20 * time.Millisecond)
	w.Notify()
	select {
	case notified := <-got:
		if !notified {
			t.Fatal("Wait released by Notify must report true")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Notify did not release the waiter")
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { got <- w.Wait(ctx, time.Minute) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case notified := <-got:
		if notified {
			t.Fatal("a cancelled Wait must report false")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation did not release the waiter")
	}
	// A Notify racing a cancelled waiter is still delivered to the next Wait.
	w.Notify()
	if !w.Wait(context.Background(), time.Second) {
		t.Fatal("Notify after cancellation was lost")
	}
}

func TestSQLCursorChangesOnInsertClaimAndFinish(t *testing.T) {
	db, _ := testsupport.NewDB(t)
	rev := testsupport.UUID(1)
	testsupport.InsertRevision(t, db, rev, 1, nil)
	read := wakeup.SQLCursor(db)
	ctx := context.Background()
	cursor := func() string {
		t.Helper()
		c, err := read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	empty := cursor()
	if again := cursor(); again != empty {
		t.Fatalf("cursor drifted without writes: %q vs %q", empty, again)
	}
	claimed, finished := testsupport.Stamp(t, time.Unix(1000, 0)), testsupport.Stamp(t, time.Unix(2000, 0))
	steps := []struct{ name, sql string }{
		{"insert", `insert into apply_jobs (id,revision_id,target,status,correlation_id,created_at) values ('` + testsupport.UUID(5) + `','` + rev + `','combined','queued','c','0')`},
		{"claim", `update apply_jobs set status='validating', claimed_at='` + claimed + `', started_at='` + claimed + `'`},
		{"finish", `update apply_jobs set status='applied', finished_at='` + finished + `'`},
	}
	seen := map[string]bool{empty: true}
	for _, step := range steps {
		if _, err := db.Exec(step.sql); err != nil {
			t.Fatal(step.name, err)
		}
		c := cursor()
		if seen[c] {
			t.Fatalf("%s did not change the cursor: %q", step.name, c)
		}
		seen[c] = true
	}
}

func TestRunWakesWhenJobIsInsertedIntoRealDatabase(t *testing.T) {
	db, _ := testsupport.NewDB(t)
	rev := testsupport.UUID(1)
	testsupport.InsertRevision(t, db, rev, 1, nil)
	w := wakeup.NewWakeup()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- wakeup.Run(ctx, wakeup.Options{PollInterval: 5 * time.Millisecond, OnJob: w.Notify, ReadCursor: wakeup.SQLCursor(db)})
	}()
	defer func() { cancel(); <-done }()
	if !w.Wait(ctx, 5*time.Second) {
		t.Fatal("no initial wakeup on first read")
	}
	if w.Wait(ctx, 100*time.Millisecond) {
		t.Fatal("wakeup fired without a change")
	}
	testsupport.InsertJob(t, db, testsupport.UUID(6), rev, "queued", "0", nil)
	if !w.Wait(ctx, 5*time.Second) {
		t.Fatal("inserting a job did not wake the worker")
	}
}
