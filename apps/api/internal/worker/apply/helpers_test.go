package apply_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/worker/apply"
	"github.com/zerkc/ProxyCore/apps/api/internal/worker/control"
	"github.com/zerkc/ProxyCore/apps/api/internal/worker/revisions"
	"github.com/zerkc/ProxyCore/apps/api/internal/worker/store"
	"github.com/zerkc/ProxyCore/apps/api/internal/worker/testsupport"
)

// fakeControl records every request as "service:operation" and answers ok
// unless a failure is configured for that key.
type fakeControl struct {
	mu       sync.Mutex
	ops      []string
	requests []control.Request
	// fail maps "service:operation" to an error text for a not-ok response.
	fail map[string]string
	// throw maps "service:operation" to a transport error.
	throw map[string]error
	// output maps "service:operation" to a raw JSON output.
	output map[string]string
}

func (f *fakeControl) Execute(_ context.Context, request control.Request) (control.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := string(request.Service) + ":" + string(request.Operation)
	f.ops = append(f.ops, key)
	f.requests = append(f.requests, request)
	if err := f.throw[key]; err != nil {
		return control.Response{}, err
	}
	response := control.Response{RequestID: request.RequestID, Operation: request.Operation, OK: true,
		Output: json.RawMessage(`{"operation":"` + string(request.Operation) + `"}`)}
	if text, ok := f.fail[key]; ok {
		response.OK, response.Output, response.Error = false, nil, text
	}
	if raw, ok := f.output[key]; ok {
		response.Output = json.RawMessage(raw)
	}
	return response, nil
}

func (f *fakeControl) operations() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ops...)
}

type env struct {
	t       *testing.T
	db      *sql.DB
	jobs    *store.Store
	revs    *revisions.Store
	control *fakeControl
	orch    *apply.Orchestrator
	root    string
	logs    []string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	db, _ := testsupport.NewDB(t)
	e := &env{t: t, db: db, jobs: store.New(db), revs: revisions.New(db), control: &fakeControl{}, root: t.TempDir()}
	e.orch = &apply.Orchestrator{
		Jobs: e.jobs, Revisions: e.revs, Control: e.control, CandidateRoot: e.root,
		Log: func(message string) { e.logs = append(e.logs, message) },
	}
	return e
}

// enqueue seeds revision number n and a queued job for it.
func (e *env) enqueue(target store.Target, n int) store.Job {
	e.t.Helper()
	testsupport.InsertRevision(e.t, e.db, testsupport.UUID(n), n, nil)
	job, err := e.jobs.Enqueue(context.Background(), store.EnqueueInput{
		RevisionID: testsupport.UUID(n), Target: target, CorrelationID: "correlation-1",
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return job
}

func (e *env) job(id string) store.Job {
	e.t.Helper()
	job, err := e.jobs.Get(context.Background(), id)
	if err != nil || job == nil {
		e.t.Fatalf("Get(%s) = %v, %v", id, job, err)
	}
	return *job
}

func (e *env) revision(id string) *revisions.Revision {
	e.t.Helper()
	revision, err := e.revs.Get(context.Background(), id)
	if err != nil || revision == nil {
		e.t.Fatalf("revision %s = %v, %v", id, revision, err)
	}
	return revision
}

// candidate builds a candidate under the env's root with no files.
func (e *env) candidate(service control.Service, job store.Job) apply.Candidate {
	return apply.Candidate{
		Service: service, Checksum: "abc",
		CandidatePath: e.root + "/" + job.RevisionID + "/" + string(service),
	}
}

func fixed(candidates ...apply.Candidate) apply.Renderer {
	return func(context.Context, json.RawMessage, store.Job) ([]apply.Candidate, error) {
		return candidates, nil
	}
}

func empty() json.RawMessage { return json.RawMessage(`{"zones":[]}`) }

func equalJSON(t *testing.T, got, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal([]byte(got), &g); err != nil {
		t.Fatalf("got is not JSON: %q", got)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatal(err)
	}
	gb, _ := json.Marshal(g)
	wb, _ := json.Marshal(w)
	if string(gb) != string(wb) {
		t.Fatalf("JSON mismatch\n got: %s\nwant: %s", got, want)
	}
}

func contains(t *testing.T, haystack string, needles ...string) {
	t.Helper()
	for _, needle := range needles {
		if !strings.Contains(haystack, needle) {
			t.Fatalf("%q does not contain %q", haystack, needle)
		}
	}
}

var fixedNow = time.Date(2026, 4, 1, 10, 0, 0, 0, time.UTC)

// sequenceControl overrides the nginx health output per call.
type sequenceControl struct {
	*fakeControl
	onHealth func() string
}

func (s *sequenceControl) Execute(ctx context.Context, request control.Request) (control.Response, error) {
	response, err := s.fakeControl.Execute(ctx, request)
	if err == nil && request.Service == control.ServiceNginx && request.Operation == control.OperationHealth {
		response.Output = json.RawMessage(s.onHealth())
	}
	return response, err
}
