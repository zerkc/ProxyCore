package revisions_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/worker/revisions"
	"github.com/zerkc/ProxyCore/apps/api/internal/worker/testsupport"
)

type bunRevision struct {
	ID             string     `json:"id"`
	RevisionNumber int        `json:"revisionNumber"`
	Checksum       string     `json:"checksum"`
	AppliedAt      *time.Time `json:"appliedAt"`
}

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

func TestBunAndGoRevisionStoresShareMarkApplied(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns Bun; skipped with -short")
	}
	testsupport.Bun(t)
	db, path := testsupport.NewDB(t)
	testsupport.InsertRevision(t, db, testsupport.UUID(1), 1, nil)
	testsupport.InsertRevision(t, db, testsupport.UUID(2), 2, nil)
	s := revisions.New(db)
	ctx := context.Background()
	goAppliedAt := time.Date(2026, 4, 2, 3, 4, 5, 678_000_000, time.UTC)

	// Go marks applied; the Bun reads see the revision and the settings pointer.
	if _, err := s.MarkApplied(ctx, testsupport.UUID(1), goAppliedAt); err != nil {
		t.Fatal(err)
	}
	var seen bunRevision
	bunStores(t, path, "getRevision", map[string]any{"id": testsupport.UUID(1)}, &seen)
	if seen.AppliedAt == nil || !seen.AppliedAt.Equal(goAppliedAt) || seen.Checksum != "checksum-1" {
		t.Fatalf("bun view of Go markApplied = %+v", seen)
	}

	// Bun marks the newer revision applied; Go reads it back and the shared settings row follows.
	bunAppliedAt := goAppliedAt.Add(time.Hour)
	var marked bunRevision
	bunStores(t, path, "markApplied", map[string]any{"id": testsupport.UUID(2), "appliedAt": bunAppliedAt.Format(time.RFC3339Nano)}, &marked)
	got, err := s.Latest(ctx)
	if err != nil || got == nil || got.ID != testsupport.UUID(2) || got.AppliedAt == nil || !got.AppliedAt.Equal(bunAppliedAt) {
		t.Fatalf("go view of Bun markApplied = %+v, %v", got, err)
	}
	var current string
	if err := db.QueryRow("select current_applied_revision_id from installation_settings where id='default'").Scan(&current); err != nil || current != testsupport.UUID(2) {
		t.Fatalf("settings pointer after Bun markApplied = %q, %v", current, err)
	}

	// Go re-marks the older revision; Bun latest still orders by revision number and the pointer moves.
	if _, err := s.MarkApplied(ctx, testsupport.UUID(1), bunAppliedAt.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	var latest bunRevision
	bunStores(t, path, "latestRevision", map[string]any{}, &latest)
	if latest.ID != testsupport.UUID(2) || latest.RevisionNumber != 2 {
		t.Fatalf("bun latest = %+v", latest)
	}
	if err := db.QueryRow("select current_applied_revision_id from installation_settings where id='default'").Scan(&current); err != nil || current != testsupport.UUID(1) {
		t.Fatalf("settings pointer after Go markApplied = %q, %v", current, err)
	}
}
