// Package apply ports the worker's apply orchestrator from
// apps/worker/src/apply.ts: it drives one job through render, stage, validate,
// promote, reload and health against the control socket, rolls every promoted
// candidate back on a post-promotion failure, and records the outcome on the
// job. Stores, control, clock and filesystem are interfaces so it is testable
// with fakes.
package apply

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/worker/control"
	"github.com/zerkc/ProxyCore/apps/api/internal/worker/jobs"
	"github.com/zerkc/ProxyCore/apps/api/internal/worker/revisions"
	"github.com/zerkc/ProxyCore/apps/api/internal/worker/store"
)

// ReconciliationCorrelationPrefix marks jobs the worker enqueues to repair nginx.
const ReconciliationCorrelationPrefix = "reconcile-nginx:"

// Jobs is the job store surface the orchestrator needs.
type Jobs interface {
	Get(ctx context.Context, id string) (*store.Job, error)
	Update(ctx context.Context, id string, patch store.JobPatch) (store.Job, error)
	EnqueueIfNotActive(ctx context.Context, input store.EnqueueInput) (*store.Job, error)
}

// Revisions is the revision store surface the orchestrator needs.
type Revisions interface {
	MarkApplied(ctx context.Context, id string, appliedAt time.Time) (revisions.Revision, error)
}

// Control executes one control request; *control.Client implements it.
type Control interface {
	Execute(ctx context.Context, request control.Request) (control.Response, error)
}

// Renderer renders the candidates for a job from its (already desired) snapshot.
type Renderer func(ctx context.Context, snapshot json.RawMessage, job store.Job) ([]Candidate, error)

// Orchestrator applies jobs. Jobs, Revisions and Control are required; the rest
// default to production behavior.
type Orchestrator struct {
	Jobs          Jobs
	Revisions     Revisions
	Control       Control
	CandidateRoot string
	FS            FS
	Now           func() time.Time
	// Log receives one line per failure, without the timestamp prefix.
	Log func(message string)
}

// Apply runs one queued or validating job and returns its final record. A
// failed or rolled-back apply is a normal result; an error means a store
// failure or an unknown job.
func (o *Orchestrator) Apply(ctx context.Context, jobID string, snapshot json.RawMessage, render Renderer) (store.Job, error) {
	found, err := o.Jobs.Get(ctx, jobID)
	if err == nil && found == nil {
		err = fmt.Errorf("Job not found: %s", jobID)
	}
	if err != nil {
		o.logf("worker apply failed job=%s revision=unavailable step=lookup service=none", safeLogID(jobID))
		return store.Job{}, err
	}
	job := *found
	switch job.Status {
	case jobs.Queued:
		job, err = o.setStatus(ctx, job, jobs.Validating, store.JobPatch{})
	case jobs.Validating:
	default:
		err = fmt.Errorf("Job %s is not queued", jobID)
	}
	if err != nil {
		o.logf("worker apply failed job=%s revision=%s correlation=%s step=claim service=none",
			safeLogID(job.ID), safeLogID(job.RevisionID), safeLogID(job.CorrelationID))
		return store.Job{}, err
	}
	run := &applyRun{o: o, job: job, step: "render", service: "none"}
	result, err := run.execute(ctx, snapshot, render)
	if err == nil {
		return result, nil
	}
	run.logFailure("failed")
	if len(run.promoted) > 0 && o.rollback(ctx, run.promoted, run.job) {
		return o.setStatus(ctx, run.job, jobs.RolledBack, store.JobPatch{
			ErrorMessage: store.Set(scrubMessage(errorMessage(err))),
			FinishedAt:   store.Set(o.now()),
		})
	}
	return o.fail(ctx, run.job, errorMessage(err), store.JobPatch{})
}

// applyRun is the state of one Apply attempt; job always holds the latest
// stored record, and step/service name the current phase for failure logs.
type applyRun struct {
	o        *Orchestrator
	job      store.Job
	promoted []Candidate
	step     string
	service  control.Service
}

func (r *applyRun) logFailure(outcome string) {
	r.o.logf("worker apply %s job=%s revision=%s correlation=%s step=%s service=%s", outcome,
		safeLogID(r.job.ID), safeLogID(r.job.RevisionID), safeLogID(r.job.CorrelationID), r.step, r.service)
}

func (r *applyRun) execute(ctx context.Context, snapshot json.RawMessage, render Renderer) (store.Job, error) {
	o := r.o
	candidates, err := render(ctx, DesiredSnapshot(snapshot), r.job)
	if err != nil {
		return store.Job{}, err
	}
	if len(candidates) == 0 {
		return store.Job{}, errors.New("No candidates were rendered")
	}

	validationOutput := map[string]json.RawMessage{}
	for _, candidate := range candidates {
		r.service = candidate.Service
		r.step = "write_candidate"
		if err := writeCandidate(o.fs(), candidate, orDefault(o.CandidateRoot, DefaultCandidateRoot)); err != nil {
			return store.Job{}, err
		}
		r.step = "stage"
		staged, err := o.request(ctx, candidate, r.job, control.OperationStage)
		if err != nil {
			return store.Job{}, err
		}
		if !staged.OK {
			r.logFailure("failed")
			return o.fail(ctx, r.job, orDefault(staged.Error, "Candidate staging failed"), store.JobPatch{
				ValidationOutput: store.Set(mustMarshal(struct {
					Service control.Service `json:"service"`
					Output  json.RawMessage `json:"output,omitempty"`
				}{candidate.Service, staged.Output})),
			})
		}
		r.step = "validate"
		validation, err := o.request(ctx, candidate, r.job, control.OperationValidate)
		if err != nil {
			return store.Job{}, err
		}
		setOutput(validationOutput, candidate.Service, validation.Output)
		if !validation.OK {
			r.logFailure("failed")
			return o.fail(ctx, r.job, orDefault(validation.Error, "Candidate validation failed"), store.JobPatch{
				ValidationOutput: store.Set(mustMarshal(validationOutput)),
			})
		}
	}

	r.step = "set_applying"
	if r.job, err = o.setStatus(ctx, r.job, jobs.Applying, store.JobPatch{
		ValidationOutput: store.Set(mustMarshal(validationOutput)),
	}); err != nil {
		return store.Job{}, err
	}
	applyOutput := map[string]json.RawMessage{}
	for _, candidate := range candidates {
		r.service = candidate.Service
		r.step = "promote"
		promoted, err := o.request(ctx, candidate, r.job, control.OperationPromote)
		if err != nil {
			return store.Job{}, err
		}
		if !promoted.OK {
			return store.Job{}, errors.New(orDefault(promoted.Error, "Candidate promotion failed"))
		}
		r.promoted = append(r.promoted, candidate)
		setOutput(applyOutput, candidate.Service, promoted.Output)
	}

	healthOutput := map[string]json.RawMessage{}
	for _, candidate := range candidates {
		r.service = candidate.Service
		r.step = "reload"
		reload, err := o.request(ctx, candidate, r.job, control.OperationReload)
		if err != nil {
			return store.Job{}, err
		}
		if !reload.OK {
			return store.Job{}, errors.New(orDefault(reload.Error, "Service reload failed"))
		}
		r.step = "health"
		health, err := o.request(ctx, candidate, r.job, control.OperationHealth)
		if err != nil {
			return store.Job{}, err
		}
		setOutput(healthOutput, candidate.Service, health.Output)
		if !health.OK || hasNginxDrift(health) {
			r.logFailure("failed")
			if o.rollback(ctx, r.promoted, r.job) {
				return o.setStatus(ctx, r.job, jobs.RolledBack, store.JobPatch{
					HealthOutput: store.Set(mustMarshal(healthOutput)),
					ErrorMessage: store.Set(scrubMessage(orDefault(health.Error, "Post-reload health failed; rolled back"))),
					FinishedAt:   store.Set(o.now()),
				})
			}
			return o.fail(ctx, r.job, orDefault(health.Error, "Post-reload health failed")+"; rollback failed", store.JobPatch{
				HealthOutput: store.Set(mustMarshal(healthOutput)),
			})
		}
	}

	r.step = "mark_applied"
	if _, err := o.Revisions.MarkApplied(ctx, r.job.RevisionID, o.now()); err != nil {
		return store.Job{}, err
	}
	return o.setStatus(ctx, r.job, jobs.Applied, store.JobPatch{
		ApplyOutput:  store.Set(mustMarshal(applyOutput)),
		HealthOutput: store.Set(mustMarshal(healthOutput)),
		FinishedAt:   store.Set(o.now()),
	})
}

// ReconcileApplied repairs one durable applied revision when the nginx data
// plane has reverted to the valid baked fallback or lost its promoted
// configuration. The store atomically suppresses only an active duplicate;
// terminal repair jobs do not prevent a later drift event from being repaired.
// It returns nil when nothing needed repair.
func (o *Orchestrator) ReconcileApplied(ctx context.Context, revision revisions.Revision, render Renderer) (*store.Job, error) {
	if revision.AppliedAt == nil {
		return nil, nil
	}
	probe, err := o.Control.Execute(ctx, control.Request{
		RequestID:  uuid.NewString(),
		Operation:  control.OperationHealth,
		Service:    control.ServiceNginx,
		RevisionID: revision.ID,
		Checksum:   revision.Checksum,
		CandidatePath: path.Join(
			orDefault(o.CandidateRoot, DefaultCandidateRoot), revision.ID, "nginx"),
	})
	if err != nil {
		return nil, err
	}
	if !probe.OK || !hasNginxDrift(probe) {
		return nil, nil
	}
	claimedAt := o.now()
	repair, err := o.Jobs.EnqueueIfNotActive(ctx, store.EnqueueInput{
		RevisionID:    revision.ID,
		Target:        store.TargetNginx,
		Status:        jobs.Validating,
		ClaimedAt:     &claimedAt,
		StartedAt:     &claimedAt,
		CorrelationID: ReconciliationCorrelationPrefix + revision.ID,
	})
	if err != nil || repair == nil {
		return nil, err
	}
	result, err := o.Apply(ctx, repair.ID, revision.Snapshot, render)
	if err != nil {
		return nil, err
	}
	return &result, nil
}

func (o *Orchestrator) setStatus(ctx context.Context, job store.Job, status jobs.Status, patch store.JobPatch) (store.Job, error) {
	next, err := jobs.Transition(job.Status, status)
	if err != nil {
		return store.Job{}, err
	}
	patch.Status = &next
	return o.Jobs.Update(ctx, job.ID, patch)
}

func (o *Orchestrator) fail(ctx context.Context, job store.Job, message string, patch store.JobPatch) (store.Job, error) {
	patch.ErrorMessage = store.Set(scrubMessage(message))
	patch.FinishedAt = store.Set(o.now())
	return o.setStatus(ctx, job, jobs.Failed, patch)
}

// rollback asks control to roll back every promoted candidate in reverse
// order, attempting all of them, and reports whether every one succeeded.
func (o *Orchestrator) rollback(ctx context.Context, candidates []Candidate, job store.Job) bool {
	succeeded := true
	for _, candidate := range slices.Backward(candidates) {
		response, err := o.request(ctx, candidate, job, control.OperationRollback)
		if err != nil || !response.OK {
			o.logf("worker rollback failed job=%s revision=%s correlation=%s service=%s",
				safeLogID(job.ID), safeLogID(job.RevisionID), safeLogID(job.CorrelationID), candidate.Service)
			succeeded = false
		}
	}
	return succeeded
}

func (o *Orchestrator) request(ctx context.Context, candidate Candidate, job store.Job, operation control.Operation) (control.Response, error) {
	return o.Control.Execute(ctx, control.Request{
		RequestID:     uuid.NewString(),
		Operation:     operation,
		Service:       candidate.Service,
		RevisionID:    job.RevisionID,
		Checksum:      candidate.Checksum,
		CandidatePath: candidate.CandidatePath,
	})
}

func (o *Orchestrator) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func (o *Orchestrator) fs() FS {
	if o.FS != nil {
		return o.FS
	}
	return OSFS{}
}

func (o *Orchestrator) logf(format string, args ...any) {
	if o.Log != nil {
		o.Log(fmt.Sprintf(format, args...))
	}
}

var safeLogIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,80}$`)

// safeLogID keeps only identifier-shaped values in log lines.
func safeLogID(value string) string {
	if safeLogIDPattern.MatchString(value) {
		return value
	}
	return "[redacted]"
}

// setOutput records a control output under its service; an absent output is
// omitted like an undefined value in JSON.stringify.
func setOutput(outputs map[string]json.RawMessage, service control.Service, output json.RawMessage) {
	if output != nil {
		outputs[string(service)] = output
	}
}

func hasNginxDrift(response control.Response) bool {
	var output struct {
		Status any `json:"status"`
	}
	if json.Unmarshal(response.Output, &output) != nil {
		return false
	}
	return output.Status == "drift"
}

func errorMessage(err error) string {
	return orDefault(err.Error(), "Apply failed")
}

func orDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
