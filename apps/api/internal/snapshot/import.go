package snapshot

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
)

// ArchiveStore persists the prior standalone desired state when an
// installation transitions to NODE. The archive carries an explicit TTL
// so the retention worker can purge it after the configured period.
type ArchiveStore interface {
	// SaveStandaloneArchive persists the local desired state alongside the
	// 30-day TTL metadata. The call MUST be transactional with the
	// desired-state write so a partially-applied import is impossible.
	SaveStandaloneArchive(ctx context.Context, archive StandaloneArchive) error
	// PurgeExpiredArchives removes archives whose expires_at is in the
	// past. The retention worker calls this on its schedule.
	PurgeExpiredArchives(ctx context.Context, now time.Time) (int, error)
}

// StandaloneArchive is the durable record of an installation's pre-NODE
// desired state. The TTL field is preserved as part of the archive so
// downstream retention workers can purge without consulting external
// configuration.
type StandaloneArchive struct {
	ID           uuid.UUID
	CapturedAt   time.Time
	ExpiresAt    time.Time
	Reason       string
	SnapshotData []byte
}

// ApplyJobEnqueuer is the minimal port the importer needs to schedule an
// apply job from the imported snapshot. Phase 0 uses the existing
// configuration.Store.CreateApplyJob surface; Phase 2 will introduce a
// typed apply pipeline that records source: 'import'.
type ApplyJobEnqueuer interface {
	CreateApplyJobFromSnapshot(ctx context.Context, sourceLabel string, snapshotBytes []byte) (uuid.UUID, error)
}

// ImporterInput carries the local identity and TTL configuration the
// importer applies when transitioning to NODE.
type ImporterInput struct {
	LocalInstallationID domain.InstallationID
	LocalNodeID         domain.NodeID
	LocalIngress        domain.Ingress
	ArchiveTTL          time.Duration
	ArchiveReason       string
}

// ArchiveTTLDefault is the default standalone-archive retention period
// when the operator does not override it. The PRD resolves to 30 days.
const ArchiveTTLDefault = 30 * 24 * time.Hour

// Importer consumes a sealed Envelope (already validated) and:
//  1. Captures the local desired state into the standalone archive with
//     a 30-day TTL.
//  2. Writes the imported desired state as the new current revision.
//  3. Enqueues an apply job tagged source: 'import' so the audit trail
//     records the provenance of the change.
type Importer struct {
	archive   ArchiveStore
	enqueuer  ApplyJobEnqueuer
	now       func() time.Time
}

// NewImporter constructs an Importer over the given ports. The now hook is
// injectable so tests can pin the captured-at timestamp without mocking
// the clock.
func NewImporter(archive ArchiveStore, enqueuer ApplyJobEnqueuer, now func() time.Time) *Importer {
	if now == nil {
		now = time.Now
	}
	return &Importer{archive: archive, enqueuer: enqueuer, now: now}
}

// ImporterResult is the outcome of an import. The ArchiveID and
// ApplyJobID are returned so the API surface can show the operator what
// was archived and what apply was scheduled.
type ImporterResult struct {
	ArchiveID  uuid.UUID
	ApplyJobID uuid.UUID
}

// Import runs the archive-and-apply sequence. The envelope MUST have been
// validated by the Validator before this call; Import does not re-run
// structural or secret checks because the validator is the gate.
//
// The envelope is taken by pointer so the caller can observe the
// node-local rewrite and the re-anchored content hash that Import
// performs before persisting.
func (i *Importer) Import(ctx context.Context, env *Envelope, in ImporterInput) (ImporterResult, error) {
	if in.ArchiveTTL == 0 {
		in.ArchiveTTL = ArchiveTTLDefault
	}
	if in.ArchiveReason == "" {
		in.ArchiveReason = "transition-to-node"
	}
	if err := i.replaceNodeLocalFields(env, in); err != nil {
		return ImporterResult{}, fmt.Errorf("importer: rewrite node-local fields: %w", err)
	}
	capturedAt := i.now().UTC()
	archive := StandaloneArchive{
		ID:           uuid.New(),
		CapturedAt:   capturedAt,
		ExpiresAt:    capturedAt.Add(in.ArchiveTTL),
		Reason:       in.ArchiveReason,
		SnapshotData: mustMarshalEnvelope(*env),
	}
	if err := i.archive.SaveStandaloneArchive(ctx, archive); err != nil {
		return ImporterResult{}, fmt.Errorf("importer: save archive: %w", err)
	}
	jobID, err := i.enqueuer.CreateApplyJobFromSnapshot(ctx, "import", archive.SnapshotData)
	if err != nil {
		return ImporterResult{}, fmt.Errorf("importer: enqueue apply job: %w", err)
	}
	return ImporterResult{ArchiveID: archive.ID, ApplyJobID: jobID}, nil
}

// replaceNodeLocalFields rewrites NodeLocalFields with the locally
// authoritative values. The importer MUST do this so a hostile or
// mistaken producer cannot redirect the node's data plane ingress.
func (i *Importer) replaceNodeLocalFields(env *Envelope, in ImporterInput) error {
	if !in.LocalNodeID.IsValid() {
		return errors.New("importer: local node id is invalid")
	}
	env.NodeLocal.Ingress = in.LocalIngress
	env.NodeLocal.NodeID = in.LocalNodeID
	// Role is updated to reflect the new topology; the importer runs as the
	// receiving node, so its role transitions to TopologyRoleNode unless
	// the caller is performing a promote-to-primary at the same time.
	if !env.NodeLocal.Role.IsValid() {
		env.NodeLocal.Role = domain.TopologyRoleNode
	}
	// Re-anchor the content hash because NodeLocal is excluded from the
	// canonical serialization; this is defensive in case the importer is
	// called without a prior Validate that already re-anchored.
	hash, err := env.ExpectedHash()
	if err != nil {
		return err
	}
	env.ContentHash = hash
	return nil
}

func mustMarshalEnvelope(env Envelope) []byte {
	data, err := Marshal(env)
	if err != nil {
		panic("importer: marshal envelope: " + err.Error())
	}
	return data
}
