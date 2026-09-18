package sync

import (
	"context"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
)

// These aliases keep the public waiter surface in sync while allowing the
// configuration store to implement the reader without an import cycle.
type ApplyTerminalStatus = configuration.ApplyTerminalStatus
type ApplyJobTerminal = configuration.ApplyJobTerminal
type TerminalApplyReader = configuration.TerminalApplyReader
type WaitOptions = configuration.ApplyWaitOptions
type ApplyWaitTimeoutError = configuration.ApplyWaitTimeoutError

const (
	ApplyTerminalStatusApplied    ApplyTerminalStatus = configuration.ApplyTerminalStatusApplied
	ApplyTerminalStatusFailed     ApplyTerminalStatus = configuration.ApplyTerminalStatusFailed
	ApplyTerminalStatusRolledBack ApplyTerminalStatus = configuration.ApplyTerminalStatusRolledBack
	DefaultApplyWaitPollInterval                      = configuration.DefaultApplyWaitPollInterval
	DefaultApplyWaitTimeout                           = configuration.DefaultApplyWaitTimeout
)

var (
	ErrApplyWaitTimeout         = configuration.ErrApplyWaitTimeout
	ErrApplyWaitJobIDRequired   = configuration.ErrApplyWaitJobIDRequired
	ErrApplyWaitStoreRequired   = configuration.ErrApplyWaitStoreRequired
	ErrApplyWaitContextRequired = configuration.ErrApplyWaitContextRequired
)

// WaitForTerminalApply is the sync package facade for the bounded apply
// waiter. The implementation lives at the lower persistence layer to keep the
// existing sync -> configuration dependency acyclic.
func WaitForTerminalApply(ctx context.Context, store TerminalApplyReader, jobID uuid.UUID, opts WaitOptions) (ApplyJobTerminal, error) {
	return configuration.WaitForTerminalApply(ctx, store, jobID, opts)
}
