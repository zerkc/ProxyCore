package identity

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

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

// PgStore is the Postgres-backed Store implementation.
type PgStore struct {
	pool *pgxpool.Pool
}

// NewPgStore constructs a Postgres-backed identity store.
func NewPgStore(pool *pgxpool.Pool) *PgStore {
	return &PgStore{pool: pool}
}

const singletonID = "default"

const selectIdentitySQL = `
select installation_id, node_id, role, leadership_generation,
       latest_known_generation, cluster_key_id, updated_at
  from installation_identity
 where id = $1
`

func (s *PgStore) Get(ctx context.Context) (Identity, error) {
	row := s.pool.QueryRow(ctx, selectIdentitySQL, singletonID)
	var installationUUID, nodeUUID uuid.UUID
	var role string
	var leadershipGen, latestGen int64
	var clusterKeyID *uuid.UUID
	var updatedAt sql.NullTime
	if err := row.Scan(&installationUUID, &nodeUUID, &role, &leadershipGen, &latestGen, &clusterKeyID, &updatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Identity{}, ErrNotFound
		}
		return Identity{}, err
	}
	if !domain.TopologyRole(role).IsValid() {
		return Identity{}, errors.New("invalid role in installation_identity: " + role)
	}
	var updated time.Time
	if updatedAt.Valid {
		updated = updatedAt.Time
	}
	return Identity{
		InstallationID:        domain.InstallationID(installationUUID.String()),
		NodeID:                domain.NodeID(nodeUUID.String()),
		Role:                  domain.TopologyRole(role),
		LeadershipGeneration:  domain.LeadershipGeneration(leadershipGen),
		LatestKnownGeneration: domain.LeadershipGeneration(latestGen),
		ClusterKeyID:          clusterKeyID,
		UpdatedAt:             updated,
	}, nil
}

func (s *PgStore) Ensure(ctx context.Context, installationID domain.InstallationID, nodeID domain.NodeID) (Identity, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Identity{}, false, err
	}
	defer tx.Rollback(ctx)

	row := tx.QueryRow(ctx, selectIdentitySQL, singletonID)
	var installationUUID, nodeUUID uuid.UUID
	var role string
	var leadershipGen, latestGen int64
	var clusterKeyID *uuid.UUID
	var updatedAt sql.NullTime
	scanErr := row.Scan(&installationUUID, &nodeUUID, &role, &leadershipGen, &latestGen, &clusterKeyID, &updatedAt)
	if scanErr == nil {
		var updated time.Time
		if updatedAt.Valid {
			updated = updatedAt.Time
		}
		existing := Identity{
			InstallationID:        domain.InstallationID(installationUUID.String()),
			NodeID:                domain.NodeID(nodeUUID.String()),
			Role:                  domain.TopologyRole(role),
			LeadershipGeneration:  domain.LeadershipGeneration(leadershipGen),
			LatestKnownGeneration: domain.LeadershipGeneration(latestGen),
			ClusterKeyID:          clusterKeyID,
			UpdatedAt:             updated,
		}
		if err := tx.Commit(ctx); err != nil {
			return Identity{}, false, err
		}
		return existing, false, nil
	}
	if !errors.Is(scanErr, pgx.ErrNoRows) {
		return Identity{}, false, scanErr
	}

	parsedInstallation, err := uuid.Parse(string(installationID))
	if err != nil {
		return Identity{}, false, errors.New("invalid installation id: " + err.Error())
	}
	parsedNode, err := uuid.Parse(string(nodeID))
	if err != nil {
		return Identity{}, false, errors.New("invalid node id: " + err.Error())
	}
	if _, err := tx.Exec(ctx,
		`insert into installation_identity
		   (id, installation_id, node_id, role, leadership_generation, latest_known_generation, updated_at)
		 values ($1, $2, $3, 'standalone-primary', 1, 1, now())`,
		singletonID, parsedInstallation, parsedNode,
	); err != nil {
		return Identity{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Identity{}, false, err
	}
	return Identity{
		InstallationID:        installationID,
		NodeID:                nodeID,
		Role:                  domain.TopologyRoleStandalone,
		LeadershipGeneration:  domain.LeadershipGeneration(1),
		LatestKnownGeneration: domain.LeadershipGeneration(1),
		UpdatedAt:             time.Now().UTC(),
	}, true, nil
}

func (s *PgStore) UpdateRole(ctx context.Context, role domain.TopologyRole) error {
	if !role.IsValid() {
		return errors.New("invalid role: " + string(role))
	}
	tag, err := s.pool.Exec(ctx,
		`update installation_identity set role = $2, updated_at = now() where id = $1`,
		singletonID, string(role),
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *PgStore) UpdateLeadershipGeneration(ctx context.Context, gen domain.LeadershipGeneration) error {
	tag, err := s.pool.Exec(ctx,
		`update installation_identity
		    set leadership_generation = $2,
		        latest_known_generation = greatest(latest_known_generation, $2),
		        updated_at = now()
		  where id = $1`,
		singletonID, int64(gen),
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *PgStore) UpdateLatestKnownGeneration(ctx context.Context, gen domain.LeadershipGeneration) error {
	tag, err := s.pool.Exec(ctx,
		`update installation_identity
		    set latest_known_generation = greatest(latest_known_generation, $2),
		        updated_at = now()
		  where id = $1`,
		singletonID, int64(gen),
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *PgStore) UpdateClusterKeyID(ctx context.Context, keyID *uuid.UUID) error {
	var arg interface{}
	if keyID != nil {
		arg = *keyID
	}
	tag, err := s.pool.Exec(ctx,
		`update installation_identity set cluster_key_id = $2, updated_at = now() where id = $1`,
		singletonID, arg,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
