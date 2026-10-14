package configuration_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/cluster"
	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

// grantAckFixture enrolls a node through the real grant exchange (which never
// writes enrollment_attempts) and seeds one published, applied snapshot.
func grantAckFixture(t *testing.T) (*grantFixture, *configuration.SQLiteSnapshotAcknowledgementStore, configuration.SnapshotAcknowledgement) {
	t.Helper()
	f := newGrantFixture(t)
	token, _ := f.insertToken("a1", 0x41, f.now.Add(time.Hour))
	request := f.request(token, newRecipient(t))
	if _, err := f.service.IssueGrant(context.Background(), request); err != nil {
		t.Fatalf("IssueGrant: %v", err)
	}
	if n := f.count(`select count(*) from enrollment_attempts`); n != 0 {
		t.Fatalf("grant exchange wrote %d enrollment_attempts rows", n)
	}
	sum := sha256.Sum256([]byte("grant-ack"))
	hash := hex.EncodeToString(sum[:])
	revision, job, applied := uuid.NewString(), uuid.NewString(), uuid.NewString()
	at := time.Unix(100, 0)
	stamp := grantStamp(t, at)
	f.exec(`insert into config_revisions(id,revision_number,checksum,snapshot,created_at) values (?,1,'grant-ack','{}','0')`, revision)
	f.exec(`update config_revisions set source='ordinary',source_primary_id=?,snapshot_content_hash=?,snapshot_version=1,replication_version=1,leadership_generation=7,applied_at=? where id=?`, f.primaryID, hash, stamp, revision)
	f.exec(`insert into apply_jobs(id,revision_id,target,status,correlation_id,created_at,finished_at,source,source_primary_id,snapshot_content_hash,snapshot_version,replication_version,leadership_generation) values (?,?,'nginx','applied','grant-ack','0',?,'ordinary',?,?,1,1,7)`, job, revision, stamp, f.primaryID, hash)
	f.exec(`insert into applied_snapshots(id,source_primary_id,leadership_generation,snapshot_version,replication_version,content_hash,status,revision_id,apply_job_id,applied_at,snapshot_body) values (?,?,7,1,1,?,'applied',?,?,?,?)`, applied, f.primaryID, hash, revision, job, stamp, []byte("{}"))
	writer := sqlitedb.NewWriter(f.db, nil)
	master := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x37}, 32))
	store := configuration.NewSQLiteSnapshotAcknowledgementStore(f.db, writer, cluster.NewSQLiteKeyStore(f.db, writer, master))
	ack := configuration.SnapshotAcknowledgement{
		NodeID: request.TargetNodeID, ContentHash: hash, SnapshotVersion: 1, ReplicationVersion: 1,
		RevisionID: revision, LeadershipGeneration: 7, AppliedAt: at,
	}
	return f, store, ack
}

func TestSQLiteNodeEnrolledThroughGrantExchangeCanAcknowledge(t *testing.T) {
	f, store, ack := grantAckFixture(t)
	if err := store.RecordSnapshotAcknowledgement(context.Background(), ack); err != nil {
		t.Fatalf("acknowledge: %v", err)
	}
	if n := f.count(`select count(*) from node_snapshot_acks where node_id=? and content_hash=?`, ack.NodeID, ack.ContentHash); n != 1 {
		t.Fatalf("ack rows = %d", n)
	}
}

func TestSQLiteGrantExchangeNodeAcknowledgementDenials(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(f *grantFixture, ack *configuration.SnapshotAcknowledgement)
		want  error
	}{
		{"revoked credential", func(f *grantFixture, _ *configuration.SnapshotAcknowledgement) {
			f.exec(`update node_credentials set revoked_at='2'`)
		}, configuration.ErrSnapshotAcknowledgementRevoked},
		{"unknown node", func(_ *grantFixture, a *configuration.SnapshotAcknowledgement) {
			a.NodeID = uuid.NewString()
		}, configuration.ErrSnapshotAcknowledgementDenied},
		{"cluster key rotated after enrollment", func(f *grantFixture, _ *configuration.SnapshotAcknowledgement) {
			id := uuid.NewString()
			f.exec(`update cluster_keys set retired_at=?`, grantStamp(t, time.Now().Add(time.Hour)))
			f.exec(`insert into cluster_keys(id,purpose,wrapped_kek,wrapping_key_version,created_at) values (?,'synthetic','synthetic',1,?)`, id, grantStamp(t, time.Now().Add(time.Hour)))
			f.exec(`update installation_identity set cluster_key_id=?`, id)
		}, configuration.ErrSnapshotAcknowledgementDenied},
		{"no cluster key bound", func(f *grantFixture, _ *configuration.SnapshotAcknowledgement) {
			f.exec(`update installation_identity set cluster_key_id=null`)
		}, configuration.ErrSnapshotAcknowledgementDenied},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, store, ack := grantAckFixture(t)
			tc.setup(f, &ack)
			if err := store.RecordSnapshotAcknowledgement(context.Background(), ack); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if n := f.count(`select count(*) from node_snapshot_acks`); n != 0 {
				t.Fatalf("ack rows = %d", n)
			}
		})
	}
}
