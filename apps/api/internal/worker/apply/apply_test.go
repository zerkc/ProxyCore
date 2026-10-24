package apply_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/zerkc/ProxyCore/apps/api/internal/worker/apply"
	"github.com/zerkc/ProxyCore/apps/api/internal/worker/control"
	"github.com/zerkc/ProxyCore/apps/api/internal/worker/jobs"
	"github.com/zerkc/ProxyCore/apps/api/internal/worker/store"
)

func TestApplyHappyPathMarksTheRevisionApplied(t *testing.T) {
	e := newEnv(t)
	job := e.enqueue(store.TargetCoreDNS, 1)
	result, err := e.orch.Apply(context.Background(), job.ID, empty(), fixed(e.candidate("coredns", job)))
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != jobs.Applied || result.FinishedAt == nil || result.ErrorMessage != nil {
		t.Fatalf("result = %+v", result)
	}
	if got := e.revision(job.RevisionID); got.AppliedAt == nil {
		t.Fatal("revision was not marked applied")
	}
	want := []string{"coredns:stage", "coredns:validate", "coredns:promote", "coredns:reload", "coredns:health"}
	if got := e.control.operations(); !reflect.DeepEqual(got, want) {
		t.Fatalf("operations = %v", got)
	}
	stored := e.job(job.ID)
	equalJSON(t, string(stored.ValidationOutput), `{"coredns":{"operation":"validate"}}`)
	equalJSON(t, string(stored.ApplyOutput), `{"coredns":{"operation":"promote"}}`)
	equalJSON(t, string(stored.HealthOutput), `{"coredns":{"operation":"health"}}`)
	for _, request := range e.control.requests {
		if request.RevisionID != job.RevisionID || request.Checksum != "abc" || request.RequestID == "" {
			t.Fatalf("request = %+v", request)
		}
	}
	if len(e.logs) != 0 {
		t.Fatalf("logs = %v", e.logs)
	}
}

func TestApplyAcceptsAnAlreadyValidatingJobAndRejectsOthers(t *testing.T) {
	e := newEnv(t)
	job := e.enqueue(store.TargetCoreDNS, 1)
	claimed, err := e.jobs.ClaimNext(context.Background(), "")
	if err != nil || claimed == nil || claimed.ID != job.ID {
		t.Fatalf("claim = %+v, %v", claimed, err)
	}
	result, err := e.orch.Apply(context.Background(), job.ID, empty(), fixed(e.candidate("coredns", job)))
	if err != nil || result.Status != jobs.Applied {
		t.Fatalf("validating job: %+v, %v", result, err)
	}
	// An applied job is terminal and cannot be applied again.
	_, err = e.orch.Apply(context.Background(), job.ID, empty(), fixed(e.candidate("coredns", job)))
	if err == nil || err.Error() != "Job "+job.ID+" is not queued" {
		t.Fatalf("err = %v", err)
	}
	contains(t, e.logs[len(e.logs)-1], "step=claim service=none")
}

func TestApplyRendersTheDesiredConfigurationOfAnEnvelope(t *testing.T) {
	e := newEnv(t)
	job := e.enqueue(store.TargetCoreDNS, 1)
	var rendered []byte
	renderer := func(_ context.Context, snapshot json.RawMessage, _ store.Job) ([]apply.Candidate, error) {
		rendered = snapshot
		return []apply.Candidate{e.candidate("coredns", job)}, nil
	}
	envelope := `{"replicated":{"configuration":{"settings":{"defaultPool":{"id":"pool-1"}},"zones":[]}}}`
	result, err := e.orch.Apply(context.Background(), job.ID, json.RawMessage(envelope), func(ctx context.Context, s json.RawMessage, j store.Job) ([]apply.Candidate, error) {
		return renderer(ctx, s, j)
	})
	if err != nil || result.Status != jobs.Applied {
		t.Fatalf("result = %+v, %v", result, err)
	}
	equalJSON(t, string(rendered), `{"settings":{"defaultPool":{"id":"pool-1"}},"zones":[]}`)
}

func TestApplyValidationFailureFailsTheJobAndLeavesTheRevisionUnapplied(t *testing.T) {
	e := newEnv(t)
	e.control.fail = map[string]string{"coredns:validate": "simulated failure"}
	job := e.enqueue(store.TargetCoreDNS, 1)
	result, err := e.orch.Apply(context.Background(), job.ID, empty(), fixed(e.candidate("coredns", job)))
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != jobs.Failed || result.ErrorMessage == nil || *result.ErrorMessage != "simulated failure" {
		t.Fatalf("result = %+v", result)
	}
	if e.revision(job.RevisionID).AppliedAt != nil {
		t.Fatal("revision must stay unapplied")
	}
	if got, want := e.control.operations(), []string{"coredns:stage", "coredns:validate"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("operations = %v", got)
	}
	// A failed validation has no output of its own; the key is omitted.
	equalJSON(t, string(result.ValidationOutput), `{}`)
}

func TestApplyStageFailureStoresTheStagingOutput(t *testing.T) {
	e := newEnv(t)
	e.control.fail = map[string]string{"nginx:stage": ""}
	job := e.enqueue(store.TargetNginx, 1)
	result, err := e.orch.Apply(context.Background(), job.ID, empty(), fixed(e.candidate("nginx", job)))
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != jobs.Failed || *result.ErrorMessage != "Candidate staging failed" {
		t.Fatalf("result = %+v", result)
	}
	equalJSON(t, string(result.ValidationOutput), `{"service":"nginx"}`)
}

func TestApplyLogsSafeIdentifiersWithoutLeakingControlText(t *testing.T) {
	e := newEnv(t)
	e.control.fail = map[string]string{"coredns:validate": "simulated failure"}
	job := e.enqueue(store.TargetCoreDNS, 1)
	candidate := e.candidate("coredns", job)
	candidate.CandidatePath = e.root + "/private/coredns"
	if _, err := e.orch.Apply(context.Background(), job.ID, empty(), fixed(candidate)); err != nil {
		t.Fatal(err)
	}
	if len(e.logs) != 1 {
		t.Fatalf("logs = %v", e.logs)
	}
	line := e.logs[0]
	contains(t, line, "worker apply failed job="+job.ID, "revision="+job.RevisionID, "correlation=correlation-1", "step=validate service=coredns")
	rest := strings.NewReplacer(job.ID, "", job.RevisionID, "", "correlation-1", "").Replace(line)
	for _, leak := range []string{"simulated failure", "private", "abc"} {
		if strings.Contains(rest, leak) {
			t.Fatalf("log leaks %q: %s", leak, line)
		}
	}
}

func TestApplyUnknownJobLogsTheLookupStep(t *testing.T) {
	e := newEnv(t)
	_, err := e.orch.Apply(context.Background(), "missing-job", empty(), func(context.Context, json.RawMessage, store.Job) ([]apply.Candidate, error) {
		t.Fatal("must not render")
		return nil, nil
	})
	if err == nil || !strings.Contains(err.Error(), "Job not found") {
		t.Fatalf("err = %v", err)
	}
	if len(e.logs) != 1 || e.logs[0] != "worker apply failed job=missing-job revision=unavailable step=lookup service=none" {
		t.Fatalf("logs = %v", e.logs)
	}
	// An id that is not identifier-shaped never reaches the log.
	e.logs = nil
	_, _ = e.orch.Apply(context.Background(), "../secret path", empty(), fixed())
	contains(t, e.logs[0], "job=[redacted]")
}

func TestApplyRollsBackAfterAHealthFailure(t *testing.T) {
	e := newEnv(t)
	e.control.fail = map[string]string{"coredns:health": ""}
	job := e.enqueue(store.TargetCoreDNS, 1)
	result, err := e.orch.Apply(context.Background(), job.ID, empty(), fixed(e.candidate("coredns", job)))
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != jobs.RolledBack || *result.ErrorMessage != "Post-reload health failed; rolled back" || result.FinishedAt == nil {
		t.Fatalf("result = %+v", result)
	}
	want := []string{"coredns:stage", "coredns:validate", "coredns:promote", "coredns:reload", "coredns:health", "coredns:rollback"}
	if got := e.control.operations(); !reflect.DeepEqual(got, want) {
		t.Fatalf("operations = %v", got)
	}
	if e.revision(job.RevisionID).AppliedAt != nil {
		t.Fatal("revision must stay unapplied")
	}
	equalJSON(t, string(result.ValidationOutput), `{"coredns":{"operation":"validate"}}`)
}

func TestApplyTreatsNginxDriftAsAHealthFailure(t *testing.T) {
	e := newEnv(t)
	e.control.output = map[string]string{"nginx:health": `{"status":"drift"}`}
	job := e.enqueue(store.TargetNginx, 1)
	result, err := e.orch.Apply(context.Background(), job.ID, empty(), fixed(e.candidate("nginx", job)))
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != jobs.RolledBack || *result.ErrorMessage != "Post-reload health failed; rolled back" {
		t.Fatalf("result = %+v", result)
	}
	equalJSON(t, string(result.HealthOutput), `{"nginx":{"status":"drift"}}`)
}

func TestApplyRollsBackEveryPromotedCandidateInReverseOrder(t *testing.T) {
	e := newEnv(t)
	// The second candidate's health fails after both were promoted.
	e.control.fail = map[string]string{"nginx:health": "nginx unhealthy"}
	job := e.enqueue(store.TargetCombined, 1)
	result, err := e.orch.Apply(context.Background(), job.ID, empty(),
		fixed(e.candidate("coredns", job), e.candidate("nginx", job)))
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != jobs.RolledBack || *result.ErrorMessage != "nginx unhealthy" {
		t.Fatalf("result = %+v", result)
	}
	want := []string{
		"coredns:stage", "coredns:validate", "nginx:stage", "nginx:validate",
		"coredns:promote", "nginx:promote", "coredns:reload", "coredns:health",
		"nginx:reload", "nginx:health", "nginx:rollback", "coredns:rollback",
	}
	if got := e.control.operations(); !reflect.DeepEqual(got, want) {
		t.Fatalf("operations = %v", got)
	}
	equalJSON(t, string(result.HealthOutput), `{"coredns":{"operation":"health"}}`)
}

func TestApplyAttemptsEveryRollbackAndFailsWhenOneFails(t *testing.T) {
	e := newEnv(t)
	e.control.fail = map[string]string{"coredns:health": "private credential", "nginx:rollback": "private credential"}
	job := e.enqueue(store.TargetCombined, 1)
	result, err := e.orch.Apply(context.Background(), job.ID, empty(),
		fixed(e.candidate("coredns", job), e.candidate("nginx", job)))
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != jobs.Failed || *result.ErrorMessage != "private credential; rollback failed" {
		t.Fatalf("result = %+v", result)
	}
	ops := e.control.operations()
	if got := ops[len(ops)-2:]; !reflect.DeepEqual(got, []string{"nginx:rollback", "coredns:rollback"}) {
		t.Fatalf("rollbacks = %v", got)
	}
	if len(e.logs) != 2 || !strings.HasPrefix(e.logs[1], "worker rollback failed job="+job.ID+" revision="+job.RevisionID+" correlation=correlation-1 service=nginx") {
		t.Fatalf("logs = %v", e.logs)
	}
	rest := strings.NewReplacer(job.ID, "", job.RevisionID, "", "correlation-1", "").Replace(strings.Join(e.logs, "\n"))
	for _, leak := range []string{"credential", "candidate", "abc"} {
		if strings.Contains(rest, leak) {
			t.Fatalf("logs leak %q: %v", leak, e.logs)
		}
	}
}

func TestApplyRollsBackWhenPromotionOfALaterCandidateFails(t *testing.T) {
	e := newEnv(t)
	e.control.fail = map[string]string{"nginx:promote": "disk full"}
	job := e.enqueue(store.TargetCombined, 1)
	result, err := e.orch.Apply(context.Background(), job.ID, empty(),
		fixed(e.candidate("coredns", job), e.candidate("nginx", job)))
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != jobs.RolledBack || *result.ErrorMessage != "disk full" {
		t.Fatalf("result = %+v", result)
	}
	ops := e.control.operations()
	// Only the candidate that was actually promoted is rolled back.
	if got := ops[len(ops)-1]; got != "coredns:rollback" || strings.Count(strings.Join(ops, ","), "rollback") != 1 {
		t.Fatalf("operations = %v", ops)
	}
}

func TestApplyFailsWithoutRollbackWhenNothingWasPromoted(t *testing.T) {
	e := newEnv(t)
	e.control.throw = map[string]error{"coredns:validate": errors.New("Control request timed out")}
	job := e.enqueue(store.TargetCoreDNS, 1)
	result, err := e.orch.Apply(context.Background(), job.ID, empty(), fixed(e.candidate("coredns", job)))
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != jobs.Failed || *result.ErrorMessage != "Control request timed out" {
		t.Fatalf("result = %+v", result)
	}
	for _, op := range e.control.operations() {
		if strings.HasSuffix(op, "rollback") {
			t.Fatalf("unexpected rollback in %v", e.control.operations())
		}
	}
}

func TestApplyControlTimeoutAfterPromotionRollsBack(t *testing.T) {
	e := newEnv(t)
	e.control.throw = map[string]error{"coredns:reload": errors.New("Control request timed out")}
	job := e.enqueue(store.TargetCoreDNS, 1)
	result, err := e.orch.Apply(context.Background(), job.ID, empty(), fixed(e.candidate("coredns", job)))
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != jobs.RolledBack || *result.ErrorMessage != "Control request timed out" {
		t.Fatalf("result = %+v", result)
	}
	if ops := e.control.operations(); ops[len(ops)-1] != "coredns:rollback" {
		t.Fatalf("operations = %v", ops)
	}
}

func TestApplyStoresScrubbedErrorMessages(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"key value pair", "login failed: password=hunter2 for operator", "login failed: password=[REDACTED] for operator"},
		{"colon pair", "token: abc123 rejected", "token: [REDACTED] rejected"},
		{"json document", `{"detail":"x","apiToken":"abc123"}`, `{"apiToken":"[REDACTED]","detail":"x"}`},
		{"plain message", "nginx: configuration test failed", "nginx: configuration test failed"},
		{"words without a value", "Basic Auth secret not found: id-1", "Basic Auth secret not found: id-1"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			e.control.fail = map[string]string{"coredns:validate": tt.in}
			job := e.enqueue(store.TargetCoreDNS, 1)
			result, err := e.orch.Apply(context.Background(), job.ID, empty(), fixed(e.candidate("coredns", job)))
			if err != nil || result.ErrorMessage == nil {
				t.Fatalf("result = %+v, %v", result, err)
			}
			if *result.ErrorMessage != tt.want {
				t.Fatalf("stored = %q, want %q", *result.ErrorMessage, tt.want)
			}
			if got := *e.job(job.ID).ErrorMessage; got != tt.want {
				t.Fatalf("persisted = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestApplyRenderFailureFailsTheJob(t *testing.T) {
	e := newEnv(t)
	job := e.enqueue(store.TargetCoreDNS, 1)
	result, err := e.orch.Apply(context.Background(), job.ID, empty(), func(context.Context, json.RawMessage, store.Job) ([]apply.Candidate, error) {
		return nil, errors.New("Unsupported apply target: certificate")
	})
	if err != nil || result.Status != jobs.Failed || *result.ErrorMessage != "Unsupported apply target: certificate" {
		t.Fatalf("result = %+v, %v", result, err)
	}
	contains(t, e.logs[0], "step=render service=none")
	if len(e.control.operations()) != 0 {
		t.Fatalf("operations = %v", e.control.operations())
	}
}

func TestApplyRejectsAnEmptyRender(t *testing.T) {
	e := newEnv(t)
	job := e.enqueue(store.TargetCoreDNS, 1)
	result, err := e.orch.Apply(context.Background(), job.ID, empty(), fixed())
	if err != nil || result.Status != jobs.Failed || *result.ErrorMessage != "No candidates were rendered" {
		t.Fatalf("result = %+v, %v", result, err)
	}
}

func TestApplyReportsACancelledContextAsAFailedControlCall(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	e.control.throw = map[string]error{}
	job := e.enqueue(store.TargetCoreDNS, 1)
	cancelling := func(ctx context.Context, _ json.RawMessage, _ store.Job) ([]apply.Candidate, error) {
		cancel()
		return nil, ctx.Err()
	}
	if _, err := e.orch.Apply(ctx, job.ID, empty(), cancelling); err == nil {
		t.Fatal("a cancelled context must surface a store error instead of silently succeeding")
	}
	// The job stays claimed; lease recovery requeues it.
	if got := e.job(job.ID).Status; got != jobs.Validating {
		t.Fatalf("status = %s", got)
	}
}

func TestSafeLogIDRedactsNonIdentifiers(t *testing.T) {
	e := newEnv(t)
	for id, want := range map[string]string{
		"abc_DEF-123":           "abc_DEF-123",
		"":                      "[redacted]",
		"has space":             "[redacted]",
		"a/b":                   "[redacted]",
		strings.Repeat("a", 81): "[redacted]",
		strings.Repeat("a", 80): strings.Repeat("a", 80),
		"line\nbreak":           "[redacted]",
		"unicode-é":             "[redacted]",
	} {
		e.logs = nil
		_, _ = e.orch.Apply(context.Background(), id, empty(), fixed())
		contains(t, e.logs[0], "job="+want+" revision=unavailable")
	}
}

var _ = control.OperationStage
