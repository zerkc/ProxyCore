package configuration

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

const (
	jobRevID = "bbbbbbbb-0000-4000-8000-0000000000b1"
	jobIDA   = "bbbbbbbb-0000-4000-8000-0000000000a1"
)

func seedApplyJob(t *testing.T, db *sql.DB, id, status string, finished, message any) {
	t.Helper()
	settingsExec(t, db, `insert or ignore into config_revisions (id,revision_number,checksum,snapshot,created_at) values (?,1,'sum','{}','0')`, jobRevID)
	settingsExec(t, db, `insert into apply_jobs (id,revision_id,target,status,correlation_id,created_at,finished_at,error_message)
		values (?,?,'combined',?,'corr','0',?,?)`, id, jobRevID, status, finished, message)
}

func TestSQLiteApplyJobTerminalMatchesPGProjection(t *testing.T) {
	db, _ := enqueueFixture(t)
	store := NewSQLiteApplyJobStore(db)
	stamp := *certKey(t, 1700000000, 5)
	seedApplyJob(t, db, jobIDA, "failed", stamp, "nginx failed")

	got, err := store.GetApplyJobTerminal(context.Background(), uuid.MustParse(jobIDA))
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != ApplyTerminalStatusFailed || got.FailureCode == nil || *got.FailureCode != "nginx failed" ||
		got.FinishedAt == nil || got.FinishedAt.Unix() != 1700000000 || got.FinishedAt.Nanosecond() != 5 {
		t.Fatalf("unexpected projection: %+v", got)
	}

	settingsExec(t, db, `update apply_jobs set status='queued', finished_at=null, error_message=null`)
	got, err = store.GetApplyJobTerminal(context.Background(), uuid.MustParse(jobIDA))
	if err != nil || got.Status != "queued" || got.FinishedAt != nil || got.FailureCode != nil {
		t.Fatalf("queued projection: %+v %v", got, err)
	}
}

func TestSQLiteApplyJobTerminalErrorsMatchPG(t *testing.T) {
	db, _ := enqueueFixture(t)
	store := NewSQLiteApplyJobStore(db)
	missing := uuid.MustParse("bbbbbbbb-0000-4000-8000-0000000000ff")
	if _, err := store.GetApplyJobTerminal(context.Background(), missing); err == nil || err.Error() != "apply job "+missing.String()+" not found" {
		t.Fatalf("missing job error: %v", err)
	}
	if _, err := store.GetApplyJobTerminal(context.Background(), uuid.Nil); err == nil || err.Error() != "apply status job id is required" {
		t.Fatalf("nil job error: %v", err)
	}
	if _, err := store.GetApplyJobTerminal(nil, missing); err == nil || err.Error() != "apply status context is required" { //nolint:staticcheck
		t.Fatalf("nil ctx error: %v", err)
	}
	if _, err := (&SQLiteApplyJobStore{}).GetApplyJobTerminal(context.Background(), missing); err == nil || err.Error() != "apply status store is required" {
		t.Fatalf("nil store error: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	seedApplyJob(t, db, jobIDA, "applied", nil, nil)
	if _, err := store.GetApplyJobTerminal(ctx, uuid.MustParse(jobIDA)); err == nil || !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("canceled ctx error: %v", err)
	}
}

func TestSQLiteWaitForJobSemantics(t *testing.T) {
	db, _ := enqueueFixture(t)
	ctx := context.Background()
	poll := 5 * time.Millisecond
	set := func(status string, message any) {
		settingsExec(t, db, `update apply_jobs set status=?, error_message=?`, status, message)
	}
	seedApplyJob(t, db, jobIDA, "applied", nil, nil)
	cases := []struct {
		name, status string
		message      any
		want         string
	}{
		{"applied", "applied", nil, ""},
		{"failed without message", "failed", nil, "HTTP-01 challenge route apply failed: failed"},
		{"failed with message", "failed", "nginx failed", "HTTP-01 challenge route apply failed: nginx failed"},
		{"rolled back without message", "rolled-back", nil, "HTTP-01 challenge route apply failed: rolled-back"},
		{"rolled back with message", "rolled-back", "rb", "HTTP-01 challenge route apply failed: rb"},
		{"queued times out", "queued", nil, "Timed out waiting for the HTTP-01 challenge route"},
		{"applying times out", "applying", nil, "Timed out waiting for the HTTP-01 challenge route"},
	}
	for _, c := range cases {
		set(c.status, c.message)
		err := sqliteWaitForJob(ctx, db, jobIDA, 40*time.Millisecond, poll)
		if (c.want == "") != (err == nil) || (err != nil && err.Error() != c.want) {
			t.Errorf("%s: got %v want %q", c.name, err, c.want)
		}
	}
	if err := sqliteWaitForJob(ctx, db, "bbbbbbbb-0000-4000-8000-0000000000ff", time.Second, poll); err == nil || err.Error() != "Certificate apply job disappeared: bbbbbbbb-0000-4000-8000-0000000000ff" {
		t.Errorf("missing job: %v", err)
	}
	if err := sqliteWaitForJob(ctx, db, "bad", time.Second, poll); err == nil || err.Error() != `ERROR: invalid input syntax for type uuid: "bad" (SQLSTATE 22P02)` {
		t.Errorf("malformed id: %v", err)
	}
	if err := sqliteWaitForJob(ctx, db, jobIDA, 0, poll); err == nil || err.Error() != "Timed out waiting for the HTTP-01 challenge route" {
		t.Errorf("zero timeout must time out without polling: %v", err)
	}
}

func TestSQLiteWaitForJobObservesLateTransitionAndCancellation(t *testing.T) {
	db, _ := enqueueFixture(t)
	seedApplyJob(t, db, jobIDA, "queued", nil, nil)
	go func() {
		time.Sleep(60 * time.Millisecond)
		_, _ = db.Exec(`update apply_jobs set status='applied'`)
	}()
	if err := sqliteWaitForJob(context.Background(), db, jobIDA, 5*time.Second, 5*time.Millisecond); err != nil {
		t.Fatalf("a job that becomes applied while polling must succeed: %v", err)
	}

	settingsExec(t, db, `update apply_jobs set status='queued'`)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	start := time.Now()
	err := sqliteWaitForJob(ctx, db, jobIDA, 30*time.Second, time.Hour)
	if !errors.Is(err, context.Canceled) || time.Since(start) > 5*time.Second {
		t.Fatalf("cancellation must interrupt the pause promptly: %v after %v", err, time.Since(start))
	}
}
