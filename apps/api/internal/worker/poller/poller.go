// Package poller ports apps/worker/src/poller.ts: the worker loop that recovers
// stale jobs, claims and applies queued work, and reconciles the nginx data
// plane when the queue is idle.
package poller

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/worker/apply"
	"github.com/zerkc/ProxyCore/apps/api/internal/worker/jobs"
	"github.com/zerkc/ProxyCore/apps/api/internal/worker/revisions"
	"github.com/zerkc/ProxyCore/apps/api/internal/worker/store"
)

// DefaultLeaseMs is how long a claimed job may sit before it is requeued.
const DefaultLeaseMs = 120_000

// Jobs is the job store surface the poller needs; *store.Store implements it.
type Jobs interface {
	RecoverStale(ctx context.Context, leaseMs int64) (int64, error)
	ClaimNext(ctx context.Context, target store.Target) (*store.Job, error)
	List(ctx context.Context) ([]store.Job, error)
	Update(ctx context.Context, id string, patch store.JobPatch) (store.Job, error)
}

// Revisions is the revision store surface the poller needs.
type Revisions interface {
	Get(ctx context.Context, id string) (*revisions.Revision, error)
	LatestApplied(ctx context.Context) (*revisions.Revision, error)
}

// Applier runs claimed jobs and reconciliation; *apply.Orchestrator implements it.
type Applier interface {
	Apply(ctx context.Context, jobID string, snapshot json.RawMessage, render apply.Renderer) (store.Job, error)
	ReconcileApplied(ctx context.Context, revision revisions.Revision, render apply.Renderer) (*store.Job, error)
}

// Waiter blocks until a notification, the timeout or ctx; *wakeup.Wakeup
// implements it.
type Waiter interface {
	Wait(ctx context.Context, timeout time.Duration) bool
}

// Options configures PollOnce and Run.
type Options struct {
	Jobs      Jobs
	Revisions Revisions
	Applier   Applier
	Render    apply.Renderer
	// LeaseMs defaults to DefaultLeaseMs.
	LeaseMs int64
	Now     func() time.Time

	// Interval is the reconciliation period between idle turns; it must be
	// positive for Run.
	Interval time.Duration
	// Wakeup ends an idle wait early; without it the loop sleeps Interval.
	Wakeup Waiter
	// Heartbeat runs once at the start of every loop turn.
	Heartbeat func()
	// OnError receives a turn's error; the loop continues.
	OnError func(err error)
}

// PollOnce recovers stale jobs, then claims and applies one job. When nothing
// was claimed and no job is queued, validating or applying, it reconciles the
// reconcile-target revision instead. It returns the job it applied or repaired,
// or nil when there was nothing to do.
//
// Once a job is claimed the work runs detached from ctx's cancellation, so a
// shutdown never leaves a half-applied job behind: the in-flight apply finishes
// (each control call is bounded by its own timeout) and records its outcome.
func PollOnce(ctx context.Context, options Options) (*store.Job, error) {
	lease := options.LeaseMs
	if lease <= 0 {
		lease = DefaultLeaseMs
	}
	if _, err := options.Jobs.RecoverStale(ctx, lease); err != nil {
		return nil, err
	}
	job, err := options.Jobs.ClaimNext(ctx, "")
	if err != nil {
		return nil, err
	}
	work := context.WithoutCancel(ctx)
	if job == nil {
		all, err := options.Jobs.List(ctx)
		if err != nil {
			return nil, err
		}
		for _, existing := range all {
			if isActive(existing) {
				return nil, nil
			}
		}
		revision, err := options.Revisions.LatestApplied(ctx)
		if err != nil || revision == nil {
			return nil, err
		}
		return options.Applier.ReconcileApplied(work, *revision, options.Render)
	}

	revision, err := options.Revisions.Get(work, job.RevisionID)
	if err != nil {
		return nil, err
	}
	if revision == nil {
		failed, err := options.Jobs.Update(work, job.ID, store.JobPatch{
			Status:       statusPtr(jobs.Failed),
			ErrorMessage: store.Set(fmt.Sprintf("Revision not found: %s", job.RevisionID)),
			FinishedAt:   store.Set(options.now()),
		})
		if err != nil {
			return nil, err
		}
		return &failed, nil
	}
	applied, err := options.Applier.Apply(work, job.ID, revision.Snapshot, options.Render)
	if err != nil {
		return nil, err
	}
	return &applied, nil
}

// Run loops until ctx is cancelled and then returns nil: per turn it writes the
// heartbeat, drains PollOnce until it returns nothing, then waits for a wakeup
// or the reconciliation interval. A failing turn is reported to OnError and the
// loop continues with the wait.
func Run(ctx context.Context, options Options) error {
	if options.Interval <= 0 {
		return fmt.Errorf("poller: Interval must be positive")
	}
	for ctx.Err() == nil {
		if options.Heartbeat != nil {
			options.Heartbeat()
		}
		for ctx.Err() == nil {
			job, err := PollOnce(ctx, options)
			if err != nil {
				if options.OnError != nil {
					options.OnError(err)
				}
				break
			}
			if job == nil {
				break
			}
		}
		if ctx.Err() != nil {
			break
		}
		if options.Wakeup != nil {
			options.Wakeup.Wait(ctx, options.Interval)
		} else {
			sleep(ctx, options.Interval)
		}
	}
	return nil
}

func (o Options) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func isActive(job store.Job) bool {
	switch job.Status {
	case jobs.Queued, jobs.Validating, jobs.Applying:
		return true
	}
	return false
}

func statusPtr(status jobs.Status) *jobs.Status { return &status }

func sleep(ctx context.Context, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
	}
}
