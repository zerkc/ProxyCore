package identity

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

var _ TopologyStore = (*SQLiteStore)(nil)

const (
	auditActionPromote = "topology.promote"
	auditActionDemote  = "topology.demote"
)

// PromoteTopology applies the whole promotion in one writer transaction. The
// source role and generation are re-checked under the write lock so a racing
// import or promotion cannot be overwritten.
//
// The cluster key is rotated lazily: every active key is retired and
// cluster_key_id is cleared, so the next enrollment or publication mints a
// fresh KEK through the existing LoadOrCreate path.
func (s *SQLiteStore) PromoteTopology(ctx context.Context, in PromoteInput) (TopologyChange, error) {
	if err := s.ready(ctx, true); err != nil {
		return TopologyChange{}, err
	}
	var change TopologyChange
	err := s.writer.Write(ctx, func(v sqlitedb.WriteValue) error {
		current, err := sqliteScanIdentity(v.Conn().QueryRowContext(v.Context(), sqliteSelectIdentity, singletonID))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if !promotableFrom(current.Role) || current.Role != in.FromRole {
			return fmt.Errorf("%w: cannot promote from %s", ErrInvalidTransition, current.Role)
		}
		if current.LeadershipGeneration != in.FromGeneration {
			return fmt.Errorf("%w: expected %d, current %d", ErrGenerationMismatch, in.FromGeneration, current.LeadershipGeneration)
		}
		if in.NextGeneration <= current.LatestKnownGeneration {
			return fmt.Errorf("%w: next generation %d is not above latest known %d",
				ErrGenerationMismatch, in.NextGeneration, current.LatestKnownGeneration)
		}
		if _, err := v.Conn().ExecContext(v.Context(),
			`update installation_identity
			    set role = ?, leadership_generation = ?,
			        latest_known_generation = max(latest_known_generation, ?),
			        cluster_key_id = null, updated_at = ?
			  where id = ?`,
			string(domain.TopologyRolePrimary), int64(in.NextGeneration), int64(in.NextGeneration), v.Timestamp(), singletonID,
		); err != nil {
			return err
		}
		revoked, err := sqliteRetireClusterRelationships(v)
		if err != nil {
			return err
		}
		if _, err := v.Conn().ExecContext(v.Context(),
			`update node_state
			    set enrolled_at = null, enrollment_token_hash = null, enrollment_primary_id = null,
			        primary_url = null, primary_installation_id = null, primary_tls_spki_sha256 = null,
			        credential_id = null, sync_enabled = 0, next_attempt_at = null,
			        consecutive_failures = 0, last_error_code = null, updated_at = ?`,
			v.Timestamp(),
		); err != nil {
			return err
		}
		if err := sqliteInsertTopologyAudit(v, auditActionPromote, in.ActorUserID, current, map[string]any{
			"previousRole":       string(current.Role),
			"previousGeneration": int64(current.LeadershipGeneration),
			"role":               string(domain.TopologyRolePrimary),
			"generation":         int64(in.NextGeneration),
			"revokedCredentials": revoked,
			"clusterKeyRotated":  true,
		}); err != nil {
			return err
		}
		change = TopologyChange{
			Role: domain.TopologyRolePrimary, Generation: in.NextGeneration, RevokedCredentials: revoked,
		}
		return nil
	})
	if err != nil {
		return TopologyChange{}, err
	}
	return change, nil
}

// DemoteTopology revokes every credential this primary issued, retires its
// cluster key, and moves the installation to NODE in one writer transaction.
// The leadership generation is unchanged.
func (s *SQLiteStore) DemoteTopology(ctx context.Context, in DemoteInput) (TopologyChange, error) {
	if err := s.ready(ctx, true); err != nil {
		return TopologyChange{}, err
	}
	var change TopologyChange
	err := s.writer.Write(ctx, func(v sqlitedb.WriteValue) error {
		current, err := sqliteScanIdentity(v.Conn().QueryRowContext(v.Context(), sqliteSelectIdentity, singletonID))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if (current.Role != domain.TopologyRolePrimary && current.Role != domain.TopologyRolePrimaryWithNodes) ||
			current.Role != in.FromRole {
			return fmt.Errorf("%w: cannot demote from %s", ErrInvalidTransition, current.Role)
		}
		if current.LeadershipGeneration != in.FromGeneration {
			return fmt.Errorf("%w: expected %d, current %d", ErrGenerationMismatch, in.FromGeneration, current.LeadershipGeneration)
		}
		revoked, err := sqliteRetireClusterRelationships(v)
		if err != nil {
			return err
		}
		if _, err := v.Conn().ExecContext(v.Context(),
			`update installation_identity set role = ?, cluster_key_id = null, updated_at = ? where id = ?`,
			string(domain.TopologyRoleNode), v.Timestamp(), singletonID,
		); err != nil {
			return err
		}
		if err := sqliteInsertTopologyAudit(v, auditActionDemote, in.ActorUserID, current, map[string]any{
			"previousRole":       string(current.Role),
			"role":               string(domain.TopologyRoleNode),
			"generation":         int64(current.LeadershipGeneration),
			"revokedCredentials": revoked,
		}); err != nil {
			return err
		}
		change = TopologyChange{
			Role: domain.TopologyRoleNode, Generation: current.LeadershipGeneration, RevokedCredentials: revoked,
		}
		return nil
	})
	if err != nil {
		return TopologyChange{}, err
	}
	return change, nil
}

// sqliteRetireClusterRelationships revokes every active node credential and
// enrolled node and retires every active cluster key. It returns the number of
// credentials newly revoked.
func sqliteRetireClusterRelationships(v sqlitedb.WriteValue) (int, error) {
	res, err := v.Conn().ExecContext(v.Context(),
		`update node_credentials set revoked_at = ? where revoked_at is null`, v.Timestamp())
	if err != nil {
		return 0, err
	}
	revoked, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if _, err := v.Conn().ExecContext(v.Context(),
		`update enrolled_nodes set revoked_at = ? where revoked_at is null`, v.Timestamp()); err != nil {
		return 0, err
	}
	if _, err := v.Conn().ExecContext(v.Context(),
		`update cluster_keys set retired_at = ? where retired_at is null`, v.Timestamp()); err != nil {
		return 0, err
	}
	return int(revoked), nil
}

func sqliteInsertTopologyAudit(v sqlitedb.WriteValue, action, actorUserID string, subject Identity, after map[string]any) error {
	payload, err := json.Marshal(after)
	if err != nil {
		return err
	}
	id, err := v.NewUUID()
	if err != nil {
		return err
	}
	correlation, err := v.NewUUID()
	if err != nil {
		return err
	}
	var actor any
	if actorUserID != "" {
		actor = actorUserID
	}
	_, err = v.Conn().ExecContext(v.Context(), `insert into audit_events
		(id, actor_user_id, action, resource_type, resource_id, before_value, after_value, correlation_id, result, created_at)
		values (?, ?, ?, 'installation_identity', ?, null, ?, ?, 'success', ?)`,
		id, actor, action, string(subject.InstallationID), string(payload), correlation, v.Timestamp())
	return err
}
