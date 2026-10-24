package apply_test

import (
	"context"
	"path"
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/worker/jobs"
	"github.com/zerkc/ProxyCore/apps/api/internal/worker/revisions"
	"github.com/zerkc/ProxyCore/apps/api/internal/worker/store"
	"github.com/zerkc/ProxyCore/apps/api/internal/worker/testsupport"
)

func appliedRevision(t *testing.T, e *env, n int) revisions.Revision {
	t.Helper()
	testsupport.InsertRevision(t, e.db, testsupport.UUID(n), n, nil)
	marked, err := e.revs.MarkApplied(context.Background(), testsupport.UUID(n), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return marked
}

func TestReconcileProbesTheRevisionsNginxCandidateAndIgnoresHealthy(t *testing.T) {
	e := newEnv(t)
	revision := appliedRevision(t, e, 1)
	result, err := e.orch.ReconcileApplied(context.Background(), revision, fixed())
	if err != nil || result != nil {
		t.Fatalf("result = %+v, %v", result, err)
	}
	if got := e.control.operations(); len(got) != 1 || got[0] != "nginx:health" {
		t.Fatalf("operations = %v", got)
	}
	request := e.control.requests[0]
	if request.CandidatePath != path.Join(e.root, revision.ID, "nginx") || request.RevisionID != revision.ID || request.Checksum != revision.Checksum {
		t.Fatalf("probe = %+v", request)
	}
	if list, _ := e.jobs.List(context.Background()); len(list) != 0 {
		t.Fatalf("jobs = %+v", list)
	}
}

func TestReconcileRepairsDriftWithATerminalNginxJob(t *testing.T) {
	e := newEnv(t)
	revision := appliedRevision(t, e, 1)
	e.control.output = map[string]string{"nginx:health": `{"status":"drift"}`}
	// The repair's own post-reload health must be healthy.
	calls := 0
	e.orch.Control = &sequenceControl{fakeControl: e.control, onHealth: func() string {
		calls++
		if calls == 1 {
			return `{"status":"drift"}`
		}
		return `{"status":"healthy"}`
	}}
	result, err := e.orch.ReconcileApplied(context.Background(), revision, fixed(e.candidate("nginx", store.Job{RevisionID: revision.ID})))
	if err != nil || result == nil {
		t.Fatalf("result = %+v, %v", result, err)
	}
	if result.Status != jobs.Applied || result.Target != store.TargetNginx || result.CorrelationID != "reconcile-nginx:"+revision.ID {
		t.Fatalf("result = %+v", result)
	}
	if result.ClaimedAt == nil || result.StartedAt == nil {
		t.Fatalf("repair job must be created already claimed: %+v", result)
	}
	if list, _ := e.jobs.List(context.Background()); len(list) != 1 {
		t.Fatalf("jobs = %+v", list)
	}
}

func TestReconcileDoesNotEnqueueWhileAnotherJobIsActive(t *testing.T) {
	e := newEnv(t)
	revision := appliedRevision(t, e, 1)
	e.enqueue(store.TargetCombined, 2) // a queued ordinary job wins over the repair
	e.control.output = map[string]string{"nginx:health": `{"status":"drift"}`}
	result, err := e.orch.ReconcileApplied(context.Background(), revision, fixed())
	if err != nil || result != nil {
		t.Fatalf("result = %+v, %v", result, err)
	}
	if got := e.control.operations(); len(got) != 1 {
		t.Fatalf("operations = %v", got)
	}
	if list, _ := e.jobs.List(context.Background()); len(list) != 1 || list[0].Status != jobs.Queued {
		t.Fatalf("jobs = %+v", list)
	}
}

func TestReconcileIgnoresARevisionThatWasNeverApplied(t *testing.T) {
	e := newEnv(t)
	testsupport.InsertRevision(t, e.db, testsupport.UUID(1), 1, nil)
	revision, _ := e.revs.Get(context.Background(), testsupport.UUID(1))
	result, err := e.orch.ReconcileApplied(context.Background(), *revision, fixed())
	if err != nil || result != nil || len(e.control.operations()) != 0 {
		t.Fatalf("result = %+v, %v, ops %v", result, err, e.control.operations())
	}
}

func TestReconcileDoesNotTreatAFailedProbeAsDrift(t *testing.T) {
	e := newEnv(t)
	revision := appliedRevision(t, e, 1)
	e.control.fail = map[string]string{"nginx:health": "down"}
	result, err := e.orch.ReconcileApplied(context.Background(), revision, fixed())
	if err != nil || result != nil {
		t.Fatalf("result = %+v, %v", result, err)
	}
}
