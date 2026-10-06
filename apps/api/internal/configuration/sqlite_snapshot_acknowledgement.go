package configuration

import (
	"context"
	"database/sql"
	"errors"
	"reflect"

	"github.com/google/uuid"
	"github.com/zerkc/ProxyCore/apps/api/internal/cluster"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

// sqliteSnapshotAcknowledgementKeys cannot start a second transaction or use
// an independent read. The active row is checked before requesting material.
type sqliteSnapshotAcknowledgementKeys interface {
	LoadOrCreateInWrite(sqlitedb.WriteValue, *uuid.UUID, *cluster.KeyCandidate) (cluster.KeyMaterial, error)
}

// SQLiteSnapshotAcknowledgementStore composes the unchanged tuple reader and
// writes complete, already-authenticated tuples. It is not an HTTP authorizer.
// The caller owns the database and writer lifecycle.
type SQLiteSnapshotAcknowledgementStore struct {
	*SQLiteSnapshotAcknowledgementReadStore
	writer *sqlitedb.Writer
	keys   sqliteSnapshotAcknowledgementKeys
}

func NewSQLiteSnapshotAcknowledgementStore(db *sql.DB, writer *sqlitedb.Writer, keys sqliteSnapshotAcknowledgementKeys) *SQLiteSnapshotAcknowledgementStore {
	return &SQLiteSnapshotAcknowledgementStore{NewSQLiteSnapshotAcknowledgementReadStore(db), writer, keys}
}

func (s *SQLiteSnapshotAcknowledgementStore) RecordSnapshotAcknowledgement(ctx context.Context, ack SnapshotAcknowledgement) error {
	// Validation precedes dependency checks, including on a nil receiver.
	if ctx == nil || !validSnapshotAcknowledgement(ack) {
		return ErrSnapshotAcknowledgementDenied
	}
	if s == nil || s.SQLiteSnapshotAcknowledgementReadStore == nil || s.db == nil || s.writer == nil || sqliteAckKeysNil(s.keys) {
		return ErrSnapshotAcknowledgementUnavailable
	}
	err := s.writer.Write(ctx, func(v sqlitedb.WriteValue) error {
		credential, err := sqliteAckCredential(v, ack.NodeID)
		if err != nil {
			return err
		}
		identity, err := sqliteAckIdentity(v)
		if err != nil {
			return err
		}
		if !readySnapshotPublicationIdentity(identity) || credential.primary != identity.InstallationID || credential.generation != identity.LeadershipGeneration || identity.ClusterKeyID == nil || !credential.lineage.Valid || credential.lineage.String != identity.ClusterKeyID.String() {
			return ErrSnapshotAcknowledgementDenied
		}
		var active bool
		if err := v.Conn().QueryRowContext(v.Context(), `select exists(select 1 from cluster_keys where id = ? and retired_at is null)`, identity.ClusterKeyID.String()).Scan(&active); err != nil {
			return ErrSnapshotAcknowledgementUnavailable
		}
		if !active {
			return ErrSnapshotAcknowledgementDenied
		}
		material, err := s.keys.LoadOrCreateInWrite(v, identity.ClusterKeyID, nil)
		// Also destroy any material returned alongside an error.
		defer material.Destroy()
		if err != nil {
			return ErrSnapshotAcknowledgementUnavailable
		}
		materialID := material.ID
		keyBytes := material.CopyBytes()
		defer clear(keyBytes)
		material.Destroy()
		if materialID == uuid.Nil || materialID != *identity.ClusterKeyID {
			return ErrSnapshotAcknowledgementUnavailable
		}
		kek, err := cluster.NewKEK(keyBytes)
		if err != nil {
			return ErrSnapshotAcknowledgementUnavailable
		}
		kek.Destroy()
		derived, found, err := sqliteAckCandidate(v, ack, identity)
		if err != nil {
			return ErrSnapshotAcknowledgementUnavailable
		}
		if !found || derived.SnapshotVersion != ack.SnapshotVersion || derived.ReplicationVersion != ack.ReplicationVersion || derived.RevisionID != ack.RevisionID || derived.LeadershipGeneration != ack.LeadershipGeneration {
			return ErrSnapshotAcknowledgementDenied
		}
		applied, err := sqliteEnrollmentTokenTime(ack.AppliedAt)
		if err != nil {
			return ErrSnapshotAcknowledgementUnavailable
		}
		// Deliberate PG divergence: this clock was captured AFTER BEGIN IMMEDIATE,
		// not before waiting for the writer. Caller clocks remain historical input.
		received := v.Timestamp()
		if !ack.ReceivedAt.IsZero() {
			received, err = sqliteEnrollmentTokenTime(ack.ReceivedAt)
			if err != nil {
				return ErrSnapshotAcknowledgementUnavailable
			}
		}
		_, err = v.Conn().ExecContext(v.Context(), `insert into node_snapshot_acks
   (node_id,content_hash,snapshot_version,replication_version,revision_id,leadership_generation,applied_at,received_at)
   values (?,?,?,?,?,?,?,?)
   on conflict (node_id,content_hash) do update set
   snapshot_version=excluded.snapshot_version, replication_version=excluded.replication_version,
   revision_id=excluded.revision_id, leadership_generation=excluded.leadership_generation,
   applied_at=excluded.applied_at, received_at=excluded.received_at`,
			ack.NodeID, ack.ContentHash, ack.SnapshotVersion, ack.ReplicationVersion,
			ack.RevisionID, ack.LeadershipGeneration, applied, received)
		if err != nil {
			return ErrSnapshotAcknowledgementUnavailable
		}
		return nil
	})
	// Commit and rollback cleanup failures are unavailable unless the original
	// transaction rejection is a recognized authorization sentinel.
	if err == nil {
		return nil
	}
	return mapSnapshotAcknowledgementCredentialError(err)
}

func sqliteAckKeysNil(keys sqliteSnapshotAcknowledgementKeys) bool {
	if keys == nil {
		return true
	}
	v := reflect.ValueOf(keys)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	}
	return false
}

type sqliteAckCredentialRecord struct {
	primary    string
	generation int64
	lineage    sql.NullString
}

func sqliteAckCredential(v sqlitedb.WriteValue, node string) (sqliteAckCredentialRecord, error) {
	var record sqliteAckCredentialRecord
	var id, nodeID, hash, version string
	var credentialRevoked, nodeRevoked, tokenRevoked, consumed sql.NullString
	err := v.Conn().QueryRowContext(v.Context(), `select c.id,c.node_id,c.credential_hash,c.hash_version,
  c.revoked_at,n.revoked_at,t.revoked_at,t.consumed_at,n.primary_id,g.primary_generation,a.cluster_key_id
  from node_credentials c
  join enrolled_nodes n on n.credential_id=c.id and n.node_id=c.node_id
  join enrollment_grants g on g.attempt_id=n.created_by_attempt_id and
   g.node_id=n.node_id and g.installation_id=n.installation_id and g.primary_id=n.primary_id
  join enrollment_attempts a on a.id=g.attempt_id
  join enrollment_tokens t on t.id=g.token_id
  where c.node_id=?`, node).Scan(&id, &nodeID, &hash, &version,
		&credentialRevoked, &nodeRevoked, &tokenRevoked, &consumed, &record.primary, &record.generation, &record.lineage)
	if errors.Is(err, sql.ErrNoRows) {
		return record, ErrSnapshotAcknowledgementDenied
	}
	if err != nil {
		return record, ErrSnapshotAcknowledgementUnavailable
	}
	if credentialRevoked.Valid || nodeRevoked.Valid || tokenRevoked.Valid {
		return record, ErrSnapshotAcknowledgementRevoked
	}
	// Consumed tokens are valid credential lineage, not revocation.
	return record, nil
}

func sqliteAckIdentity(v sqlitedb.WriteValue) (SnapshotPublicationIdentityRecord, error) {
	var record SnapshotPublicationIdentityRecord
	var role string
	var key sql.NullString
	err := v.Conn().QueryRowContext(v.Context(), `select installation_id,node_id,role,
  leadership_generation,latest_known_generation,cluster_key_id
  from installation_identity where id=?`, installationIDSingleton).Scan(
		&record.InstallationID, &record.NodeID, &role, &record.LeadershipGeneration, &record.LatestKnownGeneration, &key)
	if errors.Is(err, sql.ErrNoRows) {
		return record, ErrSnapshotAcknowledgementDenied
	}
	if err != nil {
		return record, ErrSnapshotAcknowledgementUnavailable
	}
	record.Role = domain.TopologyRole(role)
	if key.Valid {
		id, err := uuid.Parse(key.String)
		if err != nil || id.String() != key.String {
			return record, ErrSnapshotAcknowledgementUnavailable
		}
		record.ClusterKeyID = &id
	}
	return record, nil
}

func sqliteAckCandidate(v sqlitedb.WriteValue, input SnapshotAcknowledgement, identity SnapshotPublicationIdentityRecord) (SnapshotAcknowledgement, bool, error) {
	rows, err := v.Conn().QueryContext(v.Context(), `select a.source_primary_id,a.leadership_generation,
  a.snapshot_version,a.replication_version,a.revision_id,a.applied_at
  from applied_snapshots a
  join config_revisions r on r.id=a.revision_id
  join apply_jobs j on j.id=a.apply_job_id
  where a.content_hash=? and a.status='applied'
   and a.snapshot_body is not null and length(a.snapshot_body)>0
   and a.discarded_at is null and a.source_primary_id=? and a.leadership_generation=?
   and r.source='ordinary' and r.source_primary_id=a.source_primary_id
   and r.source_node_id is null and r.source_revision_id is null
   and r.snapshot_content_hash=a.content_hash and r.snapshot_version=a.snapshot_version
   and r.replication_version=a.replication_version and r.leadership_generation=a.leadership_generation
   and r.applied_at is not null and j.revision_id=r.id and j.status='applied'
   and j.finished_at is not null and j.source='ordinary' and j.source_node_id is null
   and j.source_revision_id is null and j.source_primary_id=a.source_primary_id
   and j.snapshot_content_hash=a.content_hash and j.snapshot_version=a.snapshot_version
   and j.replication_version=a.replication_version and j.leadership_generation=a.leadership_generation
  order by a.applied_at desc,a.id desc`, input.ContentHash, identity.InstallationID, identity.LeadershipGeneration)
	if err != nil {
		return SnapshotAcknowledgement{}, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var candidate SnapshotAcknowledgement
		var primary, stamp string
		if err := rows.Scan(&primary, &candidate.LeadershipGeneration, &candidate.SnapshotVersion, &candidate.ReplicationVersion, &candidate.RevisionID, &stamp); err != nil {
			return SnapshotAcknowledgement{}, false, err
		}
		applied, err := sqliteEnqueueTime(&stamp)
		if err != nil || applied == nil {
			return SnapshotAcknowledgement{}, false, ErrSnapshotAcknowledgementUnavailable
		}
		if primary == identity.InstallationID && applied.Equal(input.AppliedAt) {
			if candidate.SnapshotVersion <= 0 || candidate.ReplicationVersion <= 0 || !validPublicationUUID(candidate.RevisionID) {
				return SnapshotAcknowledgement{}, false, nil
			}
			candidate.AppliedAt = *applied
			return candidate, true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return SnapshotAcknowledgement{}, false, err
	}
	return SnapshotAcknowledgement{}, false, nil
}

var _ SnapshotAcknowledgementStore = (*SQLiteSnapshotAcknowledgementStore)(nil)
