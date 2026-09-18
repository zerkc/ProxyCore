package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/enrollment"
)

var (
	errProcessRuntimeHTTPShutdownTimeout = errors.New("ordinary HTTP shutdown timed out")
	errProcessRuntimeWorkerJoinTimeout   = errors.New("process worker join timed out")
	errProcessRuntimeHTTPShutdownFailed  = errors.New("ordinary HTTP shutdown failed")
	errProcessRuntimeHTTPServeFailed     = errors.New("ordinary HTTP serve failed")
)

const defaultServerShutdownTimeout = 10 * time.Second

func isNormalRuntimeClose(err error) bool {
	return err == nil || errors.Is(err, context.Canceled) || errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed)
}

type ordinaryHTTPServer interface {
	ListenAndServe() error
	Shutdown(context.Context) error
}

type enrollmentSupervisorRunner interface {
	Run(context.Context) error
	Status() enrollment.EnrollmentTLSSupervisorStatus
}

type processRuntimeWithTimeout func(context.Context, time.Duration) (context.Context, context.CancelFunc)

type serverRuntimeOptions struct {
	ordinary                 ordinaryHTTPServer
	enrollment               enrollmentSupervisorRunner
	enrollmentStatusInterval time.Duration
	shutdownTimeout          time.Duration
	shutdownWithTimeout      processRuntimeWithTimeout
	cancelObserved           func()
	workers                  []func(context.Context)
}

type processRuntimeShutdownOptions struct {
	Timeout           time.Duration
	WithTimeout       processRuntimeWithTimeout
	AfterHTTPShutdown func()
}

func newProcessRuntimeCancel(cancel context.CancelFunc, observed func()) context.CancelFunc {
	var once sync.Once
	return func() {
		once.Do(func() {
			if cancel != nil {
				cancel()
			}
			if observed != nil {
				observed()
			}
		})
	}
}

func shutdownProcessRuntime(cancel context.CancelFunc, ordinary ordinaryHTTPServer, workersDone <-chan struct{}, opts processRuntimeShutdownOptions) error {
	if cancel != nil {
		cancel()
	}
	if ordinary == nil {
		return errors.New("process shutdown unavailable")
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultServerShutdownTimeout
	}
	withTimeout := opts.WithTimeout
	if withTimeout == nil {
		withTimeout = context.WithTimeout
	}
	budgetCtx, budgetCancel := withTimeout(context.Background(), timeout)
	if budgetCtx == nil {
		return errors.New("process shutdown budget unavailable")
	}
	if budgetCancel != nil {
		defer budgetCancel()
	}

	shutdownDone := make(chan error, 1)
	go func() {
		shutdownDone <- ordinary.Shutdown(budgetCtx)
	}()
	var firstErr error
	select {
	case <-budgetCtx.Done():
		return errProcessRuntimeHTTPShutdownTimeout
	case err := <-shutdownDone:
		if budgetCtx.Err() != nil {
			return errProcessRuntimeHTTPShutdownTimeout
		}
		if err != nil && !isNormalRuntimeClose(err) {
			firstErr = errProcessRuntimeHTTPShutdownFailed
		}
		if opts.AfterHTTPShutdown != nil {
			opts.AfterHTTPShutdown()
		}
	}

	if workersDone == nil {
		return firstErr
	}
	select {
	case <-budgetCtx.Done():
		if firstErr != nil {
			return errors.Join(firstErr, errProcessRuntimeWorkerJoinTimeout)
		}
		return errProcessRuntimeWorkerJoinTimeout
	case <-workersDone:
		if budgetCtx.Err() != nil {
			if firstErr != nil {
				return errors.Join(firstErr, errProcessRuntimeWorkerJoinTimeout)
			}
			return errProcessRuntimeWorkerJoinTimeout
		}
		return firstErr
	}
}

func isProcessRuntimeTimeout(err error) bool {
	return errors.Is(err, errProcessRuntimeHTTPShutdownTimeout) || errors.Is(err, errProcessRuntimeWorkerJoinTimeout)
}

func redactedProcessRuntimeError(err error) string {
	switch {
	case errors.Is(err, errProcessRuntimeHTTPShutdownTimeout):
		return "ordinary HTTP shutdown timed out"
	case errors.Is(err, errProcessRuntimeHTTPShutdownFailed) && errors.Is(err, errProcessRuntimeWorkerJoinTimeout):
		return "ordinary HTTP shutdown failed; worker shutdown timed out"
	case errors.Is(err, errProcessRuntimeHTTPShutdownFailed):
		return "ordinary HTTP shutdown failed"
	case errors.Is(err, errProcessRuntimeHTTPServeFailed):
		return "ordinary HTTP serve failed"
	case errors.Is(err, errProcessRuntimeWorkerJoinTimeout):
		return "worker shutdown timed out"
	default:
		return "process shutdown failed"
	}
}
