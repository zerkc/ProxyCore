package store_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/worker/jobs"
	"github.com/zerkc/ProxyCore/apps/api/internal/worker/store"
	"github.com/zerkc/ProxyCore/apps/api/internal/worker/testsupport"
)

// bunJob is the subset of the TS JobRecord the interop assertions read.
type bunJob struct {
	ID               string          `json:"id"`
	Status           string          `json:"status"`
	Target           string          `json:"target"`
	CorrelationID    string          `json:"correlationId"`
	CreatedAt        time.Time       `json:"createdAt"`
	ClaimedAt        *time.Time      `json:"claimedAt"`
	StartedAt        *time.Time      `json:"startedAt"`
	FinishedAt       *time.Time      `json:"finishedAt"`
	ValidationOutput json.RawMessage `json:"validationOutput"`
	ErrorMessage     *string         `json:"errorMessage"`
}

// bunStores runs one operation of the real Bun stores against the shared file.
func bunStores(t *testing.T, path, operation string, input any, out any) {
	t.Helper()
	payload, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	script := testsupport.RepoPath("apps/api/internal/worker/testdata/stores.bun.ts")
	output := strings.TrimSpace(testsupport.RunBun(t, script, path, operation, string(payload)))
	if err := json.Unmarshal([]byte(output), out); err != nil {
		t.Fatalf("decode bun %s output %q: %v", operation, output, err)
	}
}

func TestBunAndGoStoresShareJobLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns Bun; skipped with -short")
	}
	testsupport.Bun(t)
	db, path := testsupport.NewDB(t)
	rev := testsupport.UUID(1)
	testsupport.InsertRevision(t, db, rev, 1, nil)
	ctx := context.Background()
	clock := base.Add(time.Hour)
	s := store.New(db, store.WithClock(func() time.Time { return clock }))

	// Go enqueues, Bun claims.
	queued, err := s.Enqueue(ctx, store.EnqueueInput{RevisionID: rev, Target: store.TargetCombined,
		CorrelationID: "go-made", ValidationOutput: json.RawMessage(`{"steps":[1]}`)})
	if err != nil {
		t.Fatal(err)
	}
	var claimed bunJob
	bunStores(t, path, "claimNext", map[string]any{}, &claimed)
	if claimed.ID != queued.ID || claimed.Status != "validating" || claimed.ClaimedAt == nil || claimed.CorrelationID != "go-made" {
		t.Fatalf("bun claim of Go row = %+v", claimed)
	}
	if string(claimed.ValidationOutput) != `{"steps":[1]}` {
		t.Fatalf("bun saw output %s", claimed.ValidationOutput)
	}
	goView, err := s.Get(ctx, queued.ID)
	if err != nil || goView == nil || goView.Status != jobs.Validating || !goView.ClaimedAt.Equal(*claimed.ClaimedAt) {
		t.Fatalf("go view after bun claim = %+v, %v", goView, err)
	}

	// Bun updates, Go reads the patched columns back.
	finished := base.Add(2 * time.Hour).Truncate(time.Millisecond)
	var updated bunJob
	bunStores(t, path, "update", map[string]any{"id": queued.ID, "patch": map[string]any{
		"status": "failed", "errorMessage": "bun failed it", "finishedAt": finished.Format(time.RFC3339Nano),
		"healthOutput": map[string]any{"ok": false},
	}}, &updated)
	goView, _ = s.Get(ctx, queued.ID)
	if goView.Status != jobs.Failed || *goView.ErrorMessage != "bun failed it" ||
		!goView.FinishedAt.Equal(finished) || string(goView.HealthOutput) != `{"ok":false}` {
		t.Fatalf("go view after bun update = %+v", goView)
	}

	// Go requeues the failed job; Bun claims, Go recovers the stale claim, Bun sees it queued.
	if _, err := s.Update(ctx, queued.ID, store.JobPatch{Status: ptr(jobs.Queued)}); err != nil {
		t.Fatal(err)
	}
	var reclaimed bunJob
	bunStores(t, path, "claimNext", map[string]any{"target": "combined"}, &reclaimed)
	if reclaimed.Status != "validating" || reclaimed.ClaimedAt == nil {
		t.Fatalf("second bun claim = %+v", reclaimed)
	}
	clock = reclaimed.ClaimedAt.Add(10 * time.Minute)
	if n, err := s.RecoverStale(ctx, 60_000); err != nil || n != 1 {
		t.Fatalf("go RecoverStale = %d, %v", n, err)
	}
	var seen bunJob
	bunStores(t, path, "get", map[string]any{"id": queued.ID}, &seen)
	if seen.Status != "queued" || seen.ClaimedAt != nil || seen.StartedAt != nil ||
		seen.ErrorMessage == nil || *seen.ErrorMessage != "Recovered after worker lease expiration" {
		t.Fatalf("bun view of Go recovery = %+v", seen)
	}

	// Bun recovers a Go claim.
	if _, err := s.ClaimNext(ctx, ""); err != nil {
		t.Fatal(err)
	}
	var count int
	bunStores(t, path, "recoverStale", map[string]any{"leaseMs": 1000, "now": clock.Add(time.Hour).Format(time.RFC3339Nano)}, &count)
	if count != 1 {
		t.Fatalf("bun recoverStale of Go claim = %d", count)
	}

	// The global active guard is shared: Bun sees the Go-queued job as active.
	var blocked *bunJob
	bunStores(t, path, "enqueueIfNotActive", map[string]any{"revisionId": rev, "target": "nginx", "correlationId": "bun-blocked"}, &blocked)
	if blocked != nil {
		t.Fatalf("bun enqueueIfNotActive inserted despite an active Go job: %+v", blocked)
	}
	if _, err := s.Update(ctx, queued.ID, store.JobPatch{Status: ptr(jobs.Validating)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Update(ctx, queued.ID, store.JobPatch{Status: ptr(jobs.Failed)}); err != nil {
		t.Fatal(err)
	}
	var enqueued *bunJob
	bunStores(t, path, "enqueueIfNotActive", map[string]any{"revisionId": rev, "target": "nginx", "correlationId": "bun-made"}, &enqueued)
	if enqueued == nil || enqueued.Status != "queued" {
		t.Fatalf("bun enqueueIfNotActive = %+v", enqueued)
	}
	next, err := s.ClaimNext(ctx, store.TargetNginx)
	if err != nil || next == nil || next.ID != enqueued.ID || next.CorrelationID != "bun-made" {
		t.Fatalf("go claim of Bun row = %+v, %v", next, err)
	}
	guard, err := s.EnqueueIfNotActive(ctx, store.EnqueueInput{RevisionID: rev, Target: store.TargetNginx, CorrelationID: "go-blocked"})
	if err != nil || guard != nil {
		t.Fatalf("go enqueueIfNotActive over active Bun job = %+v, %v", guard, err)
	}
}
