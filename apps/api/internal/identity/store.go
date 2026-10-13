package identity

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
)

// ErrNotFound is returned when the installation_identity row is missing.
// Callers normally react to this by calling Ensure to bootstrap a fresh
// installation.
var ErrNotFound = errors.New("installation identity not found")

// Store is the persistence port for installation_identity.
//
// Implementations MUST treat the row keyed by the literal id "default" as a
// singleton; concurrent first-boot callers MUST converge on a single row.
type Store interface {
	// Get returns the current identity. Returns ErrNotFound when no row
	// exists yet.
	Get(ctx context.Context) (Identity, error)
	// Ensure returns the existing identity when present, otherwise inserts
	// a fresh identity with the given installation ID, node ID, and
	// leadership generation 1, role standalone-primary. The created
	// boolean is true when the row was inserted by this call.
	Ensure(ctx context.Context, installationID domain.InstallationID, nodeID domain.NodeID) (Identity, bool, error)
	// UpdateRole persists a new role. Returns ErrNotFound when no row
	// exists yet.
	UpdateRole(ctx context.Context, role domain.TopologyRole) error
	// UpdateLeadershipGeneration persists the new leadership generation
	// and bumps latest_known_generation when the new value is greater.
	UpdateLeadershipGeneration(ctx context.Context, gen domain.LeadershipGeneration) error
	// UpdateLatestKnownGeneration persists a new latest-known generation
	// when greater than the stored value; no-op otherwise.
	UpdateLatestKnownGeneration(ctx context.Context, gen domain.LeadershipGeneration) error
	// UpdateClusterKeyID persists the active cluster KEK id.
	UpdateClusterKeyID(ctx context.Context, keyID *uuid.UUID) error
}

// singletonID is the installation_identity row key; the SQLite store uses it.
const singletonID = "default"
