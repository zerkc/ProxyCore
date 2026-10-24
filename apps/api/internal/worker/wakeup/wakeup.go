// Package wakeup ports the worker's job wakeup from
// packages/db/src/sqlite-foundation/job-wakeup.ts and WorkerWakeup in
// apps/worker/src/poller.ts: a cheap polled cursor over apply_jobs that nudges
// the worker loop, which keeps periodic reconciliation as its safety net.
package wakeup

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"time"
)

// CursorSQL is SQLITE_JOB_CURSOR_SQL. The JSON text fingerprint is compared by
// equality: count and max(rowid) catch inserts; the queued count and the max
// claimed/started/finished clocks catch claim, progress, finish and requeue.
// It is a best-effort hint (a same-count swap with identical maxima is missed).
const CursorSQL = `SELECT json_array(count(*), coalesce(max(rowid),0),
  coalesce(sum(status = 'queued'),0), coalesce(max(claimed_at),''),
  coalesce(max(started_at),''), coalesce(max(finished_at),'')) AS cursor FROM apply_jobs`

const (
	// DefaultPollInterval is how often the cursor is read.
	DefaultPollInterval = 500 * time.Millisecond
	// DefaultErrorDelay is the wait after a failed read.
	DefaultErrorDelay = time.Second
)

// SQLCursor returns a ReadCursor over db. Use a connection pool that is not the
// writer's so reads never queue behind a write transaction.
func SQLCursor(db *sql.DB) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		var cursor string
		err := db.QueryRowContext(ctx, CursorSQL).Scan(&cursor)
		return cursor, err
	}
}

// Options configures Run.
type Options struct {
	// ReadCursor returns the opaque cursor. Required.
	ReadCursor func(ctx context.Context) (string, error)
	// OnJob is called synchronously, so it must not block (use Wakeup.Notify).
	OnJob func()
	// OnError receives ReadCursor failures, never after cancellation.
	OnError func(err error)
	// PollInterval defaults to DefaultPollInterval; must not be negative.
	PollInterval time.Duration
	// ErrorDelay is the wait after a failed read, at least PollInterval;
	// defaults to DefaultErrorDelay; must not be negative.
	ErrorDelay time.Duration
	// Sleep is an injectable delay that returns early when ctx is done.
	Sleep func(ctx context.Context, d time.Duration)
}

// Run polls the cursor until ctx is cancelled and then returns nil. It calls
// OnJob after the first successful read, after every recovery from a failed
// read (like a reconnect) and whenever the cursor changes, never when
// unchanged. A failed read is reported, waited out without a hot spin and
// retried. Polls never overlap.
func Run(ctx context.Context, opts Options) error {
	if opts.ReadCursor == nil || opts.OnJob == nil {
		return errors.New("wakeup: ReadCursor and OnJob are required")
	}
	if opts.PollInterval < 0 || opts.ErrorDelay < 0 {
		return errors.New("wakeup: intervals must not be negative")
	}
	poll, errDelay, sleep := opts.PollInterval, opts.ErrorDelay, opts.Sleep
	if poll == 0 {
		poll = DefaultPollInterval
	}
	if errDelay == 0 {
		errDelay = DefaultErrorDelay
	}
	errDelay = max(errDelay, poll)
	if sleep == nil {
		sleep = sleepContext
	}
	var cursor string
	primed := false
	for ctx.Err() == nil {
		next, err := opts.ReadCursor(ctx)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			if opts.OnError != nil {
				opts.OnError(err)
			}
			primed = false
			sleep(ctx, errDelay)
			continue
		}
		if ctx.Err() != nil {
			break
		}
		if !primed || next != cursor {
			primed, cursor = true, next
			opts.OnJob()
		}
		sleep(ctx, poll)
	}
	return nil
}

func sleepContext(ctx context.Context, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
	}
}

// Wakeup is WorkerWakeup: a coalescing pending flag plus waiters. Notify wakes
// every current waiter, or sets the flag when nobody waits so the next Wait
// returns immediately.
type Wakeup struct {
	mu      sync.Mutex
	pending bool
	waiters map[chan struct{}]struct{}
}

// NewWakeup returns an idle Wakeup.
func NewWakeup() *Wakeup { return &Wakeup{waiters: map[chan struct{}]struct{}{}} }

// Notify wakes waiters or records one pending notification.
func (w *Wakeup) Notify() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.waiters) == 0 {
		w.pending = true
		return
	}
	for ch := range w.waiters {
		close(ch)
		delete(w.waiters, ch)
	}
}

// Wait blocks until Notify, the timeout, or ctx is done, and reports whether a
// notification ended the wait. A pending notification returns true at once.
func (w *Wakeup) Wait(ctx context.Context, timeout time.Duration) bool {
	w.mu.Lock()
	if w.pending {
		w.pending = false
		w.mu.Unlock()
		return true
	}
	ch := make(chan struct{})
	w.waiters[ch] = struct{}{}
	w.mu.Unlock()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ch:
		return true
	case <-timer.C:
	case <-ctx.Done():
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, waiting := w.waiters[ch]; waiting {
		delete(w.waiters, ch)
		return false
	}
	// Notify already closed ch while we timed out: report it.
	return true
}
