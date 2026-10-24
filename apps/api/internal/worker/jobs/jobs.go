// Package jobs ports the apply-job state machine from packages/domain/src/jobs.ts.
package jobs

import (
	"errors"
	"fmt"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
)

// Status is the lifecycle state of an apply job.
type Status string

const (
	Queued     Status = "queued"
	Validating Status = "validating"
	Applying   Status = "applying"
	Applied    Status = "applied"
	Failed     Status = "failed"
	RolledBack Status = "rolled-back"
)

// ErrInvalidTransition matches every error returned for a disallowed move.
var ErrInvalidTransition = errors.New("invalid job transition")

var transitions = map[Status][]Status{
	Queued:     {Validating},
	Validating: {Applying, Failed},
	Applying:   {Applied, Failed, RolledBack},
	Applied:    {},
	Failed:     {Queued},
	RolledBack: {Queued},
}

// transitionError carries the TS message and domain code while still matching
// ErrInvalidTransition via errors.Is and domain.ValidationError via errors.As.
type transitionError struct{ *domain.ValidationError }

func (e transitionError) Is(target error) bool { return target == ErrInvalidTransition }
func (e transitionError) As(target any) bool {
	if p, ok := target.(**domain.ValidationError); ok {
		*p = e.ValidationError
		return true
	}
	return false
}

// Transition returns next when current may move to it, otherwise an error with
// the same message and code ("JOB_TRANSITION") as the TypeScript domain.
func Transition(current, next Status) (Status, error) {
	for _, allowed := range transitions[current] {
		if allowed == next {
			return next, nil
		}
	}
	return "", transitionError{&domain.ValidationError{
		Message: fmt.Sprintf("Invalid job transition: %s -> %s", current, next),
		Code:    "JOB_TRANSITION",
	}}
}

// IsTerminal reports whether the job has reached applied, failed or rolled-back.
func IsTerminal(status Status) bool {
	return status == Applied || status == Failed || status == RolledBack
}
