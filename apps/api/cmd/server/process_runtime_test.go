package main

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

type shutdownTestHTTPServer struct {
	serveStarted    chan struct{}
	serveFinished   chan struct{}
	releaseServe    chan struct{}
	shutdownStarted chan struct{}
	shutdownDone    chan struct{}
	releaseShutdown chan struct{}
	ignoreContext   bool
	serveErr        error
	shutdownErr     error
}

func newShutdownTestHTTPServer() *shutdownTestHTTPServer {
	return &shutdownTestHTTPServer{
		serveStarted:    make(chan struct{}),
		serveFinished:   make(chan struct{}),
		releaseServe:    make(chan struct{}),
		shutdownStarted: make(chan struct{}),
		shutdownDone:    make(chan struct{}),
		releaseShutdown: make(chan struct{}),
	}
}

func (s *shutdownTestHTTPServer) ListenAndServe() error {
	close(s.serveStarted)
	<-s.releaseServe
	close(s.serveFinished)
	if s.serveErr != nil {
		return s.serveErr
	}
	return http.ErrServerClosed
}

func (s *shutdownTestHTTPServer) Shutdown(ctx context.Context) error {
	close(s.shutdownStarted)
	if s.ignoreContext {
		<-s.releaseShutdown
	} else {
		select {
		case <-s.releaseShutdown:
		case <-ctx.Done():
		}
	}
	close(s.shutdownDone)
	return s.shutdownErr
}

type processRuntimeTestBudget struct {
	done chan struct{}
	once sync.Once
}

func newProcessRuntimeTestBudget() *processRuntimeTestBudget {
	return &processRuntimeTestBudget{done: make(chan struct{})}
}

func (b *processRuntimeTestBudget) Context() context.Context {
	return processRuntimeTestContext{done: b.done}
}

func (b *processRuntimeTestBudget) Expire() { b.once.Do(func() { close(b.done) }) }

func (b *processRuntimeTestBudget) Cancel() { b.Expire() }

type processRuntimeTestContext struct {
	done <-chan struct{}
}

func (c processRuntimeTestContext) Deadline() (time.Time, bool) { return time.Time{}, false }

func (c processRuntimeTestContext) Done() <-chan struct{} { return c.done }

func (c processRuntimeTestContext) Err() error {
	select {
	case <-c.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

func (c processRuntimeTestContext) Value(any) any { return nil }

func processRuntimeTestWithTimeout(budgets chan<- *processRuntimeTestBudget) func(context.Context, time.Duration) (context.Context, context.CancelFunc) {
	return func(context.Context, time.Duration) (context.Context, context.CancelFunc) {
		budget := newProcessRuntimeTestBudget()
		budgets <- budget
		return budget.Context(), budget.Cancel
	}
}

func waitProcessRuntimeTest(t *testing.T, signal <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", description)
	}
}

func assertOneProcessRuntimeBudget(t *testing.T, budgets <-chan *processRuntimeTestBudget) *processRuntimeTestBudget {
	t.Helper()
	budget := <-budgets
	select {
	case <-budgets:
		t.Fatal("shutdown created more than one timeout budget")
	default:
	}
	return budget
}

func TestShutdownProcessRuntimeJoinsWorkersWithOneBudget(t *testing.T) {
	server := newShutdownTestHTTPServer()
	close(server.releaseShutdown)
	workersDone := make(chan struct{})
	close(workersDone)
	budgets := make(chan *processRuntimeTestBudget, 2)
	cancelCalls := 0

	err := shutdownProcessRuntime(func() { cancelCalls++ }, server, workersDone, processRuntimeShutdownOptions{
		Timeout:     time.Second,
		WithTimeout: processRuntimeTestWithTimeout(budgets),
	})
	if err != nil {
		t.Fatalf("shutdownProcessRuntime: %v", err)
	}
	if cancelCalls != 1 {
		t.Fatalf("cancel calls=%d, want 1", cancelCalls)
	}
	budget := assertOneProcessRuntimeBudget(t, budgets)
	if budget.Context().Err() == nil {
		t.Fatal("shutdown did not release its one budget")
	}
	waitProcessRuntimeTest(t, server.shutdownDone, "ordinary HTTP shutdown")
}

func TestShutdownProcessRuntimeHTTPTimeoutConsumesSharedBudget(t *testing.T) {
	server := newShutdownTestHTTPServer()
	server.ignoreContext = true
	workersDone := make(chan struct{})
	budgets := make(chan *processRuntimeTestBudget, 2)
	result := make(chan error, 1)
	go func() {
		result <- shutdownProcessRuntime(func() {}, server, workersDone, processRuntimeShutdownOptions{
			Timeout:     time.Second,
			WithTimeout: processRuntimeTestWithTimeout(budgets),
		})
	}()
	waitProcessRuntimeTest(t, server.shutdownStarted, "ordinary HTTP shutdown start")
	budget := assertOneProcessRuntimeBudget(t, budgets)
	budget.Expire()
	select {
	case err := <-result:
		if !errors.Is(err, errProcessRuntimeHTTPShutdownTimeout) {
			t.Fatalf("shutdown error=%v, want HTTP shutdown timeout", err)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdownProcessRuntime did not bound HTTP shutdown")
	}
	select {
	case <-server.shutdownDone:
		t.Fatal("uncooperative HTTP shutdown finished before cleanup")
	default:
	}
	close(server.releaseShutdown)
	waitProcessRuntimeTest(t, server.shutdownDone, "blocked HTTP shutdown cleanup")
}

func TestShutdownProcessRuntimeWorkerUsesRemainingSharedBudget(t *testing.T) {
	server := newShutdownTestHTTPServer()
	close(server.releaseShutdown)
	workersDone := make(chan struct{})
	workerRelease := make(chan struct{})
	workerFinished := make(chan struct{})
	go func() {
		<-workerRelease
		close(workerFinished)
		close(workersDone)
	}()
	budgets := make(chan *processRuntimeTestBudget, 2)
	httpComplete := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		result <- shutdownProcessRuntime(func() {}, server, workersDone, processRuntimeShutdownOptions{
			Timeout:           time.Second,
			WithTimeout:       processRuntimeTestWithTimeout(budgets),
			AfterHTTPShutdown: func() { close(httpComplete) },
		})
	}()
	waitProcessRuntimeTest(t, httpComplete, "ordinary HTTP shutdown completion")
	budget := assertOneProcessRuntimeBudget(t, budgets)
	budget.Expire()
	select {
	case err := <-result:
		if !errors.Is(err, errProcessRuntimeWorkerJoinTimeout) {
			t.Fatalf("shutdown error=%v, want worker join timeout", err)
		}
	case <-time.After(time.Second):
		t.Fatal("worker join did not observe the exhausted shared budget")
	}
	close(workerRelease)
	waitProcessRuntimeTest(t, workerFinished, "uncooperative worker cleanup")
}

func TestShutdownProcessRuntimeCombinesGenericFailureAndWorkerTimeout(t *testing.T) {
	server := newShutdownTestHTTPServer()
	close(server.releaseShutdown)
	server.shutdownErr = errors.New("BEGIN PRIVATE KEY secret")
	workersDone := make(chan struct{})
	budgets := make(chan *processRuntimeTestBudget, 2)
	httpComplete := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		result <- shutdownProcessRuntime(func() {}, server, workersDone, processRuntimeShutdownOptions{
			Timeout:           time.Second,
			WithTimeout:       processRuntimeTestWithTimeout(budgets),
			AfterHTTPShutdown: func() { close(httpComplete) },
		})
	}()
	waitProcessRuntimeTest(t, httpComplete, "ordinary HTTP shutdown completion")
	budget := assertOneProcessRuntimeBudget(t, budgets)
	budget.Expire()
	select {
	case err := <-result:
		if !errors.Is(err, errProcessRuntimeHTTPShutdownFailed) || !errors.Is(err, errProcessRuntimeWorkerJoinTimeout) {
			t.Fatalf("shutdown error=%v, want generic HTTP failure plus worker timeout", err)
		}
		if strings.Contains(err.Error(), "PRIVATE KEY") {
			t.Fatalf("shutdown error leaked secret: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("combined shutdown result did not return")
	}
}

func TestRunServerRuntimeRedactsOrdinaryServeFailure(t *testing.T) {
	server := newShutdownTestHTTPServer()
	server.serveErr = errors.New("listen leak-marker internal address")
	close(server.releaseServe)
	close(server.releaseShutdown)
	var logs runtimeTestLog
	result := make(chan error, 1)
	go func() {
		result <- runServerRuntime(context.Background(), log.New(&logs, "", 0), serverRuntimeOptions{ordinary: server})
	}()
	waitProcessRuntimeTest(t, server.serveStarted, "ordinary HTTP server start")
	select {
	case err := <-result:
		if !errors.Is(err, errProcessRuntimeHTTPServeFailed) {
			t.Fatalf("runtime error=%v, want generic serve failure", err)
		}
		if strings.Contains(err.Error(), "leak-marker") || strings.Contains(logs.String(), "leak-marker") {
			t.Fatalf("ordinary serve failure leaked marker: error=%v logs=%q", err, logs.String())
		}
	case <-time.After(time.Second):
		t.Fatal("runServerRuntime did not classify ordinary serve failure")
	}
}

func TestRunServerRuntimeRedactsOrdinaryShutdownFailure(t *testing.T) {
	processCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := newShutdownTestHTTPServer()
	close(server.releaseShutdown)
	server.shutdownErr = errors.New("shutdown leak-marker internal address")
	var logs runtimeTestLog
	result := make(chan error, 1)
	go func() {
		result <- runServerRuntime(processCtx, log.New(&logs, "", 0), serverRuntimeOptions{ordinary: server})
	}()
	waitProcessRuntimeTest(t, server.serveStarted, "ordinary HTTP server start")
	cancel()
	waitProcessRuntimeTest(t, server.shutdownStarted, "ordinary HTTP shutdown start")
	select {
	case err := <-result:
		if !errors.Is(err, errProcessRuntimeHTTPShutdownFailed) {
			t.Fatalf("runtime error=%v, want generic shutdown failure", err)
		}
		if strings.Contains(err.Error(), "leak-marker") || strings.Contains(logs.String(), "leak-marker") {
			t.Fatalf("ordinary shutdown failure leaked marker: error=%v logs=%q", err, logs.String())
		}
	case <-time.After(time.Second):
		t.Fatal("runServerRuntime did not classify ordinary shutdown failure")
	}
	close(server.releaseServe)
	waitProcessRuntimeTest(t, server.serveFinished, "HTTP serve cleanup")
}

func TestRunServerRuntimeCancelsExactlyOnceOnNormalShutdown(t *testing.T) {
	processCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := newShutdownTestHTTPServer()
	close(server.releaseShutdown)
	cancelCalls := 0
	result := make(chan error, 1)
	go func() {
		result <- runServerRuntime(processCtx, log.New(io.Discard, "", 0), serverRuntimeOptions{
			ordinary:       server,
			cancelObserved: func() { cancelCalls++ },
		})
	}()
	waitProcessRuntimeTest(t, server.serveStarted, "ordinary HTTP server start")
	cancel()
	waitProcessRuntimeTest(t, server.shutdownStarted, "ordinary HTTP shutdown start")
	close(server.releaseServe)
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("runServerRuntime: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("runServerRuntime did not finish normal shutdown")
	}
	if cancelCalls != 1 {
		t.Fatalf("cancel calls=%d, want 1", cancelCalls)
	}
}

func TestRunServerRuntimeCancelsExactlyOnceOnTimeout(t *testing.T) {
	processCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := newShutdownTestHTTPServer()
	server.ignoreContext = true
	budgets := make(chan *processRuntimeTestBudget, 2)
	cancelCalls := 0
	result := make(chan error, 1)
	go func() {
		result <- runServerRuntime(processCtx, log.New(io.Discard, "", 0), serverRuntimeOptions{
			ordinary:            server,
			cancelObserved:      func() { cancelCalls++ },
			shutdownWithTimeout: processRuntimeTestWithTimeout(budgets),
		})
	}()
	waitProcessRuntimeTest(t, server.serveStarted, "ordinary HTTP server start")
	cancel()
	waitProcessRuntimeTest(t, server.shutdownStarted, "ordinary HTTP shutdown start")
	budget := assertOneProcessRuntimeBudget(t, budgets)
	budget.Expire()
	select {
	case err := <-result:
		if !errors.Is(err, errProcessRuntimeHTTPShutdownTimeout) {
			t.Fatalf("runtime error=%v, want HTTP shutdown timeout", err)
		}
	case <-time.After(time.Second):
		t.Fatal("runServerRuntime did not return after timeout")
	}
	if cancelCalls != 1 {
		t.Fatalf("cancel calls=%d, want 1", cancelCalls)
	}
	close(server.releaseShutdown)
	close(server.releaseServe)
	waitProcessRuntimeTest(t, server.shutdownDone, "HTTP shutdown cleanup")
	waitProcessRuntimeTest(t, server.serveFinished, "HTTP serve cleanup")
}
