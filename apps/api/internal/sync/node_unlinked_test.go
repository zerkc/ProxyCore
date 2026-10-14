package sync

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
)

const unlinkedBody = `{"error":"node unlinked","code":"NODE_UNLINKED"}`

func goneClient(body []byte) *SnapshotClient {
	return NewSnapshotClient(SnapshotClientOptions{
		HTTPClient: &http.Client{Transport: snapshotRoundTripper(func(*http.Request) (*http.Response, error) {
			return snapshotResponse(http.StatusGone, body), nil
		})},
		MaxAttempts: 3, InitialBackoff: time.Nanosecond,
	})
}

func TestSnapshotClientUnlinkedOnlyOnExactSignalCode(t *testing.T) {
	cases := []struct {
		name string
		body string
		want error
	}{
		{"exact signal", unlinkedBody, ErrSnapshotClientUnlinked},
		{"exact code without error text", `{"code":"NODE_UNLINKED"}`, ErrSnapshotClientUnlinked},
		{"bare 410", ``, ErrSnapshotClientGone},
		{"empty object", `{}`, ErrSnapshotClientGone},
		{"other code", `{"error":"gone","code":"CREDENTIAL_REVOKED"}`, ErrSnapshotClientGone},
		{"lowercase code", `{"code":"node_unlinked"}`, ErrSnapshotClientGone},
		{"code in error text only", `{"error":"NODE_UNLINKED"}`, ErrSnapshotClientGone},
		{"malformed json", `{"code":"NODE_UNLINKED"`, ErrSnapshotClientGone},
		{"plain text", `NODE_UNLINKED`, ErrSnapshotClientGone},
		{"oversized body", `{"code":"NODE_UNLINKED","pad":"` + strings.Repeat("x", 2<<10) + `"}`, ErrSnapshotClientGone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := goneClient([]byte(tc.body))
			_, err := client.FetchSnapshotIfChanged(context.Background(), "https://primary.example:3443/",
				bytes.Repeat([]byte{0x42}, NodeCredentialSecretBytes), "")
			if !errors.Is(err, tc.want) {
				t.Fatalf("fetch: err=%v want %v", err, tc.want)
			}
			err = client.AcknowledgeSnapshot(context.Background(), "https://primary.example:3443/",
				bytes.Repeat([]byte{0x42}, NodeCredentialSecretBytes), "99998888-7777-4666-8555-444433332222", strings.Repeat("c", 64), time.Now())
			if !errors.Is(err, tc.want) {
				t.Fatalf("ack: err=%v want %v", err, tc.want)
			}
		})
	}
}

func TestSnapshotClientSignalBodyOnOtherStatusIsNeverUnlinked(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusInternalServerError, http.StatusServiceUnavailable, http.StatusBadRequest} {
		client := NewSnapshotClient(SnapshotClientOptions{
			HTTPClient: &http.Client{Transport: snapshotRoundTripper(func(*http.Request) (*http.Response, error) {
				return snapshotResponse(status, []byte(unlinkedBody)), nil
			})},
			MaxAttempts: 1, InitialBackoff: time.Nanosecond,
		})
		_, err := client.FetchSnapshotIfChanged(context.Background(), "https://primary.example:3443/",
			bytes.Repeat([]byte{0x42}, NodeCredentialSecretBytes), "")
		if err == nil || errors.Is(err, ErrSnapshotClientUnlinked) {
			t.Fatalf("status %d: err=%v must not be unlinked", status, err)
		}
	}
}

// unlinkRecorder tracks OnUnlinked calls on a harness syncer.
func recordUnlinked(h *nodeSyncHarness) *[]bool {
	var calls []bool
	h.syncer.opts.OnUnlinked = func(v bool) { calls = append(calls, v) }
	return &calls
}

func TestNodeSyncPersistsUnlinkedOnlyFromExplicitSignalAndStopsPolling(t *testing.T) {
	h := newNodeSyncHarness(t, strings.Repeat("a", 64))
	calls := recordUnlinked(h)
	h.fetchFn = func(string) (SnapshotFetchResult, error) { return SnapshotFetchResult{}, ErrSnapshotClientUnlinked }

	err := h.syncer.RunOnce(context.Background())
	var failure *NodeSyncFailure
	if !errors.As(err, &failure) || failure.Code != "node_unlinked" {
		t.Fatalf("RunOnce err=%v want node_unlinked", err)
	}
	st := h.store.state.NodeSyncProgress
	if !st.Unlinked || !st.UnlinkedAt.Equal(h.now) || st.AppliedHash != strings.Repeat("a", 64) || st.ConsecutiveFailures != 0 {
		t.Fatalf("progress=%+v", st)
	}
	if got := h.syncer.delay(); got != 5*time.Minute {
		t.Fatalf("delay=%v want MaxBackoff 5m", got)
	}
	if len(*calls) != 1 || !(*calls)[0] {
		t.Fatalf("OnUnlinked calls=%v", *calls)
	}

	// Later cycles (and a restart: a fresh syncer over the same store) make no
	// primary request and never re-enable aggressive polling.
	restarted := newNodeSyncHarness(t, "")
	restarted.store = h.store
	restarted.syncer.opts.Store = h.store
	rcalls := recordUnlinked(restarted)
	fetchCount := len(h.fetches)
	for _, s := range []*NodeSyncer{h.syncer, restarted.syncer} {
		err = s.RunOnce(context.Background())
		if !errors.As(err, &failure) || failure.Code != "node_unlinked" {
			t.Fatalf("unlinked cycle err=%v", err)
		}
		if s.delay() != 5*time.Minute {
			t.Fatalf("delay=%v want 5m", s.delay())
		}
	}
	if len(h.fetches) != fetchCount || len(restarted.fetches) != 0 {
		t.Fatalf("an unlinked node polled the primary: %v %v", h.fetches, restarted.fetches)
	}
	if len(*rcalls) != 1 || !(*rcalls)[0] {
		t.Fatalf("restart must restore the unlinked overlay, calls=%v", *rcalls)
	}
}

func TestNodeSyncAckUnlinkedMarksUnlinkedAndKeepsApply(t *testing.T) {
	envelope, body := syncTestEnvelope(t, "v9")
	h := newNodeSyncHarness(t, strings.Repeat("a", 64))
	h.fetchFn = func(string) (SnapshotFetchResult, error) {
		return SnapshotFetchResult{Body: body, ContentHash: envelope.ContentHash, AppliedAt: h.now}, nil
	}
	h.ackFn = func(string) error { return ErrSnapshotClientUnlinked }
	if err := h.syncer.RunOnce(context.Background()); err == nil {
		t.Fatal("expected node_unlinked")
	}
	st := h.store.state.NodeSyncProgress
	if !st.Unlinked || st.AppliedHash != envelope.ContentHash || st.AckedHash == envelope.ContentHash {
		t.Fatalf("progress=%+v", st)
	}
}

func TestNodeSyncNeverMarksUnlinkedFromNonSignalErrors(t *testing.T) {
	errs := map[string]error{
		"bare 410":            ErrSnapshotClientGone,
		"unauthorized":        ErrSnapshotClientUnauthorized,
		"unavailable":         ErrSnapshotClientUnavailable,
		"invalid":             ErrSnapshotClientInvalid,
		"untrusted tls":       ErrSnapshotClientUntrusted,
		"not found":           ErrSnapshotClientNotFound,
		"generic network":     errors.New("dial tcp: connection refused"),
		"wrapped ctx timeout": fmt.Errorf("fetch: %w", context.DeadlineExceeded),
	}
	for name, fetchErr := range errs {
		for _, phase := range []string{"fetch", "ack"} {
			t.Run(name+"/"+phase, func(t *testing.T) {
				envelope, body := syncTestEnvelope(t, "vx")
				h := newNodeSyncHarness(t, strings.Repeat("a", 64))
				calls := recordUnlinked(h)
				h.fetchFn = func(string) (SnapshotFetchResult, error) {
					if phase == "fetch" {
						return SnapshotFetchResult{}, fetchErr
					}
					return SnapshotFetchResult{Body: body, ContentHash: envelope.ContentHash, AppliedAt: h.now}, nil
				}
				h.ackFn = func(string) error { return fetchErr }
				_ = h.syncer.RunOnce(context.Background())
				if h.store.state.Unlinked || !h.store.state.UnlinkedAt.IsZero() || len(*calls) != 0 {
					t.Fatalf("marked unlinked from %v: %+v calls=%v", fetchErr, h.store.state.NodeSyncProgress, *calls)
				}
				if h.syncer.delay() >= 5*time.Minute {
					t.Fatalf("ordinary failure must keep ordinary backoff, got %v", h.syncer.delay())
				}
			})
		}
	}
}

type failingLoadStore struct {
	*memoryNodeSyncStore
	err error
}

func (s failingLoadStore) LoadNodeSyncState(context.Context) (configuration.NodeSyncState, bool, error) {
	return configuration.NodeSyncState{}, false, s.err
}

func TestNodeSyncStoreReadErrorNeverMarksUnlinked(t *testing.T) {
	h := newNodeSyncHarness(t, strings.Repeat("a", 64))
	calls := recordUnlinked(h)
	h.syncer.opts.Store = failingLoadStore{memoryNodeSyncStore: h.store, err: errors.New("database is locked")}
	h.fetchFn = func(string) (SnapshotFetchResult, error) { return SnapshotFetchResult{}, ErrSnapshotClientUnlinked }
	err := h.syncer.RunOnce(context.Background())
	var failure *NodeSyncFailure
	if !errors.As(err, &failure) || failure.Code != "state_unavailable" {
		t.Fatalf("err=%v want state_unavailable", err)
	}
	if len(h.fetches) != 0 || h.store.state.Unlinked || len(*calls) != 0 {
		t.Fatalf("fetches=%v state=%+v calls=%v", h.fetches, h.store.state.NodeSyncProgress, *calls)
	}
}

func TestNodeSyncUnlinkSignalNotNotifiedWhenPersistenceFails(t *testing.T) {
	h := newNodeSyncHarness(t, strings.Repeat("a", 64))
	calls := recordUnlinked(h)
	h.store.saveErr = errors.New("disk full")
	h.fetchFn = func(string) (SnapshotFetchResult, error) { return SnapshotFetchResult{}, ErrSnapshotClientUnlinked }
	err := h.syncer.RunOnce(context.Background())
	var failure *NodeSyncFailure
	if !errors.As(err, &failure) || failure.Code != "state_unavailable" {
		t.Fatalf("err=%v", err)
	}
	if len(*calls) != 0 || h.store.state.Unlinked {
		t.Fatalf("overlay must follow persisted truth: calls=%v state=%+v", *calls, h.store.state.NodeSyncProgress)
	}
}
