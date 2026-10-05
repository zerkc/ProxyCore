package main

import (
	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
	"github.com/zerkc/ProxyCore/apps/api/internal/enrollment"
	replicationsnapshot "github.com/zerkc/ProxyCore/apps/api/internal/snapshot"
)

// nodeConverterStore is what buildNodeConverter needs from the configuration
// backend: the standalone archive, the snapshot-import apply enqueuer and the
// apply-job terminal reader.
type nodeConverterStore interface {
	replicationsnapshot.ArchiveStore
	replicationsnapshot.ApplyJobEnqueuer
	configuration.TerminalApplyReader
}

// phase2Backend is the enrollment/snapshot persistence composition main wires
// into the token authority, publication, by-token, acknowledgement and
// canonical-snapshot producer consumers.
type phase2Backend interface {
	enrollment.Store
	configuration.SnapshotPublicationStore
	configuration.SnapshotAcknowledgementStore
	configuration.CanonicalSnapshotStore
	snapshotByTokenStoreReader
}

var (
	_ nodeConverterStore              = (*configuration.Store)(nil)
	_ phase2Backend                   = (*configuration.PgPhase2Store)(nil)
	_ enrollmentTLSConfigurationStore = (*configuration.Store)(nil)
)
