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

func setFinished(t *testing.T, db *sql.DB, jobID string, at time.Time) {
	t.Helper()
	if _, err := db.Exec("update apply_jobs set finished_at=? where id=?", testsupport.Stamp(t, at), jobID); err != nil {
		t.Fatal(err)
	}
}

func TestLatestAppliedEmptyAndFallbackToLatestAppliedRevision(t *testing.T) {
	db, _ := testsupport.NewDB(t)
	s := revisions.New(db)
	ctx := context.Background()
	if got, err := s.LatestApplied(ctx); err != nil || got != nil {
		t.Fatalf("empty = %+v, %v", got, err)
	}
	first, second := testsupport.UUID(1), testsupport.UUID(2)
	testsupport.InsertRevision(t, db, first, 1, testsupport.Stamp(t, base))
	testsupport.InsertRevision(t, db, second, 2, nil)
	// The highest revision number is not applied, so the fallback has nothing.
	if got, err := s.LatestApplied(ctx); err != nil || got != nil {
		t.Fatalf("latest unapplied = %+v, %v", got, err)
	}
	if _, err := s.MarkApplied(ctx, second, base.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got, err := s.LatestApplied(ctx); err != nil || got == nil || got.ID != second {
		t.Fatalf("no applied job falls back to latest applied revision = %+v, %v", got, err)
	}
}

// Revision 43 was just applied by the newest job while revision 45 is an older
// applied revision with a higher number (the API reuses an old revision when a
// snapshot checksum matches). The reconcile target must be 43, never 45.
func TestLatestAppliedFollowsTheNewestAppliedJobNotTheHighestNumber(t *testing.T) {
	db, _ := testsupport.NewDB(t)
	s := revisions.New(db)
	ctx := context.Background()
	just, older := testsupport.UUID(43), testsupport.UUID(45)
	testsupport.InsertRevision(t, db, just, 43, testsupport.Stamp(t, base.Add(10*time.Minute)))
	testsupport.InsertRevision(t, db, older, 45, testsupport.Stamp(t, base))
	testsupport.InsertJob(t, db, testsupport.UUID(10), older, "applied", testsupport.Stamp(t, base), nil)
	testsupport.InsertJob(t, db, testsupport.UUID(11), just, "applied", testsupport.Stamp(t, base.Add(9*time.Minute)), nil)
	setFinished(t, db, testsupport.UUID(10), base)
	setFinished(t, db, testsupport.UUID(11), base.Add(10*time.Minute))
	got, err := s.LatestApplied(ctx)
	if err != nil || got == nil || got.ID != just || got.RevisionNumber != 43 {
		t.Fatalf("LatestApplied = %+v, %v; want revision 43", got, err)
	}
}

func TestLatestAppliedOrdersByFinishedAtThenCreatedAtAndSkipsUnappliedRevisions(t *testing.T) {
	db, _ := testsupport.NewDB(t)
	s := revisions.New(db)
	ctx := context.Background()
	newest, middle, oldest, unapplied := testsupport.UUID(1), testsupport.UUID(2), testsupport.UUID(3), testsupport.UUID(4)
	stamp := func(d time.Duration) string { return testsupport.Stamp(t, base.Add(d)) }
	testsupport.InsertRevision(t, db, newest, 1, stamp(0))
	testsupport.InsertRevision(t, db, middle, 2, stamp(0))
	testsupport.InsertRevision(t, db, oldest, 3, stamp(0))
	testsupport.InsertRevision(t, db, unapplied, 4, nil)
	testsupport.InsertJob(t, db, testsupport.UUID(10), oldest, "applied", stamp(0), nil)
	setFinished(t, db, testsupport.UUID(10), base)
	testsupport.InsertJob(t, db, testsupport.UUID(11), newest, "applied", stamp(0), nil)
	setFinished(t, db, testsupport.UUID(11), base.Add(9*time.Minute))
	// No finished_at: created_at (earlier than newest's finished_at) is used.
	testsupport.InsertJob(t, db, testsupport.UUID(12), middle, "applied", stamp(3*time.Minute), nil)
	// A later applied job whose revision has no applied_at is skipped, and a
	// later job that did not apply does not count.
	testsupport.InsertJob(t, db, testsupport.UUID(13), unapplied, "applied", stamp(20*time.Minute), nil)
	setFinished(t, db, testsupport.UUID(13), base.Add(20*time.Minute))
	testsupport.InsertJob(t, db, testsupport.UUID(14), middle, "failed", stamp(30*time.Minute), nil)
	setFinished(t, db, testsupport.UUID(14), base.Add(30*time.Minute))
	got, err := s.LatestApplied(ctx)
	if err != nil || got == nil || got.ID != newest {
		t.Fatalf("LatestApplied = %+v, %v; want %s", got, err, newest)
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
