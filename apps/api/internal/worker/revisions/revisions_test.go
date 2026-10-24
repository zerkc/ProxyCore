package revisions_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/worker/revisions"
	"github.com/zerkc/ProxyCore/apps/api/internal/worker/store"
	"github.com/zerkc/ProxyCore/apps/api/internal/worker/testsupport"
)

var base = time.Date(2026, 3, 1, 12, 0, 0, 123456789, time.UTC)

func settingsApplied(t *testing.T, db *sql.DB) (id sql.NullString, updated string, count int) {
	t.Helper()
	if err := db.QueryRow("select count(*) from installation_settings").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count > 0 {
		if err := db.QueryRow("select current_applied_revision_id, updated_at from installation_settings where id='default'").Scan(&id, &updated); err != nil {
			t.Fatal(err)
		}
	}
	return id, updated, count
}

func TestGetAndLatestProjectRevisions(t *testing.T) {
	db, _ := testsupport.NewDB(t)
	s := revisions.New(db)
	ctx := context.Background()
	if got, err := s.Get(ctx, "missing"); err != nil || got != nil {
		t.Fatalf("Get(missing) = %+v, %v", got, err)
	}
	if got, err := s.Latest(ctx); err != nil || got != nil {
		t.Fatalf("Latest on empty table = %+v, %v", got, err)
	}
	testsupport.InsertRevision(t, db, testsupport.UUID(1), 1, nil)
	testsupport.InsertRevision(t, db, testsupport.UUID(2), 2, testsupport.Stamp(t, base))
	latest, err := s.Latest(ctx)
	if err != nil || latest == nil || latest.ID != testsupport.UUID(2) || latest.RevisionNumber != 2 {
		t.Fatalf("Latest = %+v, %v", latest, err)
	}
	if latest.AppliedAt == nil || !latest.AppliedAt.Equal(base) || string(latest.Snapshot) != `{"zones":[]}` ||
		latest.Source != store.SourceOrdinary || latest.Checksum != "checksum-2" {
		t.Fatalf("projection = %+v", latest)
	}
	first, err := s.Get(ctx, testsupport.UUID(1))
	if err != nil || first == nil || first.AppliedAt != nil {
		t.Fatalf("Get(unapplied) = %+v, %v", first, err)
	}
}

func TestLatestAppliedPrefersLatestThenFallsBackToAppliedJobs(t *testing.T) {
	db, _ := testsupport.NewDB(t)
	s := revisions.New(db)
	ctx := context.Background()
	if got, err := s.LatestApplied(ctx); err != nil || got != nil {
		t.Fatalf("empty = %+v, %v", got, err)
	}
	old, newer, newest := testsupport.UUID(1), testsupport.UUID(2), testsupport.UUID(3)
	testsupport.InsertRevision(t, db, old, 1, testsupport.Stamp(t, base))
	testsupport.InsertRevision(t, db, newer, 2, testsupport.Stamp(t, base.Add(time.Hour)))
	testsupport.InsertRevision(t, db, newest, 3, nil)
	// An applied timestamp without an applied job does not count for the fallback.
	if got, err := s.LatestApplied(ctx); err != nil || got != nil {
		t.Fatalf("no applied jobs = %+v, %v", got, err)
	}
	testsupport.InsertJob(t, db, testsupport.UUID(10), old, "applied", testsupport.Stamp(t, base), nil)
	testsupport.InsertJob(t, db, testsupport.UUID(11), newer, "applied", testsupport.Stamp(t, base), nil)
	testsupport.InsertJob(t, db, testsupport.UUID(12), newest, "failed", testsupport.Stamp(t, base), nil)
	got, err := s.LatestApplied(ctx)
	if err != nil || got == nil || got.ID != newer {
		t.Fatalf("fallback = %+v, %v; want the most recently applied job revision", got, err)
	}
	if _, err := s.MarkApplied(ctx, newest, base.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got, err := s.LatestApplied(ctx); err != nil || got == nil || got.ID != newest {
		t.Fatalf("latest applied = %+v, %v", got, err)
	}
}

func TestMarkAppliedUpsertsInstallationSettingsInOneTransaction(t *testing.T) {
	db, _ := testsupport.NewDB(t)
	clock := base.Add(24 * time.Hour)
	s := revisions.New(db, revisions.WithClock(func() time.Time { return clock }))
	ctx := context.Background()
	testsupport.InsertRevision(t, db, testsupport.UUID(1), 1, nil)
	testsupport.InsertRevision(t, db, testsupport.UUID(2), 2, nil)

	if _, _, count := settingsApplied(t, db); count != 0 {
		t.Fatalf("settings rows before = %d", count)
	}
	appliedAt := base
	got, err := s.MarkApplied(ctx, testsupport.UUID(1), appliedAt)
	if err != nil || got.AppliedAt == nil || !got.AppliedAt.Equal(appliedAt) {
		t.Fatalf("MarkApplied = %+v, %v", got, err)
	}
	id, updated, count := settingsApplied(t, db)
	if count != 1 || id.String != testsupport.UUID(1) || updated != testsupport.Stamp(t, appliedAt) {
		t.Fatalf("settings = %q %q rows=%d", id.String, updated, count)
	}
	var created, forwarding string
	if err := db.QueryRow("select created_at, forwarding_rules from installation_settings").Scan(&created, &forwarding); err != nil {
		t.Fatal(err)
	}
	if created != testsupport.Stamp(t, clock) || forwarding != "[]" {
		t.Fatalf("created_at=%q forwarding_rules=%q; want the writer clock and []", created, forwarding)
	}

	// Existing settings are updated, not duplicated; a zero time means the store clock.
	got, err = s.MarkApplied(ctx, testsupport.UUID(2), time.Time{})
	if err != nil || got.AppliedAt == nil || !got.AppliedAt.Equal(clock) {
		t.Fatalf("MarkApplied(zero) = %+v, %v", got, err)
	}
	if id, _, count = settingsApplied(t, db); count != 1 || id.String != testsupport.UUID(2) {
		t.Fatalf("settings after second mark = %q rows=%d", id.String, count)
	}
}

func TestMarkAppliedMissingRevisionChangesNothing(t *testing.T) {
	db, _ := testsupport.NewDB(t)
	s := revisions.New(db)
	_, err := s.MarkApplied(context.Background(), testsupport.UUID(9), base)
	if !errors.Is(err, revisions.ErrRevisionNotFound) {
		t.Fatalf("err = %v, want ErrRevisionNotFound", err)
	}
	if _, _, count := settingsApplied(t, db); count != 0 {
		t.Fatalf("a failed markApplied created %d settings rows", count)
	}
}
