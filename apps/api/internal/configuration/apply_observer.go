package configuration

import "context"

type applyJobObserverKey struct{}

// WithApplyJobObserver attaches a request-local observer for successfully
// enqueued apply jobs. It does not alter the persisted correlation ID.
func WithApplyJobObserver(ctx context.Context, observe func(ApplyResult)) context.Context {
	if observe == nil {
		return ctx
	}
	return context.WithValue(ctx, applyJobObserverKey{}, observe)
}

// NotifyApplyJob reports only a successful, durable enqueue to an observer.
// Callers must invoke it after the enclosing transaction has committed.
func NotifyApplyJob(ctx context.Context, result ApplyResult) {
	if ctx == nil {
		return
	}
	if observe, ok := ctx.Value(applyJobObserverKey{}).(func(ApplyResult)); ok {
		observe(result)
	}
}
