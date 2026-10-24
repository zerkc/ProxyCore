package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/worker/jobs"
	"github.com/zerkc/ProxyCore/apps/api/internal/worker/store"
	"github.com/zerkc/ProxyCore/apps/api/internal/worker/testsupport"
)

var base = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

func ptr[T any](v T) *T { return &v }

func TestClaimNextTakesOldestQueuedJobAndStampsIt(t *testing.T) {
	db, _ := testsupport.NewDB(t)
	rev := testsupport.UUID(1)
	testsupport.InsertRevision(t, db, rev, 1, nil)
	clock := base.Add(time.Hour)
	s := store.New(db, store.WithClock(func() time.Time { return clock }))
	testsupport.InsertJob(t, db, testsupport.UUID(11), rev, "queued", testsupport.Stamp(t, base.Add(2*time.Second)), nil)
	testsupport.InsertJob(t, db, testsupport.UUID(10), rev, "queued", testsupport.Stamp(t, base), nil)
	testsupport.InsertJob(t, db, testsupport.UUID(12), rev, "failed", testsupport.Stamp(t, base.Add(-time.Minute)), nil)

	got, err := s.ClaimNext(context.Background(), "")
	if err != nil || got == nil {
		t.Fatalf("ClaimNext = %v, %v", got, err)
	}
	if got.ID != testsupport.UUID(10) || got.Status != jobs.Validating {
		t.Fatalf("claimed %s as %s, want oldest queued as validating", got.ID, got.Status)
	}
	if got.ClaimedAt == nil || !got.ClaimedAt.Equal(clock) || got.StartedAt == nil || !got.StartedAt.Equal(clock) {
		t.Fatalf("claimed_at=%v started_at=%v, want %v", got.ClaimedAt, got.StartedAt, clock)
	}
}

func TestClaimNextKeepsExistingStartedAtAndHonoursTarget(t *testing.T) {
	db, _ := testsupport.NewDB(t)
	rev := testsupport.UUID(1)
	testsupport.InsertRevision(t, db, rev, 1, nil)
	clock := base.Add(time.Hour)
	s := store.New(db, store.WithClock(func() time.Time { return clock }))
	started := testsupport.Stamp(t, base.Add(time.Minute))
	if _, err := db.Exec(`insert into apply_jobs (id,revision_id,target,status,correlation_id,created_at,started_at)
		values (?,?,'nginx','queued','c',?,?)`, testsupport.UUID(20), rev, testsupport.Stamp(t, base), started); err != nil {
		t.Fatal(err)
	}
	testsupport.InsertJob(t, db, testsupport.UUID(21), rev, "queued", testsupport.Stamp(t, base.Add(-time.Hour)), nil)

	got, err := s.ClaimNext(context.Background(), store.TargetNginx)
	if err != nil || got == nil || got.ID != testsupport.UUID(20) {
		t.Fatalf("ClaimNext(nginx) = %+v, %v", got, err)
	}
	if !got.StartedAt.Equal(base.Add(time.Minute)) || !got.ClaimedAt.Equal(clock) {
		t.Fatalf("started_at=%v claimed_at=%v, want preserved start and fresh claim", got.StartedAt, got.ClaimedAt)
	}
	if again, err := s.ClaimNext(context.Background(), store.TargetNginx); err != nil || again != nil {
		t.Fatalf("second nginx claim = %+v, %v; want nil", again, err)
	}
}

func TestClaimNextWithoutQueuedJobsReturnsNil(t *testing.T) {
	db, _ := testsupport.NewDB(t)
	s := store.New(db)
	got, err := s.ClaimNext(context.Background(), "")
	if err != nil || got != nil {
		t.Fatalf("ClaimNext on empty table = %+v, %v", got, err)
	}
}

func TestClaimNextRejectsUnknownTarget(t *testing.T) {
	db, _ := testsupport.NewDB(t)
	if _, err := store.New(db).ClaimNext(context.Background(), "other"); err == nil {
		t.Fatal("expected an error for an unknown target")
	}
}

func TestConcurrentClaimsTakeEachJobExactlyOnce(t *testing.T) {
	db, _ := testsupport.NewDB(t)
	rev := testsupport.UUID(1)
	testsupport.InsertRevision(t, db, rev, 1, nil)
	const jobsCount = 12
	for i := 0; i < jobsCount; i++ {
		testsupport.InsertJob(t, db, testsupport.UUID(100+i), rev, "queued", testsupport.Stamp(t, base.Add(time.Duration(i)*time.Second)), nil)
	}
	// Two independent stores model two worker connections on the same file.
	stores := []*store.Store{store.New(db), store.New(db)}
	var mu sync.Mutex
	claimed := map[string]int{}
	var wg sync.WaitGroup
	for _, s := range stores {
		for w := 0; w < 3; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					job, err := s.ClaimNext(context.Background(), "")
					if err != nil {
						t.Error(err)
						return
					}
					if job == nil {
						return
					}
					mu.Lock()
					claimed[job.ID]++
					mu.Unlock()
				}
			}()
		}
	}
	wg.Wait()
	if len(claimed) != jobsCount {
		t.Fatalf("claimed %d distinct jobs, want %d", len(claimed), jobsCount)
	}
	for id, n := range claimed {
		if n != 1 {
			t.Fatalf("job %s claimed %d times", id, n)
		}
	}
}

func TestRecoverStaleRequeuesExpiredClaims(t *testing.T) {
	db, _ := testsupport.NewDB(t)
	rev := testsupport.UUID(1)
	testsupport.InsertRevision(t, db, rev, 1, nil)
	now := base.Add(10 * time.Minute)
	s := store.New(db, store.WithClock(func() time.Time { return now }))
	stamp := func(at time.Time) any { return testsupport.Stamp(t, at) }
	old, fresh := stamp(base), stamp(now.Add(-time.Second))
	testsupport.InsertJob(t, db, testsupport.UUID(1), rev, "validating", testsupport.Stamp(t, base), old)
	testsupport.InsertJob(t, db, testsupport.UUID(2), rev, "applying", testsupport.Stamp(t, base), old)
	testsupport.InsertJob(t, db, testsupport.UUID(3), rev, "validating", testsupport.Stamp(t, base), fresh)
	testsupport.InsertJob(t, db, testsupport.UUID(4), rev, "failed", testsupport.Stamp(t, base), old)
	testsupport.InsertJob(t, db, testsupport.UUID(5), rev, "validating", testsupport.Stamp(t, base), nil)
	if _, err := db.Exec("update apply_jobs set started_at=? where id=?", old, testsupport.UUID(1)); err != nil {
		t.Fatal(err)
	}

	n, err := s.RecoverStale(context.Background(), 120_000)
	if err != nil || n != 2 {
		t.Fatalf("RecoverStale = %d, %v; want 2", n, err)
	}
	want := map[int]string{1: "queued", 2: "queued", 3: "validating", 4: "failed", 5: "validating"}
	for id, status := range want {
		got, err := s.Get(context.Background(), testsupport.UUID(id))
		if err != nil || got == nil || string(got.Status) != status {
			t.Fatalf("job %d = %+v, %v; want status %s", id, got, err, status)
		}
	}
	recovered, _ := s.Get(context.Background(), testsupport.UUID(1))
	if recovered.ClaimedAt != nil || recovered.StartedAt != nil ||
		recovered.ErrorMessage == nil || *recovered.ErrorMessage != "Recovered after worker lease expiration" {
		t.Fatalf("recovered job not reset: %+v", recovered)
	}
}

func TestGetAndListProjectRows(t *testing.T) {
	db, _ := testsupport.NewDB(t)
	rev := testsupport.UUID(1)
	testsupport.InsertRevision(t, db, rev, 1, nil)
	s := store.New(db)
	ctx := context.Background()
	if got, err := s.Get(ctx, "missing"); err != nil || got != nil {
		t.Fatalf("Get(missing) = %+v, %v", got, err)
	}
	for i := 0; i < 3; i++ {
		testsupport.InsertJob(t, db, testsupport.UUID(30+i), rev, "queued", testsupport.Stamp(t, base.Add(time.Duration(i)*time.Second)), nil)
	}
	if _, err := db.Exec(`update apply_jobs set validation_output='{"steps":[1]}', apply_output='null',
		snapshot_version=7, leadership_generation=9007199254740993 where id=?`, testsupport.UUID(31)); err != nil {
		t.Fatal(err)
	}
	list, err := s.List(ctx)
	if err != nil || len(list) != 3 {
		t.Fatalf("List = %d rows, %v", len(list), err)
	}
	if list[0].ID != testsupport.UUID(32) || list[2].ID != testsupport.UUID(30) {
		t.Fatalf("List order = %s ... %s, want created_at DESC", list[0].ID, list[2].ID)
	}
	got, err := s.Get(ctx, testsupport.UUID(31))
	if err != nil || got == nil {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	if string(got.ValidationOutput) != `{"steps":[1]}` || got.ApplyOutput != nil {
		t.Fatalf("outputs = %s / %s; JSON null must project to absent", got.ValidationOutput, got.ApplyOutput)
	}
	if got.SnapshotVersion == nil || *got.SnapshotVersion != 7 ||
		got.LeadershipGeneration == nil || *got.LeadershipGeneration != 9007199254740993 {
		t.Fatalf("integers = %v %v", got.SnapshotVersion, got.LeadershipGeneration)
	}
	if !got.CreatedAt.Equal(base.Add(time.Second)) || got.Target != store.TargetCombined || got.Source != store.SourceOrdinary {
		t.Fatalf("scalars = %v %v %v", got.CreatedAt, got.Target, got.Source)
	}
}

func TestUpdatePatchesColumnsAndEnforcesTransitions(t *testing.T) {
	db, _ := testsupport.NewDB(t)
	rev := testsupport.UUID(1)
	testsupport.InsertRevision(t, db, rev, 1, nil)
	s := store.New(db)
	ctx := context.Background()
	id := testsupport.UUID(40)
	testsupport.InsertJob(t, db, id, rev, "queued", testsupport.Stamp(t, base), nil)

	if _, err := s.Update(ctx, id, store.JobPatch{}); err == nil || err.Error() != "No values to set" {
		t.Fatalf("empty patch error = %v", err)
	}
	if _, err := s.Update(ctx, testsupport.UUID(99), store.JobPatch{ErrorMessage: store.Set("x")}); !errors.Is(err, store.ErrJobNotFound) {
		t.Fatalf("missing job error = %v", err)
	}
	if _, err := s.Update(ctx, id, store.JobPatch{Status: ptr(jobs.Applied)}); !errors.Is(err, jobs.ErrInvalidTransition) {
		t.Fatalf("queued -> applied error = %v, want ErrInvalidTransition", err)
	}
	if got, _ := s.Get(ctx, id); got.Status != jobs.Queued {
		t.Fatalf("rejected transition changed status to %s", got.Status)
	}

	finished := base.Add(time.Hour).Add(123456789 * time.Nanosecond)
	steps := []store.JobPatch{
		{Status: ptr(jobs.Validating)},
		{Status: ptr(jobs.Applying), ValidationOutput: store.Set(json.RawMessage(`{ "ok": true }`))},
		{Status: ptr(jobs.Applied), ApplyOutput: store.Set(json.RawMessage(`[1,2]`)),
			HealthOutput: store.Set(json.RawMessage(`"fine"`)), FinishedAt: store.Set(finished),
			ClaimedAt: store.Set(base), StartedAt: store.Set(base), ErrorMessage: store.Set("note")},
	}
	var got store.Job
	for _, patch := range steps {
		var err error
		if got, err = s.Update(ctx, id, patch); err != nil {
			t.Fatal(err)
		}
	}
	if got.Status != jobs.Applied || string(got.ValidationOutput) != `{"ok":true}` ||
		string(got.ApplyOutput) != `[1,2]` || string(got.HealthOutput) != `"fine"` ||
		!got.FinishedAt.Equal(finished) || *got.ErrorMessage != "note" {
		t.Fatalf("patched job = %+v", got)
	}

	cleared, err := s.Update(ctx, id, store.JobPatch{ErrorMessage: store.Clear[string](), ApplyOutput: store.Clear[json.RawMessage]()})
	if err != nil || cleared.ErrorMessage != nil || cleared.ApplyOutput != nil {
		t.Fatalf("clear = %+v, %v", cleared, err)
	}
	if _, err := s.Update(ctx, id, store.JobPatch{ValidationOutput: store.Set(json.RawMessage(`{bad`))}); err == nil {
		t.Fatal("invalid JSON output must be rejected")
	}
}

func TestEnqueueIfNotActiveHonoursGlobalGuard(t *testing.T) {
	db, _ := testsupport.NewDB(t)
	rev := testsupport.UUID(1)
	testsupport.InsertRevision(t, db, rev, 1, nil)
	clock := base.Add(5 * time.Minute)
	s := store.New(db, store.WithClock(func() time.Time { return clock }))
	ctx := context.Background()
	input := store.EnqueueInput{RevisionID: rev, Target: store.TargetNginx, CorrelationID: "corr-new"}

	for _, status := range []string{"queued", "validating", "applying"} {
		testsupport.InsertJob(t, db, testsupport.UUID(50), rev, status, testsupport.Stamp(t, base), nil)
		got, err := s.EnqueueIfNotActive(ctx, input)
		if err != nil || got != nil {
			t.Fatalf("active %s job: got %+v, %v; want nil", status, got, err)
		}
		if _, err := db.Exec("delete from apply_jobs"); err != nil {
			t.Fatal(err)
		}
	}
	testsupport.InsertJob(t, db, testsupport.UUID(51), rev, "applied", testsupport.Stamp(t, base), nil)
	testsupport.InsertJob(t, db, testsupport.UUID(52), rev, "failed", testsupport.Stamp(t, base), nil)
	got, err := s.EnqueueIfNotActive(ctx, input)
	if err != nil || got == nil {
		t.Fatalf("EnqueueIfNotActive = %+v, %v", got, err)
	}
	if got.Status != jobs.Queued || got.Source != store.SourceOrdinary || got.Target != store.TargetNginx ||
		!got.CreatedAt.Equal(clock) || got.CorrelationID != "corr-new" {
		t.Fatalf("inserted job = %+v", got)
	}
	again, err := s.EnqueueIfNotActive(ctx, input)
	if err != nil || again != nil {
		t.Fatalf("second enqueue = %+v, %v; the first job is now active", again, err)
	}
}

func TestEnqueueIfNotActiveRacesInsertExactlyOneJob(t *testing.T) {
	db, _ := testsupport.NewDB(t)
	rev := testsupport.UUID(1)
	testsupport.InsertRevision(t, db, rev, 1, nil)
	s := store.New(db)
	var wg sync.WaitGroup
	var mu sync.Mutex
	inserted := 0
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got, err := s.EnqueueIfNotActive(context.Background(), store.EnqueueInput{
				RevisionID: rev, Target: store.TargetCombined, CorrelationID: fmt.Sprintf("race-%d", i)})
			if err != nil {
				t.Error(err)
				return
			}
			if got != nil {
				mu.Lock()
				inserted++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if inserted != 1 {
		t.Fatalf("%d concurrent enqueues inserted, want exactly 1", inserted)
	}
}
