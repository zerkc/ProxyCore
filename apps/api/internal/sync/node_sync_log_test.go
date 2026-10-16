package sync

import (
	"context"
	"fmt"
	"strings"
	gosync "sync"
	"testing"
	"time"

	replicationsnapshot "github.com/zerkc/ProxyCore/apps/api/internal/snapshot"
)

type typedApplyError struct{ detail string }

func (e *typedApplyError) Error() string { return "apply failed: " + e.detail }

func TestNodeSyncLogsStepAndRedactedCauseForEveryFailure(t *testing.T) {
	const secret = "SECRET-pcenr2_TOKENVALUE-payload"
	old := strings.Repeat("a", 64)
	envelope, body := syncTestEnvelope(t, "v9")
	tampered := envelope
	tampered.ContentHash = strings.Repeat("0", 64)
	tamperedBody, err := replicationsnapshot.Marshal(tampered)
	if err != nil {
		t.Fatal(err)
	}
	fresh := func(h *nodeSyncHarness) {
		h.fetchFn = func(string) (SnapshotFetchResult, error) {
			return SnapshotFetchResult{Body: body, ContentHash: envelope.ContentHash, AppliedAt: h.now}, nil
		}
	}
	cases := []struct {
		name  string
		setup func(h *nodeSyncHarness)
		want  []string
	}{
		{"apply typed error", func(h *nodeSyncHarness) {
			fresh(h)
			h.applyFn = func(*replicationsnapshot.Envelope) error {
				return fmt.Errorf("wrap %s: %w", secret, &typedApplyError{detail: secret})
			}
		}, []string{"apply_failed", "step=apply", "*fmt.wrapError>*sync.typedApplyError"}},
		{"fetch unauthorized", func(h *nodeSyncHarness) {
			h.fetchFn = func(string) (SnapshotFetchResult, error) {
				return SnapshotFetchResult{}, fmt.Errorf("401 %s: %w", secret, ErrSnapshotClientUnauthorized)
			}
		}, []string{"credential_rejected", "step=fetch", "ErrSnapshotClientUnauthorized"}},
		{"fetch unknown", func(h *nodeSyncHarness) {
			h.fetchFn = func(string) (SnapshotFetchResult, error) { return SnapshotFetchResult{}, &typedApplyError{secret} }
		}, []string{"primary_unavailable", "step=fetch", "*sync.typedApplyError"}},
		{"integrity hash", func(h *nodeSyncHarness) {
			h.fetchFn = func(string) (SnapshotFetchResult, error) {
				return SnapshotFetchResult{Body: tamperedBody, ContentHash: tampered.ContentHash, AppliedAt: h.now}, nil
			}
		}, []string{"integrity_failed", "step=hash"}},
		{"integrity unmarshal", func(h *nodeSyncHarness) {
			h.fetchFn = func(string) (SnapshotFetchResult, error) {
				return SnapshotFetchResult{Body: []byte(secret), ContentHash: "x", AppliedAt: h.now}, nil
			}
		}, []string{"integrity_failed", "step=unmarshal"}},
		{"acknowledge", func(h *nodeSyncHarness) {
			fresh(h)
			h.ackFn = func(string) error { return fmt.Errorf("ack %s: %w", secret, ErrSnapshotClientUnavailable) }
		}, []string{"acknowledge_failed", "step=acknowledge", "ErrSnapshotClientUnavailable"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newNodeSyncHarness(t, old)
			var mu gosync.Mutex
			var lines []string
			h.syncer.opts.Logf = func(format string, args ...any) {
				mu.Lock()
				defer mu.Unlock()
				lines = append(lines, fmt.Sprintf(format, args...))
			}
			tc.setup(h)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			h.syncer.wait = func(ctx context.Context, _ time.Duration) error {
				cancel()
				return ctx.Err()
			}
			h.syncer.Run(ctx)
			mu.Lock()
			out := strings.Join(lines, "\n")
			mu.Unlock()
			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Fatalf("log missing %q:\n%s", want, out)
				}
			}
			for _, leaked := range []string{secret, "pcenr2_", "TOKENVALUE", "wrap ", "401 "} {
				if strings.Contains(out, leaked) {
					t.Fatalf("log leaks %q:\n%s", leaked, out)
				}
			}
		})
	}
}
