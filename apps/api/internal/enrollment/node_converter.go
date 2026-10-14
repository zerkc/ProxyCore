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
	// ClusterKeyID is the cluster KEK delivered by the accepted grant. It is
	// recorded on the identity at the commit point so LocalKEKLoader resolves
	// it after conversion. Nil only moves the role.
	ClusterKeyID *uuid.UUID
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

// CanEnrollAsNode reports whether the live identity may currently enroll as a
// NODE (including an unlinked NODE re-enrolling). The confirm handler asks it
// when its own role/key rule refuses.
func (c *NodeConverter) CanEnrollAsNode() bool {
	return c != nil && c.identity != nil && c.identity.Current().CanEnrollAsNode()
}

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

	// Defence in depth behind the confirm handler's per-request gate: refuse
	// an ineligible identity before archiving or applying anything.
	if !c.identity.Current().CanEnrollAsNode() {
		return NodeConversionResult{}, &NodeConversionDeniedError{Cause: identity.ErrEnrollmentIneligible}
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

	postTransition, err := c.identity.CommitNodeEnrollment(ctx, in.ClusterKeyID, in.Envelope.Transient.LeadershipGeneration)
	if err != nil {
		c.logger("node conversion identity transition denied archive=%s job=%s", imported.ArchiveID, imported.ApplyJobID)
		return NodeConversionResult{}, &NodeConversionDeniedError{Cause: err}
	}
	// The node is linked again: drop the unlinked overlay that allowed this
	// re-enrollment so a linked node is refused like any other.
	c.identity.SetNodeUnlinked(false)
	postTransition.NodeUnlinked = false
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

// NodeResyncInput is one continuous-sync import for an installation that is
// already a NODE of the primary that produced the envelope.
type NodeResyncInput struct {
	Envelope         *replicationsnapshot.Envelope
	LocalNodeID      domain.NodeID
	LocalIngress     domain.Ingress
	LocalInstallId   domain.InstallationID
	ArchiveTTL       time.Duration
	ApplyWaitTimeout time.Duration
}

// NodeResyncResult identifies the archive and apply job of one resync.
type NodeResyncResult struct {
	ArchiveID  uuid.UUID
	ApplyJobID uuid.UUID
}

// nodeSyncArchiveReason labels archives written by continuous sync.
const nodeSyncArchiveReason = "node-sync"

// nodeSyncArchiveTTL keeps routine sync archives short-lived; the enrollment
// archive (the pre-enrollment standalone configuration) keeps its own TTL.
const nodeSyncArchiveTTL = 7 * 24 * time.Hour

// Resync runs the same archive/import/materialize/apply path as Convert for an
// installation that is already a NODE, and returns only when the apply reached
// a terminal success. It never touches the identity: role, generation and
// cluster key were fixed at enrollment, and a primary that changed generation
// needs a fresh enrollment, not a silent re-anchor.
func (c *NodeConverter) Resync(ctx context.Context, in NodeResyncInput) (NodeResyncResult, error) {
	if c == nil || c.importer == nil || c.identity == nil || c.applyWaiter == nil || in.Envelope == nil {
		return NodeResyncResult{}, errNodeConversionUnavailable
	}
	if ctx == nil {
		return NodeResyncResult{}, context.Canceled
	}
	if err := ctx.Err(); err != nil {
		return NodeResyncResult{}, err
	}
	current := c.identity.Current()
	if current.Role != domain.TopologyRoleNode || current.ClusterKeyID == nil {
		return NodeResyncResult{}, &NodeConversionDeniedError{Cause: identity.ErrEnrollmentIneligible}
	}
	if in.ArchiveTTL <= 0 {
		in.ArchiveTTL = nodeSyncArchiveTTL
	}
	imported, err := c.importer.Import(ctx, in.Envelope, replicationsnapshot.ImporterInput{
		LocalInstallationID: in.LocalInstallId,
		LocalNodeID:         in.LocalNodeID,
		LocalIngress:        in.LocalIngress,
		ArchiveTTL:          in.ArchiveTTL,
		ArchiveReason:       nodeSyncArchiveReason,
	})
	if err != nil {
		return NodeResyncResult{}, fmt.Errorf("node resync import: %w", err)
	}
	terminal, err := configuration.WaitForTerminalApply(ctx, c.applyWaiter, imported.ApplyJobID, configuration.ApplyWaitOptions{
		Timeout: in.ApplyWaitTimeout,
		Clock:   c.now,
	})
	if err != nil {
		return NodeResyncResult{}, err
	}
	if terminal.Status != configuration.ApplyTerminalStatusApplied {
		c.logger("node resync apply terminal status=%s job=%s", terminal.Status, imported.ApplyJobID)
		return NodeResyncResult{}, &NodeConversionAbortedError{
			Status: terminal.Status, ArchiveID: imported.ArchiveID, ApplyJobID: imported.ApplyJobID,
		}
	}
	return NodeResyncResult{ArchiveID: imported.ArchiveID, ApplyJobID: imported.ApplyJobID}, nil
}
