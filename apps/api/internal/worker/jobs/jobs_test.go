package jobs

import (
	"errors"
	"testing"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
)

func TestTransitionAllowsNormalValidationApplyPath(t *testing.T) {
	status := Queued
	for _, next := range []Status{Validating, Applying, Applied} {
		var err error
		status, err = Transition(status, next)
		if err != nil {
			t.Fatalf("Transition to %s: %v", next, err)
		}
	}
	if status != Applied {
		t.Fatalf("status = %s, want applied", status)
	}
}

func TestTransitionTable(t *testing.T) {
	allowed := map[Status][]Status{
		Queued:     {Validating},
		Validating: {Applying, Failed},
		Applying:   {Applied, Failed, RolledBack},
		Applied:    {},
		Failed:     {Queued},
		RolledBack: {Queued},
	}
	all := []Status{Queued, Validating, Applying, Applied, Failed, RolledBack}
	for _, from := range all {
		for _, to := range all {
			want := false
			for _, ok := range allowed[from] {
				if ok == to {
					want = true
				}
			}
			got, err := Transition(from, to)
			if want && (err != nil || got != to) {
				t.Errorf("%s -> %s: got (%s, %v), want allowed", from, to, got, err)
			}
			if !want && err == nil {
				t.Errorf("%s -> %s: want error", from, to)
			}
		}
	}
}

func TestTransitionRejectsJumpToApplied(t *testing.T) {
	_, err := Transition(Queued, Applied)
	if err == nil {
		t.Fatal("expected error")
	}
	if err.Error() != "Invalid job transition: queued -> applied" {
		t.Errorf("message = %q", err.Error())
	}
	var ve *domain.ValidationError
	if !errors.As(err, &ve) || ve.Code != "JOB_TRANSITION" {
		t.Errorf("error = %#v, want domain.ValidationError JOB_TRANSITION", err)
	}
	if !errors.Is(err, ErrInvalidTransition) {
		t.Error("error should match ErrInvalidTransition")
	}
}

func TestTransitionRejectsUnknownStatus(t *testing.T) {
	if _, err := Transition(Status("bogus"), Queued); err == nil {
		t.Error("expected error for unknown current status")
	}
}

func TestIsTerminal(t *testing.T) {
	cases := map[Status]bool{
		Queued: false, Validating: false, Applying: false,
		Applied: true, Failed: true, RolledBack: true,
	}
	for status, want := range cases {
		if got := IsTerminal(status); got != want {
			t.Errorf("IsTerminal(%s) = %v, want %v", status, got, want)
		}
	}
}
