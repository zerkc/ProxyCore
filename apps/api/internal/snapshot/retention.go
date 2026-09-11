package snapshot

import (
	"context"
	"log"
	"time"
)

// RetentionWorker periodically calls ArchiveStore.PurgeExpiredArchives to
// honour the standalone-archive TTL (30 days by default, resolved by the
// user round before implementation began). The worker is intentionally
// minimal: a goroutine, a ticker, and a shutdown channel.
type RetentionWorker struct {
	archive ArchiveStore
	period  time.Duration
	log     *log.Logger
}

// NewRetentionWorker constructs a worker with the given period. A zero or
// negative period defaults to one hour.
func NewRetentionWorker(archive ArchiveStore, period time.Duration, logger *log.Logger) *RetentionWorker {
	if period <= 0 {
		period = time.Hour
	}
	if logger == nil {
		logger = log.Default()
	}
	return &RetentionWorker{archive: archive, period: period, log: logger}
}

// Run blocks until ctx is cancelled. It performs an initial purge
// immediately, then schedules one every period. The worker logs every
// purge result so the operator can confirm retention behaviour without
// reaching for ad-hoc queries.
func (w *RetentionWorker) Run(ctx context.Context) {
	w.log.Printf("snapshot retention worker started (period=%s)", w.period)
	if purged, err := w.archive.PurgeExpiredArchives(ctx, time.Now().UTC()); err != nil {
		w.log.Printf("retention purge failed: %v", err)
	} else if purged > 0 {
		w.log.Printf("retention purge removed %d expired archive(s)", purged)
	}
	ticker := time.NewTicker(w.period)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			w.log.Printf("snapshot retention worker stopping")
			return
		case <-ticker.C:
			if purged, err := w.archive.PurgeExpiredArchives(ctx, time.Now().UTC()); err != nil {
				w.log.Printf("retention purge failed: %v", err)
			} else if purged > 0 {
				w.log.Printf("retention purge removed %d expired archive(s)", purged)
			}
		}
	}
}
