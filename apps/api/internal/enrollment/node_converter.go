package enrollment

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/identity"
	replicationsnapshot "github.com/zerkc/ProxyCore/apps/api/internal/snapshot"
)

var (
	// ErrNodeConversionAborted means the imported apply reached a terminal
	// state other than applied. The archive and job remain as audit evidence.
	ErrNodeConversionAborted = errors.New("node conversion aborted")
	// ErrNodeConversionDenied means the durable identity state machine refused
	// the final NODE transition after a successful apply.
	ErrNodeConversionDenied      = errors.New("node conversion denied")
	errNodeConversionUnavailable = errors.New("node conversion dependencies are unavailable")
)

// NodeConverter orchestrates the bounded archive/import/apply/identity
// sequence. Its dependencies are ports or existing phase services; it does
// not own a long-lived worker loop or a cross-step transaction.
type NodeConverter struct {
	importer      *replicationsnapshot.Importer
	archive       replicationsnapshot.ArchiveStore
	identity      *identity.Service
	applyWaiter   configuration.TerminalApplyReader
	applyEnqueuer replicationsnapshot.ApplyJobEnqueuer
	logger        func(format string, args ...any)
	now           func() time.Time
}

// NodeConverterOptions supplies the converter dependencies. When Importer is
// nil, Archive and ApplyEnqueuer are used to construct the existing importer.
type NodeConverterOptions struct {
	Importer      *replicationsnapshot.Importer
	Archive       replicationsnapshot.ArchiveStore
	Identity      *identity.Service
	ApplyWaiter   configuration.TerminalApplyReader
	ApplyEnqueuer replicationsnapshot.ApplyJobEnqueuer
	Logger        func(format string, args ...any)
	Now           func() time.Time
}

// NewNodeConverter constructs a single-call NODE conversion orchestrator.
func NewNodeConverter(opts NodeConverterOptions) *NodeConverter {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	importer := opts.Importer
	if importer == nil && opts.Archive != nil && opts.ApplyEnqueuer != nil {
		importer = replicationsnapshot.NewImporter(opts.Archive, opts.ApplyEnqueuer, now)
	}
	logger := opts.Logger
	if logger == nil {
		logger = func(string, ...any) {}
	}
	return &NodeConverter{
		importer:      importer,
		archive:       opts.Archive,
		identity:      opts.Identity,
		applyWaiter:   opts.ApplyWaiter,
		applyEnqueuer: opts.ApplyEnqueuer,
		logger:        logger,
		now:           now,
	}
}

// NodeConversionInput contains the already validator-approved envelope and
// the receiving installation's authoritative local values.
type NodeConversionInput struct {
	Envelope         *replicationsnapshot.Envelope
	LocalNodeID      domain.NodeID
	LocalIngress     domain.Ingress
	LocalInstallId   domain.InstallationID
	ArchiveTTL       time.Duration
	ApplyWaitTimeout time.Duration
}

type NodeLineage struct {
	NodeID       domain.NodeID
	Generation   uint64
	Role         domain.TopologyRole
	ClusterKeyID *uuid.UUID
}

type NodeConversionResult struct {
	ArchiveID   uuid.UUID
	ApplyJobID  uuid.UUID
	Identity    identity.Identity
	NodeLineage NodeLineage
}

// NodeConversionAbortedError retains the terminal apply evidence while
// allowing callers to use errors.Is(err, ErrNodeConversionAborted).
type NodeConversionAbortedError struct {
	Status     configuration.ApplyTerminalStatus
	ArchiveID  uuid.UUID
	ApplyJobID uuid.UUID
}

func (e *NodeConversionAbortedError) Error() string {
	if e == nil {
		return ErrNodeConversionAborted.Error()
	}
	return fmt.Sprintf("%s: apply status %q archive=%s job=%s", ErrNodeConversionAborted, e.Status, e.ArchiveID, e.ApplyJobID)
}

func (e *NodeConversionAbortedError) Unwrap() error { return ErrNodeConversionAborted }

// NodeConversionDeniedError retains the transition failure without exposing a
// different top-level outcome to callers.
type NodeConversionDeniedError struct {
	Cause error
}

func (e *NodeConversionDeniedError) Error() string {
	if e == nil || e.Cause == nil {
		return ErrNodeConversionDenied.Error()
	}
	return fmt.Sprintf("%s: %v", ErrNodeConversionDenied, e.Cause)
}

func (e *NodeConversionDeniedError) Unwrap() error { return ErrNodeConversionDenied }

// Convert archives and queues the validated envelope, waits for exact
// terminal apply success, and only then commits the local NODE role.
func (c *NodeConverter) Convert(ctx context.Context, in NodeConversionInput) (NodeConversionResult, error) {
	if c == nil || c.importer == nil || c.identity == nil || c.applyWaiter == nil || in.Envelope == nil {
		return NodeConversionResult{}, errNodeConversionUnavailable
	}
	if ctx == nil {
		return NodeConversionResult{}, context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return NodeConversionResult{}, err
	}

	imported, err := c.importer.Import(ctx, in.Envelope, replicationsnapshot.ImporterInput{
		LocalInstallationID: in.LocalInstallId,
		LocalNodeID:         in.LocalNodeID,
		LocalIngress:        in.LocalIngress,
		ArchiveTTL:          in.ArchiveTTL,
	})
	if err != nil {
		return NodeConversionResult{}, fmt.Errorf("node conversion import: %w", err)
	}
	c.logger("node conversion apply queued archive=%s job=%s", imported.ArchiveID, imported.ApplyJobID)

	terminal, err := configuration.WaitForTerminalApply(ctx, c.applyWaiter, imported.ApplyJobID, configuration.ApplyWaitOptions{
		Timeout: in.ApplyWaitTimeout,
		Clock:   c.now,
	})
	if err != nil {
		return NodeConversionResult{}, err
	}
	if terminal.Status != configuration.ApplyTerminalStatusApplied {
		c.logger("node conversion apply terminal status=%s archive=%s job=%s", terminal.Status, imported.ArchiveID, imported.ApplyJobID)
		return NodeConversionResult{}, &NodeConversionAbortedError{
			Status:     terminal.Status,
			ArchiveID:  imported.ArchiveID,
			ApplyJobID: imported.ApplyJobID,
		}
	}

	postTransition, err := c.identity.TransitionTo(ctx, domain.TopologyRoleNode)
	if err != nil {
		c.logger("node conversion identity transition denied archive=%s job=%s", imported.ArchiveID, imported.ApplyJobID)
		return NodeConversionResult{}, &NodeConversionDeniedError{Cause: err}
	}
	lineage := NodeLineage{
		NodeID:     postTransition.NodeID,
		Generation: uint64(postTransition.LeadershipGeneration),
		Role:       postTransition.Role,
	}
	if postTransition.ClusterKeyID != nil {
		clusterKeyID := *postTransition.ClusterKeyID
		lineage.ClusterKeyID = &clusterKeyID
	}
	c.logger("node conversion committed node=%s generation=%d", lineage.NodeID, lineage.Generation)
	return NodeConversionResult{
		ArchiveID:   imported.ArchiveID,
		ApplyJobID:  imported.ApplyJobID,
		Identity:    postTransition,
		NodeLineage: lineage,
	}, nil
}
