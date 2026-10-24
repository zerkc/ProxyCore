package poller_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/worker/apply"
	"github.com/zerkc/ProxyCore/apps/api/internal/worker/control"
	"github.com/zerkc/ProxyCore/apps/api/internal/worker/jobs"
	"github.com/zerkc/ProxyCore/apps/api/internal/worker/poller"
	"github.com/zerkc/ProxyCore/apps/api/internal/worker/render"
	"github.com/zerkc/ProxyCore/apps/api/internal/worker/revisions"
	"github.com/zerkc/ProxyCore/apps/api/internal/worker/store"
	"github.com/zerkc/ProxyCore/apps/api/internal/worker/testsupport"
	"github.com/zerkc/ProxyCore/apps/api/internal/worker/wakeup"
)

const snapshotJSON = `{"settings":{"ingress":{"ipv4":"192.0.2.10"},
  "defaultPool":{"id":"default","endpoints":[{"host":"1.1.1.1","port":53}]},
  "forwardingRules":[],"retentionMaxAgeDays":7,"retentionMaxSizeMb":50},
  "zones":[],"streams":[],"certificates":[]}`

var base = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// scriptedControl answers every request ok; nginx health walks nginxHealth
// (sticking on the last entry), and failOnce fails the first matching operation.
type scriptedControl struct {
	mu          sync.Mutex
	ops         []string
	requests    []control.Request
	nginxHealth []string
	probes      int
	failOnce    string
	failed      bool
	before      func(control.Request)
	driftPath   string
}

func (c *scriptedControl) Execute(_ context.Context, request control.Request) (control.Response, error) {
	if c.before != nil {
		c.before(request)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	key := string(request.Service) + ":" + string(request.Operation)
	c.ops = append(c.ops, key)
	c.requests = append(c.requests, request)
	response := control.Response{RequestID: request.RequestID, Operation: request.Operation, OK: true,
		Output: json.RawMessage(`{"service":"` + string(request.Service) + `"}`)}
	if c.failOnce == string(request.Operation) && !c.failed {
		c.failed = true
		response.OK, response.Output, response.Error = false, nil, "simulated failure"
		return response, nil
	}
	if request.Service == control.ServiceNginx && request.Operation == control.OperationHealth {
		status := "healthy"
		switch {
		case c.driftPath != "":
			if request.CandidatePath == c.driftPath {
				status = "drift"
			}
		case len(c.nginxHealth) > 0:
			status = c.nginxHealth[min(c.probes, len(c.nginxHealth)-1)]
		}
		c.probes++
		response.Output = json.RawMessage(`{"status":"` + status + `"}`)
	}
	return response, nil
}

func (c *scriptedControl) operations() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.ops...)
}

func (c *scriptedControl) probedPaths() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var paths []string
	for _, r := range c.requests {
		if r.Service == control.ServiceNginx && r.Operation == control.OperationHealth {
			paths = append(paths, r.CandidatePath)
		}
	}
	return paths
}

type harness struct {
	t       *testing.T
	db      *sql.DB
	jobs    *store.Store
	revs    *revisions.Store
	control *scriptedControl
	root    string
	logs    []string
	opts    poller.Options
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	db, _ := testsupport.NewDB(t)
	h := &harness{t: t, db: db, jobs: store.New(db), revs: revisions.New(db), control: &scriptedControl{}, root: t.TempDir()}
	orch := &apply.Orchestrator{Jobs: h.jobs, Revisions: h.revs, Control: h.control, CandidateRoot: h.root,
		Log: func(m string) { h.logs = append(h.logs, m) }}
	h.opts = poller.Options{
		Jobs: h.jobs, Revisions: h.revs, Applier: orch,
		Render: render.ForJob(render.Options{CandidateRoot: h.root}),
	}
	return h
}

func (h *harness) revision(n int, applied bool) string {
	h.t.Helper()
	id := testsupport.UUID(n)
	var appliedAt any
	if applied {
		appliedAt = testsupport.Stamp(h.t, base)
	}
	if _, err := h.db.Exec(`insert into config_revisions (id,revision_number,checksum,snapshot,created_at,applied_at)
		values (?,?,?,?,?,?)`, id, n, fmt.Sprintf("checksum-%d", n), snapshotJSON, testsupport.Stamp(h.t, base), appliedAt); err != nil {
		h.t.Fatal(err)
	}
	return id
}

func (h *harness) enqueue(revisionID string, target store.Target, correlation string) store.Job {
	h.t.Helper()
	job, err := h.jobs.Enqueue(context.Background(), store.EnqueueInput{RevisionID: revisionID, Target: target, CorrelationID: correlation})
	if err != nil {
		h.t.Fatal(err)
	}
	return job
}

// appliedJob seeds an applied job; finished and created are optional offsets.
func (h *harness) appliedJob(revisionID, correlation string, created time.Time, finished *time.Time) {
	h.t.Helper()
	job := h.enqueue(revisionID, store.TargetCombined, correlation)
	if _, err := h.db.Exec("update apply_jobs set status='applied', created_at=?, finished_at=? where id=?",
		testsupport.Stamp(h.t, created), nilOr(h.t, finished), job.ID); err != nil {
		h.t.Fatal(err)
	}
}

func nilOr(t *testing.T, at *time.Time) any {
	if at == nil {
		return nil
	}
	return testsupport.Stamp(t, *at)
}

func (h *harness) allJobs() []store.Job {
	h.t.Helper()
	list, err := h.jobs.List(context.Background())
	if err != nil {
		h.t.Fatal(err)
	}
	return list
}

func at(minutes int) *time.Time {
	v := base.Add(time.Duration(minutes) * time.Minute)
	return &v
}

func TestPollOnceClaimsAPersistentJobAndAppliesBothCandidates(t *testing.T) {
	h := newHarness(t)
	rev := h.revision(1, false)
	h.enqueue(rev, store.TargetCombined, "poll-1")
	result, err := poller.PollOnce(context.Background(), h.opts)
	if err != nil || result == nil || result.Status != jobs.Applied {
		t.Fatalf("result = %+v, %v", result, err)
	}
	if got, _ := h.revs.Get(context.Background(), rev); got.AppliedAt == nil {
		t.Fatal("revision not marked applied")
	}
	want := []string{
		"coredns:stage", "coredns:validate", "nginx:stage", "nginx:validate", "coredns:promote", "nginx:promote",
		"coredns:reload", "coredns:health", "nginx:reload", "nginx:health",
	}
	if got := h.control.operations(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("operations = %v", got)
	}
	corefile, err := os.ReadFile(filepath.Join(h.root, rev, "coredns", "Corefile"))
	if err != nil || !strings.Contains(string(corefile), "forward . 1.1.1.1:53") {
		t.Fatalf("Corefile = %q, %v", corefile, err)
	}
	conf, err := os.ReadFile(filepath.Join(h.root, rev, "nginx", "nginx.conf"))
	if err != nil || !strings.Contains(string(conf), "events {}") {
		t.Fatalf("nginx.conf = %q, %v", conf, err)
	}
}

func TestPollOnceRepairsAnAppliedRevisionAfterTheQueueDrains(t *testing.T) {
	h := newHarness(t)
	rev := h.revision(1, true)
	h.control.nginxHealth = []string{"drift", "healthy"}
	result, err := poller.PollOnce(context.Background(), h.opts)
	if err != nil || result == nil || result.Status != jobs.Applied || result.Target != store.TargetNginx {
		t.Fatalf("result = %+v, %v", result, err)
	}
	want := []string{"nginx:health", "nginx:stage", "nginx:validate", "nginx:promote", "nginx:reload", "nginx:health"}
	if got := h.control.operations(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("operations = %v", got)
	}
	list := h.allJobs()
	if len(list) != 1 || list[0].CorrelationID != "reconcile-nginx:"+rev {
		t.Fatalf("jobs = %+v", list)
	}
	if conf, err := os.ReadFile(filepath.Join(h.root, rev, "nginx", "nginx.conf")); err != nil || !strings.Contains(string(conf), "events {}") {
		t.Fatalf("nginx.conf = %q, %v", conf, err)
	}
}

// Revision 43 was just applied by the newest job while revision 45 is an older
// applied revision with a higher number: the poller must probe (and so never
// revert to) 43.
func TestReconcileTargetsTheNewestAppliedJobNotTheHighestRevisionNumber(t *testing.T) {
	h := newHarness(t)
	just, older := h.revision(43, true), h.revision(45, true)
	h.appliedJob(older, "ordinary-45", base, at(0))
	h.appliedJob(just, "ordinary-43", base, at(5))
	h.control.driftPath = filepath.Join(h.root, older, "nginx")
	result, err := poller.PollOnce(context.Background(), h.opts)
	if err != nil || result != nil {
		t.Fatalf("result = %+v, %v", result, err)
	}
	if got := h.control.probedPaths(); len(got) != 1 || got[0] != filepath.Join(h.root, just, "nginx") {
		t.Fatalf("probed = %v", got)
	}
	for _, job := range h.allJobs() {
		if strings.HasPrefix(job.CorrelationID, "reconcile-nginx:") {
			t.Fatalf("unexpected reconcile job %+v", job)
		}
	}
}

func TestReconcileTargetsTheRevisionOfANewestAppliedReconcileJob(t *testing.T) {
	h := newHarness(t)
	repaired, other := h.revision(1, true), h.revision(2, true)
	h.appliedJob(other, "ordinary", base, at(0))
	h.appliedJob(repaired, "reconcile-nginx:"+repaired, base, at(5))
	if _, err := poller.PollOnce(context.Background(), h.opts); err != nil {
		t.Fatal(err)
	}
	if got := h.control.probedPaths(); len(got) != 1 || got[0] != filepath.Join(h.root, repaired, "nginx") {
		t.Fatalf("probed = %v", got)
	}
}

func TestReconcileFallsBackToTheLatestAppliedRevisionWithoutAppliedJobs(t *testing.T) {
	h := newHarness(t)
	h.revision(1, false)
	latest := h.revision(2, true)
	if _, err := poller.PollOnce(context.Background(), h.opts); err != nil {
		t.Fatal(err)
	}
	if got := h.control.probedPaths(); len(got) != 1 || got[0] != filepath.Join(h.root, latest, "nginx") {
		t.Fatalf("probed = %v", got)
	}
}

func TestReconcileOrdersAppliedJobsByFinishedAtThenCreatedAt(t *testing.T) {
	h := newHarness(t)
	newest, middle, oldest := h.revision(1, true), h.revision(2, true), h.revision(3, true)
	h.appliedJob(oldest, "a", base, at(0))
	h.appliedJob(newest, "b", base, at(9))
	h.appliedJob(middle, "c", *at(3), nil) // no finished_at: created_at is used
	if _, err := poller.PollOnce(context.Background(), h.opts); err != nil {
		t.Fatal(err)
	}
	if got := h.control.probedPaths(); len(got) != 1 || got[0] != filepath.Join(h.root, newest, "nginx") {
		t.Fatalf("probed = %v", got)
	}
}

func TestReconcileDoesNothingForAHealthyRevisionOrWithoutAnyApplied(t *testing.T) {
	h := newHarness(t)
	if result, err := poller.PollOnce(context.Background(), h.opts); err != nil || result != nil || len(h.control.operations()) != 0 {
		t.Fatalf("empty store: %+v, %v, %v", result, err, h.control.operations())
	}
	rev := h.revision(1, true)
	result, err := poller.PollOnce(context.Background(), h.opts)
	if err != nil || result != nil {
		t.Fatalf("result = %+v, %v", result, err)
	}
	if got := h.control.operations(); len(got) != 1 || got[0] != "nginx:health" {
		t.Fatalf("operations = %v", got)
	}
	if got := h.control.requests[0].CandidatePath; got != filepath.Join(h.root, rev, "nginx") {
		t.Fatalf("probe path = %s", got)
	}
	if len(h.allJobs()) != 0 {
		t.Fatal("a healthy probe must not enqueue")
	}
}

func TestOrdinaryWorkClaimedDuringTheProbeWinsOverRepair(t *testing.T) {
	h := newHarness(t)
	rev := h.revision(1, true)
	h.control.nginxHealth = []string{"drift"}
	var once sync.Once
	h.control.before = func(r control.Request) {
		if r.Service == control.ServiceNginx && r.Operation == control.OperationHealth {
			once.Do(func() {
				h.enqueue(h.revision(2, false), store.TargetCombined, "ordinary-1")
				if _, err := h.jobs.ClaimNext(context.Background(), ""); err != nil {
					t.Error(err)
				}
			})
		}
	}
	result, err := poller.PollOnce(context.Background(), h.opts)
	if err != nil || result != nil {
		t.Fatalf("result = %+v, %v", result, err)
	}
	list := h.allJobs()
	if len(list) != 1 || list[0].Status != jobs.Validating || list[0].CorrelationID != "ordinary-1" {
		t.Fatalf("jobs = %+v (revision %s)", list, rev)
	}
	if got := h.control.operations(); len(got) != 1 {
		t.Fatalf("operations = %v", got)
	}
}

func TestAnActiveJobSuppressesReconciliationEntirely(t *testing.T) {
	h := newHarness(t)
	rev := h.revision(1, true)
	h.enqueue(rev, store.TargetCombined, "busy")
	// Claim it directly so PollOnce sees only a fresh validating job.
	if _, err := h.jobs.ClaimNext(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	result, err := poller.PollOnce(context.Background(), h.opts)
	if err != nil || result != nil || len(h.control.operations()) != 0 {
		t.Fatalf("result = %+v, %v, ops %v", result, err, h.control.operations())
	}
}

func TestALaterDriftIsRepairedAgainAndAFailedRepairIsRetried(t *testing.T) {
	h := newHarness(t)
	h.revision(1, true)
	h.control.nginxHealth = []string{"drift", "healthy", "drift", "healthy"}
	first, err1 := poller.PollOnce(context.Background(), h.opts)
	second, err2 := poller.PollOnce(context.Background(), h.opts)
	if err1 != nil || err2 != nil || first.Status != jobs.Applied || second.Status != jobs.Applied {
		t.Fatalf("results = %+v %+v, %v %v", first, second, err1, err2)
	}
	if len(h.allJobs()) != 2 {
		t.Fatalf("jobs = %d", len(h.allJobs()))
	}

	retry := newHarness(t)
	retry.revision(1, true)
	retry.control.nginxHealth = []string{"drift", "drift", "healthy"}
	retry.control.failOnce = "stage"
	failed, _ := poller.PollOnce(context.Background(), retry.opts)
	repaired, _ := poller.PollOnce(context.Background(), retry.opts)
	if failed.Status != jobs.Failed || repaired.Status != jobs.Applied || len(retry.allJobs()) != 2 {
		t.Fatalf("failed = %+v repaired = %+v", failed, repaired)
	}
}

func TestPollOnceRecoversStaleJobsBeforeClaiming(t *testing.T) {
	h := newHarness(t)
	rev := h.revision(1, false)
	job := h.enqueue(rev, store.TargetCoreDNS, "stale")
	// A worker that died an hour ago left the job validating.
	if _, err := h.db.Exec("update apply_jobs set status='validating', claimed_at=? where id=?",
		testsupport.Stamp(t, time.Now().Add(-time.Hour)), job.ID); err != nil {
		t.Fatal(err)
	}
	h.opts.LeaseMs = 120_000
	result, err := poller.PollOnce(context.Background(), h.opts)
	if err != nil || result == nil || result.ID != job.ID || result.Status != jobs.Applied {
		t.Fatalf("result = %+v, %v", result, err)
	}
	// A fresh claim inside the lease is left alone.
	fresh := h.enqueue(h.revision(2, false), store.TargetCoreDNS, "fresh")
	if _, err := h.jobs.ClaimNext(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if result, err := poller.PollOnce(context.Background(), h.opts); err != nil || result != nil {
		t.Fatalf("fresh claim was disturbed: %+v, %v", result, err)
	}
	if got, _ := h.jobs.Get(context.Background(), fresh.ID); got.Status != jobs.Validating {
		t.Fatalf("status = %s", got.Status)
	}
}

// missingRevisions hides every revision to simulate a vanished row.
type missingRevisions struct{ poller.Revisions }

func (missingRevisions) Get(context.Context, string) (*revisions.Revision, error) { return nil, nil }

func TestPollOnceFailsAClaimedJobWhoseRevisionIsMissing(t *testing.T) {
	h := newHarness(t)
	rev := h.revision(1, false)
	job := h.enqueue(rev, store.TargetCombined, "orphan")
	h.opts.Revisions = missingRevisions{h.revs}
	result, err := poller.PollOnce(context.Background(), h.opts)
	if err != nil || result == nil || result.ID != job.ID || result.Status != jobs.Failed {
		t.Fatalf("result = %+v, %v", result, err)
	}
	if result.ErrorMessage == nil || *result.ErrorMessage != "Revision not found: "+rev || result.FinishedAt == nil {
		t.Fatalf("result = %+v", result)
	}
	if len(h.control.operations()) != 0 {
		t.Fatal("nothing should reach control")
	}
}

func TestPollOnceReportsUnsupportedTargetAsAFailedJob(t *testing.T) {
	h := newHarness(t)
	rev := h.revision(1, false)
	h.enqueue(rev, store.TargetCertificate, "cert")
	result, err := poller.PollOnce(context.Background(), h.opts)
	if err != nil || result.Status != jobs.Failed || *result.ErrorMessage != "Unsupported apply target: certificate" {
		t.Fatalf("result = %+v, %v", result, err)
	}
}

type failingJobs struct {
	poller.Jobs
	err error
}

func (f failingJobs) RecoverStale(context.Context, int64) (int64, error) { return 0, f.err }

func TestPollOncePropagatesStoreErrors(t *testing.T) {
	h := newHarness(t)
	boom := errors.New("database is locked")
	h.opts.Jobs = failingJobs{h.jobs, boom}
	if _, err := poller.PollOnce(context.Background(), h.opts); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
}

func TestRunDrainsTheQueueHeartbeatsEveryTurnAndWakesOnNotify(t *testing.T) {
	h := newHarness(t)
	wake := wakeup.NewWakeup()
	var beats atomic.Int32
	h.opts.Interval = time.Hour
	h.opts.Wakeup = wake
	h.opts.Heartbeat = func() { beats.Add(1) }
	for n := 1; n <= 3; n++ {
		h.enqueue(h.revision(n, false), store.TargetCoreDNS, "job")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- poller.Run(ctx, h.opts) }()

	waitFor(t, func() bool { return countStatus(h, jobs.Applied) == 3 })
	// The first turn drained all three jobs without another wake.
	if beats.Load() != 1 {
		t.Fatalf("heartbeats after the first turn = %d", beats.Load())
	}
	h.enqueue(h.revision(4, false), store.TargetCoreDNS, "late")
	wake.Notify()
	waitFor(t, func() bool { return countStatus(h, jobs.Applied) == 4 })
	if beats.Load() != 2 {
		t.Fatalf("heartbeats = %d, want one per turn", beats.Load())
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop on cancellation while waiting")
	}
}

func TestRunLogsATurnErrorAndKeepsGoing(t *testing.T) {
	h := newHarness(t)
	boom := errors.New("database is locked")
	var calls atomic.Int32
	h.opts.Jobs = flakyJobs{Jobs: h.jobs, calls: &calls, err: boom}
	h.opts.Interval = 10 * time.Millisecond
	var mu sync.Mutex
	var errs []string
	h.opts.OnError = func(err error) {
		mu.Lock()
		errs = append(errs, err.Error())
		mu.Unlock()
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- poller.Run(ctx, h.opts) }()
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(errs) >= 2 })
	cancel()
	<-done
	if errs[0] != "database is locked" {
		t.Fatalf("errs = %v", errs)
	}
}

type flakyJobs struct {
	poller.Jobs
	calls *atomic.Int32
	err   error
}

func (f flakyJobs) RecoverStale(context.Context, int64) (int64, error) {
	f.calls.Add(1)
	return 0, f.err
}

func TestRunLetsAnInFlightApplyFinishOnShutdown(t *testing.T) {
	h := newHarness(t)
	rev := h.revision(1, false)
	h.enqueue(rev, store.TargetCoreDNS, "slow")
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	h.control.before = func(r control.Request) {
		if r.Operation == control.OperationValidate {
			once.Do(func() { close(started); <-release })
		}
	}
	h.opts.Interval = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- poller.Run(ctx, h.opts) }()
	<-started
	cancel()
	select {
	case <-done:
		t.Fatal("Run returned while an apply was in flight")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after the apply finished")
	}
	if countStatus(h, jobs.Applied) != 1 {
		t.Fatalf("jobs = %+v", h.allJobs())
	}
}

func TestRunRequiresAPositiveInterval(t *testing.T) {
	h := newHarness(t)
	if err := poller.Run(context.Background(), h.opts); err == nil {
		t.Fatal("want an error for a zero interval")
	}
}

func TestRunWithoutWakeupSleepsTheIntervalAndStopsOnCancel(t *testing.T) {
	h := newHarness(t)
	h.opts.Interval = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- poller.Run(ctx, h.opts) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
}

func countStatus(h *harness, status jobs.Status) int {
	n := 0
	for _, job := range h.allJobs() {
		if job.Status == status {
			n++
		}
	}
	return n
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
