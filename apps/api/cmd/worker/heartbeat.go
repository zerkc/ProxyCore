package main

import (
	"context"
	"os"
	"path/filepath"
	"time"
)

// heartbeatInterval is how often the ticker rewrites the heartbeat file; the
// compose healthcheck treats a file older than 180 seconds as unhealthy.
const heartbeatInterval = 30 * time.Second

// isoTime formats like JavaScript's Date.toISOString.
func isoTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

// heartbeat rewrites a file with the current ISO timestamp.
type heartbeat struct {
	path string
	now  func() time.Time
}

// prepare creates the parent directory (mkdir -p) and writes the first beat.
func (h heartbeat) prepare() error {
	if err := os.MkdirAll(filepath.Dir(h.path), 0o755); err != nil {
		return err
	}
	return h.beat()
}

func (h heartbeat) beat() error {
	return os.WriteFile(h.path, []byte(isoTime(h.now())), 0o644)
}

// tick beats every interval from its own goroutine until ctx is done, so a long
// apply on the poller goroutine can never stall it. Failures go to onError.
func (h heartbeat) tick(ctx context.Context, interval time.Duration, onError func(error)) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := h.beat(); err != nil && onError != nil {
				onError(err)
			}
		}
	}
}
