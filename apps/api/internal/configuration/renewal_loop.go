package configuration

import (
	"context"
	"log"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
)

const defaultRenewalInitialDelay = 15 * time.Second

type renewalLoopTimer interface {
	C() <-chan time.Time
	Stop() bool
	Reset(time.Duration) bool
}

type realRenewalLoopTimer struct{ timer *time.Timer }

func (t realRenewalLoopTimer) C() <-chan time.Time            { return t.timer.C }
func (t realRenewalLoopTimer) Stop() bool                     { return t.timer.Stop() }
func (t realRenewalLoopTimer) Reset(delay time.Duration) bool { return t.timer.Reset(delay) }

type renewalLoopRuntime struct {
	InitialDelay time.Duration
	NewTimer     func(time.Duration) renewalLoopTimer
	Renew        func(context.Context) (int, int, error)
}

// RunRenewalLoop periodically renews due Let's Encrypt and internal
// certificates until ctx is done. Enrollment TLS material is refreshed by its
// role-aware supervisor, not by this ordinary certificate worker.
func RunRenewalLoop(ctx context.Context, store *Store, opts RenewalOptions, interval time.Duration) {
	runRenewalLoop(ctx, store, opts, interval, renewalLoopRuntime{})
}

func runRenewalLoop(ctx context.Context, store *Store, opts RenewalOptions, interval time.Duration, runtime renewalLoopRuntime) {
	if ctx == nil || store == nil {
		return
	}
	if interval <= 0 {
		interval = time.Hour
	}
	logger := opts.Log
	if logger == nil {
		logger = log.Default()
	}
	initialDelay := runtime.InitialDelay
	if initialDelay <= 0 {
		initialDelay = defaultRenewalInitialDelay
	}
	newTimer := runtime.NewTimer
	if newTimer == nil {
		newTimer = func(delay time.Duration) renewalLoopTimer {
			return realRenewalLoopTimer{timer: time.NewTimer(delay)}
		}
	}
	renew := runtime.Renew
	if renew == nil {
		renew = func(workCtx context.Context) (int, int, error) {
			return store.RenewDueCertificates(workCtx, opts)
		}
	}

	lastSkipReason := RenewalSkipReason("")
	lastSkipRole := domain.TopologyRole("")
	run := func() {
		if ctx.Err() != nil {
			return
		}
		status := CheckRenewalPolicy(ctx, opts.Identity)
		if !status.Allowed {
			if status.Reason != lastSkipReason || status.Role != lastSkipRole {
				logger.Printf("certificate renewal skipped: reason=%s role=%s", status.Reason, status.Role)
				lastSkipReason, lastSkipRole = status.Reason, status.Role
			}
			return
		}
		lastSkipReason, lastSkipRole = "", ""
		renewed, failed, err := renew(ctx)
		if err != nil {
			if ctx.Err() == nil {
				logger.Printf("certificate renewal sweep failed: %v", err)
			}
			return
		}
		if ctx.Err() == nil && (renewed > 0 || failed > 0) {
			logger.Printf("certificate renewal sweep: renewed=%d failed=%d", renewed, failed)
		}
	}

	// Run once shortly after startup, then on the interval.
	timer := newTimer(initialDelay)
	if timer == nil {
		return
	}
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C():
			run()
			if ctx.Err() != nil {
				return
			}
			timer.Reset(interval)
		}
	}
}
