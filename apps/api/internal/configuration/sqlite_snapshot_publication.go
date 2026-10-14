package configuration

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/cluster"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	replicationsnapshot "github.com/zerkc/ProxyCore/apps/api/internal/snapshot"
	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

// sqliteSnapshotPublicationContract mirrors the two PgPhase2Store reads consumed
// by sync.Publisher (via SnapshotPublicationStore) and by
// httpserver.SnapshotByTokenStore. httpserver and sync import this package, so
// the by-token assertion against the real interface cannot live here.
type sqliteSnapshotPublicationContract interface {
	SnapshotPublicationStore
	ReadSnapshotPublicationByToken(context.Context, string, SnapshotPublicationIdentityRecord) ([]byte, error)
}

var (
	_ sqliteSnapshotPublicationContract = (*SQLiteSnapshotPublicationStore)(nil)
)

// SQLiteSnapshotPublicationStore is the unwired SQLite counterpart of the
// PostgreSQL snapshot publication reads. Every read runs inside one writer
// transaction (BEGIN IMMEDIATE), which provides the serialization the PG row
// locks give: credential, token and identity changes cannot interleave with
// the proof check and the body copy. The caller owns the writer and key store.
type SQLiteSnapshotPublicationStore struct {
	writer *sqlitedb.Writer
	keys   *cluster.SQLiteKeyStore
}

func NewSQLiteSnapshotPublicationStore(writer *sqlitedb.Writer, keys *cluster.SQLiteKeyStore) *SQLiteSnapshotPublicationStore {
	return &SQLiteSnapshotPublicationStore{writer: writer, keys: keys}
}

// sqliteSnapshotProofSQL is the terminal-proof predicate shared by both reads.
// Missing joined rows make comparisons NULL; callers coalesce to false.
const sqliteSnapshotProofSQL = `a.status = 'applied'
	and a.snapshot_body is not null
	and a.discarded_at is null
	and r.id is not null
	and r.applied_at is not null
	and r.source = 'ordinary'
	and r.source_node_id is null
	and r.source_revision_id is null
	and r.source_primary_id = a.source_primary_id
	and r.snapshot_content_hash = a.content_hash
	and r.snapshot_version = a.snapshot_version
	and r.replication_version = a.replication_version
	and r.leadership_generation = a.leadership_generation
	and j.id is not null
	and j.revision_id = r.id
	and j.status = 'applied'
	and j.finished_at is not null
	and j.source = 'ordinary'
	and j.source_node_id is null
	and j.source_revision_id is null
	and j.source_primary_id = a.source_primary_id
	and j.snapshot_content_hash = a.content_hash
	and j.snapshot_version = a.snapshot_version
	and j.replication_version = a.replication_version
	and j.leadership_generation = a.leadership_generation
	and j.target = 'combined'`

func (s *SQLiteSnapshotPublicationStore) ready(ctx context.Context) bool {
	return s != nil && s.writer != nil && ctx != nil
}

// ReadSnapshotPublicationByToken mirrors PgPhase2Store: the caller has already
// authenticated the full credential, so only the selector is accepted. The
// token is never consumed.
func (s *SQLiteSnapshotPublicationStore) ReadSnapshotPublicationByToken(
	ctx context.Context,
	selector string,
	identity SnapshotPublicationIdentityRecord,
) ([]byte, error) {
	if !s.ready(ctx) {
		return nil, ErrSnapshotByTokenUnavailable
	}
	if selector == "" {
		return nil, ErrSnapshotByTokenUnauthenticated
	}
	var result []byte
	err := s.writer.Write(ctx, func(v sqlitedb.WriteValue) (err error) {
		result, err = s.readByToken(v, selector, identity)
		return err
	})
	if err != nil {
		for _, known := range []error{ErrSnapshotByTokenUnauthenticated, ErrSnapshotByTokenGone, ErrSnapshotByTokenDenied, ErrNoPublishableSnapshot} {
			if errors.Is(err, known) {
				return nil, known
			}
		}
		return nil, ErrSnapshotByTokenUnavailable
	}
	return result, nil
}

func (s *SQLiteSnapshotPublicationStore) readByToken(v sqlitedb.WriteValue, selector string, identity SnapshotPublicationIdentityRecord) ([]byte, error) {
	var hashVersion, expires string
	var consumed, revoked *string
	if err := v.Conn().QueryRowContext(v.Context(), `select hash_version, expires_at, consumed_at, revoked_at
		from enrollment_tokens where token_selector = ?`, selector).Scan(&hashVersion, &expires, &consumed, &revoked); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrSnapshotByTokenUnauthenticated
		}
		return nil, ErrSnapshotByTokenUnavailable
	}
	// Persisted clocks keep their tagged encoding: a corrupt key is unavailable,
	// and ordering of the canonical keys needs no calendar conversion.
	if _, err := sqlitedb.DecodeTimestamp(&expires); err != nil {
		return nil, ErrSnapshotByTokenUnavailable
	}
	now, err := sqliteEnrollmentTokenTime(time.Now().UTC())
	if err != nil {
		return nil, ErrSnapshotByTokenUnavailable
	}
	if hashVersion != EnrollmentTokenHashVersion || expires <= now {
		return nil, ErrSnapshotByTokenUnauthenticated
	}
	if consumed != nil || revoked != nil {
		return nil, ErrSnapshotByTokenGone
	}
	if !validSnapshotByTokenIdentity(identity) {
		return nil, ErrSnapshotByTokenDenied
	}
	if s.keys == nil {
		return nil, ErrSnapshotByTokenUnavailable
	}

	var activeText string
	if err := v.Conn().QueryRowContext(v.Context(), `select id from cluster_keys
		where retired_at is null order by created_at asc, id asc limit 1`).Scan(&activeText); err != nil {
		return nil, ErrSnapshotByTokenUnavailable
	}
	activeID, err := uuid.Parse(activeText)
	if err != nil {
		return nil, ErrSnapshotByTokenUnavailable
	}
	material, err := s.keys.LoadOrCreateInWrite(v, &activeID, nil)
	if err != nil {
		return nil, ErrSnapshotByTokenUnavailable
	}
	materialID := material.ID
	keyBytes := material.CopyBytes()
	material.Destroy()
	keyUsable := len(keyBytes) > 0
	zeroGrantBytes(keyBytes)
	if !keyUsable {
		return nil, ErrSnapshotByTokenUnavailable
	}
	if identity.ClusterKeyID == nil || materialID != *identity.ClusterKeyID {
		return nil, ErrSnapshotByTokenDenied
	}

	var body []byte
	var proofComplete int
	err = v.Conn().QueryRowContext(v.Context(), `select a.snapshot_body, coalesce((`+sqliteSnapshotProofSQL+`), 0)
		from applied_snapshots a
		left join config_revisions r on r.id = a.revision_id
		left join apply_jobs j on j.id = a.apply_job_id
		where a.source_primary_id = ? and a.leadership_generation = ?
		order by a.applied_at desc, a.id desc
		limit 1`, identity.InstallationID, identity.LeadershipGeneration).Scan(&body, &proofComplete)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoPublishableSnapshot
	}
	if err != nil {
		return nil, ErrSnapshotByTokenUnavailable
	}
	if proofComplete != 1 || len(body) == 0 {
		return nil, ErrNoPublishableSnapshot
	}
	return append([]byte(nil), body...), nil
}

// ReadSnapshotPublication mirrors PgPhase2Store: it authenticates the node
// credential and selects the absolute newest candidate for the primary in one
// transaction, denying an incomplete newest candidate instead of falling back.
// Every failure is the bare ErrSnapshotPublicationStore.
func (s *SQLiteSnapshotPublicationStore) ReadSnapshotPublication(ctx context.Context, request SnapshotPublicationRequest) (SnapshotPublicationResult, error) {
	if !s.ready(ctx) || request.Authenticate == nil || request.CredentialID == "" || request.PresentedCredential == "" ||
		request.MaxBytes <= 0 || request.MaxBytes > SnapshotPublicationMaxBytes {
		return SnapshotPublicationResult{}, ErrSnapshotPublicationStore
	}
	var result SnapshotPublicationResult
	if err := s.writer.Write(ctx, func(v sqlitedb.WriteValue) (err error) {
		result, err = s.readPublication(v, request)
		return err
	}); err != nil {
		return SnapshotPublicationResult{}, ErrSnapshotPublicationStore
	}
	return result, nil
}

func (s *SQLiteSnapshotPublicationStore) readPublication(v sqlitedb.WriteValue, request SnapshotPublicationRequest) (SnapshotPublicationResult, error) {
	deny := SnapshotPublicationResult{}
	credential, err := sqlitePublicationCredential(v, request.CredentialID)
	if err != nil {
		return deny, ErrSnapshotPublicationStore
	}
	principal, err := request.Authenticate(request.PresentedCredential, credential)
	if err != nil || principal.CredentialID != credential.ID || principal.NodeID != credential.NodeID {
		return deny, ErrSnapshotPublicationStore
	}
	if request.AfterContentHash != "" && !validSnapshotPublicationHash(request.AfterContentHash) {
		return deny, ErrSnapshotPublicationStore
	}
	identity, err := sqlitePublicationIdentity(v)
	if err != nil || !readySnapshotPublicationIdentity(identity) || credential.PrimaryID != identity.InstallationID {
		return deny, ErrSnapshotPublicationStore
	}
	if identity.ClusterKeyID == nil || s.keys == nil {
		return deny, ErrSnapshotPublicationStore
	}
	material, err := s.keys.LoadOrCreateInWrite(v, identity.ClusterKeyID, nil)
	if err != nil {
		return deny, ErrSnapshotPublicationStore
	}
	keyBytes := material.CopyBytes()
	material.Destroy()
	kek, err := cluster.NewKEK(keyBytes)
	zeroGrantBytes(keyBytes)
	if err != nil {
		return deny, ErrSnapshotPublicationStore
	}
	defer kek.Destroy()
	identity.ClusterKeyUsable = true

	candidate, size, err := sqliteLatestSnapshotPublication(v, credential.PrimaryID)
	if err != nil || size <= 0 || size > request.MaxBytes || !candidate.ProofComplete {
		return deny, ErrSnapshotPublicationStore
	}
	var raw []byte
	if err := v.Conn().QueryRowContext(v.Context(), `select snapshot_body from applied_snapshots where id = ?`,
		candidate.SnapshotID).Scan(&raw); err != nil || len(raw) == 0 || len(raw) != size || len(raw) > request.MaxBytes {
		return deny, ErrSnapshotPublicationStore
	}
	envelope, err := replicationsnapshot.Unmarshal(raw)
	if err != nil || !replicationsnapshot.NewValidator(kek).Validate(v.Context(), envelope).OK {
		return deny, ErrSnapshotPublicationStore
	}
	candidate.Bytes = append([]byte(nil), raw...)
	result := SnapshotPublicationResult{Principal: principal, Identity: identity, Snapshot: candidate}
	if request.AfterContentHash != "" && request.AfterContentHash == candidate.ContentHash {
		result.Current = true
		result.Snapshot.Bytes = nil
	}
	return result, nil
}

func sqlitePublicationTime(key *string) (*time.Time, error) {
	if key == nil {
		return nil, nil
	}
	return sqliteEnqueueTime(key)
}

func sqlitePublicationCredential(v sqlitedb.WriteValue, id string) (SnapshotPublicationCredentialRecord, error) {
	var record SnapshotPublicationCredentialRecord
	var credentialRevoked, nodeRevoked *string
	if err := v.Conn().QueryRowContext(v.Context(), `select c.id, c.node_id, c.credential_hash, c.hash_version,
			c.revoked_at, n.revoked_at, n.primary_id
		from node_credentials c
		join enrolled_nodes n on n.credential_id = c.id and n.node_id = c.node_id
		where c.id = ?`, id).Scan(&record.ID, &record.NodeID, &record.CredentialHash, &record.HashVersion,
		&credentialRevoked, &nodeRevoked, &record.PrimaryID); err != nil {
		return SnapshotPublicationCredentialRecord{}, err
	}
	var err error
	if record.CredentialRevokedAt, err = sqlitePublicationTime(credentialRevoked); err != nil {
		return SnapshotPublicationCredentialRecord{}, err
	}
	if record.NodeRevokedAt, err = sqlitePublicationTime(nodeRevoked); err != nil {
		return SnapshotPublicationCredentialRecord{}, err
	}
	return record, nil
}

func sqlitePublicationIdentity(v sqlitedb.WriteValue) (SnapshotPublicationIdentityRecord, error) {
	var record SnapshotPublicationIdentityRecord
	var role string
	var keyText *string
	if err := v.Conn().QueryRowContext(v.Context(), `select installation_id, node_id, role,
			leadership_generation, latest_known_generation, cluster_key_id
		from installation_identity where id = ?`, installationIDSingleton).Scan(&record.InstallationID, &record.NodeID, &role,
		&record.LeadershipGeneration, &record.LatestKnownGeneration, &keyText); err != nil {
		return SnapshotPublicationIdentityRecord{}, err
	}
	record.Role = domain.TopologyRole(role)
	if keyText != nil {
		key, err := uuid.Parse(*keyText)
		if err != nil {
			return SnapshotPublicationIdentityRecord{}, err
		}
		record.ClusterKeyID = &key
	}
	return record, nil
}

// sqliteLatestSnapshotPublication selects the absolute newest candidate without
// reading its body; the size comes from length() of the BLOB. Ordering matches
// PG: revision number, then revision/applied clock, then snapshot id.
func sqliteLatestSnapshotPublication(v sqlitedb.WriteValue, primaryID string) (SnapshotPublicationRecord, int, error) {
	var record SnapshotPublicationRecord
	var size, proofComplete int
	var applied string
	var discarded *string
	if err := v.Conn().QueryRowContext(v.Context(), `select a.id, a.source_primary_id, a.leadership_generation,
			a.snapshot_version, a.replication_version, a.content_hash,
			coalesce(r.id, ''), coalesce(j.id, ''), a.applied_at, a.discarded_at,
			coalesce(r.revision_number, 0), coalesce(length(a.snapshot_body), 0),
			coalesce((`+sqliteSnapshotProofSQL+`), 0)
		from applied_snapshots a
		left join config_revisions r on r.id = a.revision_id
		left join apply_jobs j on j.id = a.apply_job_id
		where a.source_primary_id = ?
		order by coalesce(r.revision_number, -1) desc, coalesce(r.created_at, a.applied_at) desc, a.id desc
		limit 1`, primaryID).Scan(&record.SnapshotID, &record.SourcePrimaryID, &record.LeadershipGeneration,
		&record.SnapshotVersion, &record.ReplicationVersion, &record.ContentHash,
		&record.RevisionID, &record.ApplyJobID, &applied, &discarded,
		&record.RevisionNumber, &size, &proofComplete); err != nil {
		return SnapshotPublicationRecord{}, 0, err
	}
	appliedAt, err := sqliteEnqueueTime(&applied)
	if err != nil || appliedAt == nil {
		return SnapshotPublicationRecord{}, 0, errors.New("snapshot applied_at is not a finite clock")
	}
	record.AppliedAt = *appliedAt
	if record.DiscardedAt, err = sqlitePublicationTime(discarded); err != nil {
		return SnapshotPublicationRecord{}, 0, err
	}
	record.ProofComplete = proofComplete == 1
	return record, size, nil
}

// LatestPublishedContentHash returns the content hash of the snapshot this
// PRIMARY currently publishes to its nodes: the absolute newest candidate for
// the local installation, and only when it is complete and not discarded.
// ok is false when nothing publishable exists yet. It needs no credential and
// returns no snapshot bytes.
func (s *SQLiteSnapshotPublicationStore) LatestPublishedContentHash(ctx context.Context) (hash string, ok bool, err error) {
	if ctx == nil || s == nil || s.writer == nil {
		return "", false, ErrSnapshotPublicationStore
	}
	err = s.writer.Write(ctx, func(v sqlitedb.WriteValue) error {
		identity, err := sqlitePublicationIdentity(v)
		if err != nil {
			return err
		}
		candidate, size, err := sqliteLatestSnapshotPublication(v, identity.InstallationID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if candidate.ProofComplete && candidate.DiscardedAt == nil && size > 0 &&
			candidate.LeadershipGeneration == identity.LeadershipGeneration {
			hash, ok = candidate.ContentHash, true
		}
		return nil
	})
	if err != nil {
		return "", false, ErrSnapshotPublicationStore
	}
	return hash, ok, nil
}
