package sync

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

type terminalReaderFunc func(context.Context, uuid.UUID) (ApplyJobTerminal, error)

func (f terminalReaderFunc) GetApplyJobTerminal(ctx context.Context, jobID uuid.UUID) (ApplyJobTerminal, error) {
	return f(ctx, jobID)
}

func TestWaitForTerminalApplyReturnsFirstTerminalObservation(t *testing.T) {
	jobID := uuid.New()
	finished := time.Date(2026, 5, 10, 11, 12, 13, 0, time.UTC)
	want := ApplyJobTerminal{Status: ApplyTerminalStatusApplied, FinishedAt: &finished}
	calls := 0
	got, err := WaitForTerminalApply(context.Background(), terminalReaderFunc(func(_ context.Context, gotID uuid.UUID) (ApplyJobTerminal, error) {
		calls++
		if gotID != jobID {
			t.Fatalf("job id = %s, want %s", gotID, jobID)
		}
		return want, nil
	}), jobID, WaitOptions{PollInterval: time.Hour, Timeout: time.Hour})
	if err != nil {
		t.Fatalf("WaitForTerminalApply: %v", err)
	}
	if calls != 1 || got.Status != want.Status || got.FinishedAt == nil || !got.FinishedAt.Equal(finished) {
		t.Fatalf("calls=%d result=%+v, want one call and %+v", calls, got, want)
	}
}

func TestWaitForTerminalApplyObservesStatusChangesBetweenPolls(t *testing.T) {
	jobID := uuid.New()
	statuses := []ApplyTerminalStatus{"queued", "running", ApplyTerminalStatusApplied}
	calls := 0
	got, err := WaitForTerminalApply(context.Background(), terminalReaderFunc(func(_ context.Context, gotID uuid.UUID) (ApplyJobTerminal, error) {
		if gotID != jobID {
			t.Fatalf("job id = %s, want %s", gotID, jobID)
		}
		status := statuses[calls]
		calls++
		return ApplyJobTerminal{Status: status}, nil
	}), jobID, WaitOptions{PollInterval: time.Nanosecond, Timeout: time.Second})
	if err != nil {
		t.Fatalf("WaitForTerminalApply: %v", err)
	}
	if calls != len(statuses) || got.Status != ApplyTerminalStatusApplied {
		t.Fatalf("calls=%d result=%+v, want %d calls and applied", calls, got, len(statuses))
	}
}

func TestWaitForTerminalApplyReturnsLastStatusOnTimeout(t *testing.T) {
	jobID := uuid.New()
	clock := advancingApplyWaitClock{}
	got, err := WaitForTerminalApply(context.Background(), terminalReaderFunc(func(context.Context, uuid.UUID) (ApplyJobTerminal, error) {
		return ApplyJobTerminal{Status: "running"}, nil
	}), jobID, WaitOptions{PollInterval: time.Nanosecond, Timeout: time.Nanosecond, Clock: clock.Now})
	if !errors.Is(err, ErrApplyWaitTimeout) {
		t.Fatalf("error = %v, want ErrApplyWaitTimeout", err)
	}
	var timeoutErr *ApplyWaitTimeoutError
	if !errors.As(err, &timeoutErr) {
		t.Fatalf("error = %T %v, want ApplyWaitTimeoutError", err, err)
	}
	if timeoutErr.Last.Status != "running" || got.Status != "running" {
		t.Fatalf("last status error=%q result=%q, want running", timeoutErr.Last.Status, got.Status)
	}
}

func TestWaitForTerminalApplyReturnsContextCancellationDuringWait(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	called := make(chan struct{})
	reader := terminalReaderFunc(func(ctx context.Context, _ uuid.UUID) (ApplyJobTerminal, error) {
		close(called)
		<-ctx.Done()
		return ApplyJobTerminal{Status: "running"}, ctx.Err()
	})
	result := make(chan error, 1)
	go func() {
		_, err := WaitForTerminalApply(ctx, reader, uuid.New(), WaitOptions{PollInterval: time.Hour, Timeout: time.Hour})
		result <- err
	}()
	waitForApplyWaitSignal(t, called)
	cancel()
	if err := waitForApplyWaitResult(t, result); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestWaitForTerminalApplyPropagatesStoreError(t *testing.T) {
	cause := errors.New("database unavailable")
	_, err := WaitForTerminalApply(context.Background(), terminalReaderFunc(func(context.Context, uuid.UUID) (ApplyJobTerminal, error) {
		return ApplyJobTerminal{}, cause
	}), uuid.New(), WaitOptions{})
	if !errors.Is(err, cause) {
		t.Fatalf("error = %v, want cause", err)
	}
}

func TestWaitForTerminalApplyRejectsMissingJobID(t *testing.T) {
	called := false
	_, err := WaitForTerminalApply(context.Background(), terminalReaderFunc(func(context.Context, uuid.UUID) (ApplyJobTerminal, error) {
		called = true
		return ApplyJobTerminal{}, nil
	}), uuid.Nil, WaitOptions{})
	if err == nil || called {
		t.Fatalf("error=%v called=%t, want validation error without polling", err, called)
	}
}

type advancingApplyWaitClock struct {
	current time.Time
}

func (c *advancingApplyWaitClock) Now() time.Time {
	if c.current.IsZero() {
		c.current = time.Unix(0, 0)
	}
	c.current = c.current.Add(time.Nanosecond)
	return c.current
}

func waitForApplyWaitSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case <-signal:
	case <-timer.C:
		t.Fatal("waiter did not poll the terminal reader")
	}
}

func waitForApplyWaitResult(t *testing.T, result <-chan error) error {
	t.Helper()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case err := <-result:
		return err
	case <-timer.C:
		t.Fatal("waiter did not return after context cancellation")
		return nil
	}
}
