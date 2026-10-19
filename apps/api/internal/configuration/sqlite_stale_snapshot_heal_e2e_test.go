package configuration_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/backupcore/dbexport"
	"github.com/zerkc/ProxyCore/apps/api/internal/cluster"
	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	replicationsnapshot "github.com/zerkc/ProxyCore/apps/api/internal/snapshot"
	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
	syncpublication "github.com/zerkc/ProxyCore/apps/api/internal/sync"
)

// markerLeakingTx re-creates the pre-0.6.8 export: it lists the bundle-only
// backup master-key marker (reserved nil id) as if it were a replicable secret.
type markerLeakingTx struct {
	configuration.CanonicalSnapshotTransaction
}

func (m markerLeakingTx) ListSecrets(ctx context.Context) ([]replicationsnapshot.PlainSecret, error) {
	list, err := m.CanonicalSnapshotTransaction.ListSecrets(ctx)
	if err != nil {
		return nil, err
	}
	return append(list, replicationsnapshot.PlainSecret{
		ID: uuid.Nil, Purpose: dbexport.BackupMasterKeyMarkerPurpose, Value: []byte("legacy-marker"),
	}), nil
}

type fixedDesired struct{ snapshot domain.ConfigurationSnapshot }

func (f fixedDesired) Snapshot(context.Context) (domain.ConfigurationSnapshot, error) {
	return f.snapshot, nil
}

type healPrimary struct {
	e       *configuration.ExternalTwoInstalls
	store   *configuration.SQLitePhase2Store
	desired domain.ConfigurationSnapshot
	stamp   string
	keyID   uuid.UUID
}

func newHealPrimary(t *testing.T) *healPrimary {
	t.Helper()
	e := configuration.NewExternalTwoInstalls(t)
	backupAndRestorePrimary(t, e)
	writer := sqlitedb.NewWriter(e.PrimaryDB, func() sqlitedb.FiniteTimestamp {
		return sqlitedb.FiniteTimestamp{Seconds: 123, Nanos: 456}
	})
	keys := cluster.NewSQLiteKeyStore(e.PrimaryDB, writer, e.PrimaryMaster)
	stamp, err := sqlitedb.EncodeTimestamp(sqlitedb.FiniteTimestamp{Seconds: time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	h := &healPrimary{
		e: e, store: configuration.NewSQLitePhase2Store(e.PrimaryDB, writer, keys, e.PrimaryMaster), stamp: *stamp,
		desired: domain.ConfigurationSnapshot{
			Settings: domain.Settings{Ingress: domain.Ingress{IPv4: "203.0.113.1"}},
			Zones:    []domain.ZoneState{}, Streams: []domain.StreamRoute{}, Certificates: []domain.CertificateStatus{},
		},
	}
	if err := e.PrimaryDB.QueryRow(`select cluster_key_id from installation_identity`).Scan(&h.keyID); err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *healPrimary) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := h.e.PrimaryDB.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

// export builds a canonical envelope for the primary with the real exporter.
func (h *healPrimary) export(t *testing.T, leakMarker bool) []byte {
	t.Helper()
	material, err := cluster.NewSQLiteKeyStore(h.e.PrimaryDB, sqlitedb.NewWriter(h.e.PrimaryDB, nil), h.e.PrimaryMaster).
		LoadActive(context.Background(), h.keyID)
	if err != nil {
		t.Fatal(err)
	}
	raw := material.CopyBytes()
	material.Destroy()
	kek, err := cluster.NewKEK(raw)
	if err != nil {
		t.Fatal(err)
	}
	defer kek.Destroy()
	var body []byte
	err = h.store.WithCanonicalSnapshot(context.Background(), func(tx configuration.CanonicalSnapshotTransaction) error {
		var lister configuration.CanonicalSnapshotTransaction = tx
		if leakMarker {
			lister = markerLeakingTx{tx}
		}
		env, err := replicationsnapshot.NewExporter(fixedDesired{h.desired}, lister, lister).Export(context.Background(), replicationsnapshot.ExporterInput{
			InstallationID: domain.InstallationID(h.e.PrimaryInstallID), NodeID: domain.NodeID(h.e.PrimaryNodeID),
			Role: domain.TopologyRolePrimary, Ingress: h.desired.Settings.Ingress, LeadershipGeneration: 7,
			ClusterKEK: kek, ClusterKeyRef: &h.keyID,
		})
		if err != nil {
			return err
		}
		body, err = replicationsnapshot.Marshal(env)
		return err
	})
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	return body
}

// seedJob stores an ordinary applied revision with its terminal combined job.
func (h *healPrimary) seedJob(t *testing.T) (revisionID, jobID string) {
	t.Helper()
	desired, err := json.Marshal(h.desired)
	if err != nil {
		t.Fatal(err)
	}
	revisionID, jobID = uuid.NewString(), uuid.NewString()
	h.exec(t, `insert into config_revisions(id,revision_number,checksum,snapshot,created_at,applied_at,source)
		values(?,(select coalesce(max(revision_number),0)+1 from config_revisions),?,?,?,?,'ordinary')`,
		revisionID, uuid.NewString(), string(desired), h.stamp, h.stamp)
	h.exec(t, `insert into apply_jobs(id,revision_id,target,status,correlation_id,created_at,finished_at,source)
		values(?,?,'combined','applied',?,?,?,'ordinary')`, jobID, revisionID, uuid.NewString(), h.stamp, h.stamp)
	return revisionID, jobID
}

// plant stores body as the job's published snapshot the way the legacy
// producer did, including the proof columns on the revision and the job.
func (h *healPrimary) plant(t *testing.T, revisionID, jobID string, body []byte) (snapshotID, hash string) {
	t.Helper()
	env, err := replicationsnapshot.Unmarshal(body)
	if err != nil {
		t.Fatal(err)
	}
	snapshotID, hash = uuid.NewString(), env.ContentHash
	h.exec(t, `update config_revisions set source_primary_id=?,snapshot_content_hash=?,snapshot_version=1,replication_version=1,leadership_generation=7 where id=?`,
		h.e.PrimaryInstallID, hash, revisionID)
	h.exec(t, `update apply_jobs set source_primary_id=?,snapshot_content_hash=?,snapshot_version=1,replication_version=1,leadership_generation=7 where id=?`,
		h.e.PrimaryInstallID, hash, jobID)
	h.exec(t, `insert into applied_snapshots(id,source_primary_id,leadership_generation,snapshot_version,replication_version,content_hash,snapshot_body,revision_id,status,apply_job_id,applied_at)
		values(?,?,7,1,1,?,?,?,'applied',?,?)`, snapshotID, h.e.PrimaryInstallID, hash, body, revisionID, jobID, h.stamp)
	return snapshotID, hash
}

func (h *healPrimary) producer() *syncpublication.CanonicalSnapshotProducer {
	return syncpublication.NewCanonicalSnapshotProducer(h.store, syncpublication.CanonicalSnapshotProducerOptions{})
}

func envHasNilSecret(t *testing.T, body []byte) bool {
	t.Helper()
	env, err := replicationsnapshot.Unmarshal(body)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range env.Replicated.Secrets {
		if secret.ID == uuid.Nil || secret.Purpose == dbexport.BackupMasterKeyMarkerPurpose {
			return true
		}
	}
	return false
}

// TestStaleMarkerSnapshotIsRegeneratedByReconcile covers the upgrade path of a
// primary restored from a pre-0.6.8 backup: its stored canonical snapshot
// carries the backup marker secret, the validator rejects it, and Reconcile must
// replace it without operator action so a node can enroll.
func TestStaleMarkerSnapshotIsRegeneratedByReconcile(t *testing.T) {
	h := newHealPrimary(t)
	ctx := context.Background()
	revisionID, jobID := h.seedJob(t)
	staleID, staleHash := h.plant(t, revisionID, jobID, h.export(t, true))

	kek := h.e.DeliverKEK(t)
	defer kek.Destroy()
	var stored []byte
	if err := h.e.PrimaryDB.QueryRow(`select snapshot_body from applied_snapshots where id = ?`, staleID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !envHasNilSecret(t, stored) {
		t.Fatal("precondition: planted snapshot must carry the nil-id marker secret")
	}
	staleEnv, _ := replicationsnapshot.Unmarshal(stored)
	if replicationsnapshot.NewValidator(kek).Validate(ctx, staleEnv).OK {
		t.Fatal("precondition: validator must reject the legacy snapshot")
	}

	if _, err := h.producer().Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var discarded *string
	if err := h.e.PrimaryDB.QueryRow(`select discarded_at from applied_snapshots where id = ?`, staleID).Scan(&discarded); err != nil || discarded == nil {
		t.Fatalf("stale row must be discarded, got %v, %v", discarded, err)
	}
	var freshID string
	if err := h.e.PrimaryDB.QueryRow(`select id, snapshot_body from applied_snapshots
		where apply_job_id = ? and discarded_at is null and status = 'applied'`, jobID).Scan(&freshID, &stored); err != nil {
		t.Fatalf("no regenerated snapshot for the job: %v", err)
	}
	if freshID == staleID {
		t.Fatal("regenerated snapshot must be a new row")
	}
	if envHasNilSecret(t, stored) {
		t.Fatal("regenerated snapshot still carries the marker secret")
	}
	freshEnv, err := replicationsnapshot.Unmarshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	if freshEnv.ContentHash == staleHash {
		t.Fatal("regenerated snapshot must have a new content hash")
	}
	if result := replicationsnapshot.NewValidator(kek).Validate(ctx, freshEnv); !result.OK {
		t.Fatalf("regenerated snapshot rejected by validator: %+v", result.Issues)
	}
	if err := h.e.ImportOnNode(freshEnv); err != nil {
		t.Fatalf("node import of regenerated snapshot: %v", err)
	}

	// A second pass is a no-op: the regenerated snapshot is healthy.
	if _, err := h.producer().Reconcile(ctx); err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	var live int
	if err := h.e.PrimaryDB.QueryRow(`select count(*) from applied_snapshots where apply_job_id = ? and discarded_at is null`, jobID).Scan(&live); err != nil || live != 1 {
		t.Fatalf("live snapshots after second Reconcile = %d, %v", live, err)
	}
}

// TestHealthyStoredSnapshotIsNotRegenerated proves Reconcile leaves a valid
// stored snapshot byte-for-byte alone.
func TestHealthyStoredSnapshotIsNotRegenerated(t *testing.T) {
	h := newHealPrimary(t)
	revisionID, jobID := h.seedJob(t)
	id, hash := h.plant(t, revisionID, jobID, h.export(t, false))

	if _, err := h.producer().Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var gotID, gotHash string
	var discarded *string
	if err := h.e.PrimaryDB.QueryRow(`select id, content_hash, discarded_at from applied_snapshots where apply_job_id = ?`, jobID).Scan(&gotID, &gotHash, &discarded); err != nil {
		t.Fatal(err)
	}
	if gotID != id || gotHash != hash || discarded != nil {
		t.Fatalf("healthy snapshot changed: id %s->%s hash %s->%s discarded=%v", id, gotID, hash, gotHash, discarded)
	}
}
