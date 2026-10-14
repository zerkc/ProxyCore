package configuration

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/cluster"
	"github.com/zerkc/ProxyCore/apps/api/internal/secrets"
	replicationsnapshot "github.com/zerkc/ProxyCore/apps/api/internal/snapshot"
)

var errPhase2Rollback = errors.New("phase2 rollback sentinel")

func newSQLitePhase2Fixture(t *testing.T) (*sqlitePublicationFixture, *SQLitePhase2Store) {
	t.Helper()
	f := newSQLitePublicationFixture(t)
	keys := cluster.NewSQLiteKeyStore(f.db, f.writer, f.master)
	return f, NewSQLitePhase2Store(f.db, f.writer, keys, f.master)
}

// insertCanonicalApply stores an ordinary applied revision and its terminal job
// with no applied snapshot, i.e. a canonical snapshot candidate.
func insertCanonicalApply(t *testing.T, f *sqlitePublicationFixture, number int) (revisionID, jobID string) {
	t.Helper()
	revisionID, jobID = uuid.NewString(), uuid.NewString()
	at := tokenKey(t, time.Date(2026, 2, 3, 4, 5, number, 0, time.UTC))
	settingsExec(t, f.db, `insert into config_revisions(id,revision_number,checksum,snapshot,created_at,applied_at,source) values(?,?,?,'{"desired":true}',?,?,'ordinary')`,
		revisionID, 100+number, uuid.NewString(), at, at)
	settingsExec(t, f.db, `insert into apply_jobs(id,revision_id,target,status,correlation_id,created_at,finished_at,source) values(?,?,'combined','applied',?,?,?,'ordinary')`,
		jobID, revisionID, uuid.NewString(), at, at)
	return revisionID, jobID
}

func canonicalWriteFor(f *sqlitePublicationFixture, revisionID, jobID, hash string, body []byte) CanonicalSnapshotWrite {
	return CanonicalSnapshotWrite{
		ID: uuid.NewString(), SnapshotBody: body, SourcePrimaryID: f.primaryID, LeadershipGeneration: 7,
		SnapshotVersion: 1, ReplicationVersion: 1, ContentHash: hash, RevisionID: revisionID,
		ApplyJobID: jobID, AppliedAt: time.Date(2026, 3, 1, 2, 3, 4, 5, time.UTC),
	}
}

func phase2Count(t *testing.T, f *sqlitePublicationFixture, query string, args ...any) int {
	t.Helper()
	var n int
	if err := f.db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestSQLitePhase2StoreSatisfiesContract(t *testing.T) {
	_, store := newSQLitePhase2Fixture(t)
	var _ sqlitePhase2Contract = store
	var _ replicationsnapshot.ArchiveStore = store
	var _ replicationsnapshot.ApplyJobEnqueuer = store
}

func TestSQLitePhase2WithCanonicalSnapshotReadsAndPublishes(t *testing.T) {
	f, store := newSQLitePhase2Fixture(t)
	ctx := context.Background()
	revisionID, jobID := insertCanonicalApply(t, f, 1)
	if _, err := secrets.NewSQLiteStore(f.db, f.master).Put(ctx, "dns", "plain-value"); err != nil {
		t.Fatal(err)
	}
	body, hash := publicationEnvelopeBody(t, f.primaryID)

	candidates, err := store.ListCanonicalSnapshotCandidates(ctx)
	if err != nil || len(candidates) != 1 || candidates[0].JobID != jobID {
		t.Fatalf("candidates before = %v, %v", candidates, err)
	}
	err = store.WithCanonicalSnapshot(ctx, func(tx CanonicalSnapshotTransaction) error {
		identity, err := tx.LockPrimaryIdentity(ctx)
		if err != nil {
			return err
		}
		if identity.InstallationID != f.primaryID || identity.NodeID != f.nodeID || identity.LeadershipGeneration != 7 ||
			identity.LatestKnownGeneration != 7 || identity.ClusterKeyID == nil || *identity.ClusterKeyID != f.clusterKey {
			t.Errorf("identity = %#v", identity)
		}
		material, err := tx.LoadOrCreateClusterKEK(ctx, identity.ClusterKeyID.String())
		if err != nil {
			return err
		}
		if material.ID() != f.clusterKey.String() || len(material.CopyBytes()) != 32 {
			t.Errorf("material id = %s", material.ID())
		}
		material.Destroy()
		apply, err := tx.GetCanonicalSnapshotApply(ctx, jobID)
		if err != nil {
			return err
		}
		if apply.JobID != jobID || apply.RevisionID != revisionID || apply.RevisionNumber != 101 ||
			string(apply.DesiredSnapshot) != `{"desired":true}` || apply.Status != "applied" || apply.Target != "combined" ||
			apply.Source != PersistenceSourceOrdinary || apply.RevisionSource != PersistenceSourceOrdinary ||
			apply.FinishedAt == nil || apply.RevisionAppliedAt == nil || apply.SourcePrimaryID != nil {
			t.Errorf("apply = %#v", apply)
		}
		if existing, err := tx.FindCanonicalSnapshotForJob(ctx, jobID); err != nil || existing != nil {
			t.Errorf("existing = %v, %v", existing, err)
		}
		list, err := tx.ListSecrets(ctx)
		if err != nil || len(list) != 1 || list[0].Purpose != "dns" || string(list[0].Value) != "plain-value" {
			t.Errorf("secrets = %v, %v", list, err)
		}
		owners, err := tx.ListReplicableOwners(ctx)
		if err != nil || len(owners) != 1 || owners[0].Username != "synthetic-owner" || owners[0].Role != "owner" {
			t.Errorf("owners = %v, %v", owners, err)
		}
		return tx.PublishCanonicalSnapshot(ctx, canonicalWriteFor(f, revisionID, jobID, hash, body))
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := phase2Count(t, f, `select count(*) from applied_snapshots where apply_job_id = ? and content_hash = ? and snapshot_body is not null and status = 'applied'`, jobID, hash); n != 1 {
		t.Fatalf("published rows = %d", n)
	}
	if n := phase2Count(t, f, `select count(*) from config_revisions where id = ? and snapshot_content_hash = ? and source_primary_id = ?`, revisionID, hash, f.primaryID); n != 1 {
		t.Fatalf("revision proof rows = %d", n)
	}
	if n := phase2Count(t, f, `select count(*) from apply_jobs where id = ? and snapshot_content_hash = ? and leadership_generation = 7`, jobID, hash); n != 1 {
		t.Fatalf("job proof rows = %d", n)
	}
	if candidates, err := store.ListCanonicalSnapshotCandidates(ctx); err != nil || len(candidates) != 0 {
		t.Fatalf("candidates after = %v, %v", candidates, err)
	}
	// Replay with identical proof is idempotent inside a second transaction.
	err = store.WithCanonicalSnapshot(ctx, func(tx CanonicalSnapshotTransaction) error {
		existing, err := tx.FindCanonicalSnapshotForJob(ctx, jobID)
		if err != nil || existing == nil || existing.ContentHash != hash || string(existing.SnapshotBody) != string(body) ||
			existing.Status != AppliedSnapshotApplied || existing.DiscardedAt != nil {
			t.Errorf("existing = %#v, %v", existing, err)
		}
		write := canonicalWriteFor(f, revisionID, jobID, hash, body)
		write.ID = existing.ID
		write.AppliedAt = existing.AppliedAt
		return tx.PublishCanonicalSnapshot(ctx, write)
	})
	if err != nil {
		t.Fatalf("idempotent replay: %v", err)
	}
}

func TestSQLitePhase2PublishRollsBackWithTransaction(t *testing.T) {
	f, store := newSQLitePhase2Fixture(t)
	ctx := context.Background()
	revisionID, jobID := insertCanonicalApply(t, f, 1)
	body, hash := publicationEnvelopeBody(t, f.primaryID)
	err := store.WithCanonicalSnapshot(ctx, func(tx CanonicalSnapshotTransaction) error {
		if err := tx.PublishCanonicalSnapshot(ctx, canonicalWriteFor(f, revisionID, jobID, hash, body)); err != nil {
			return err
		}
		return errPhase2Rollback
	})
	if !errors.Is(err, errPhase2Rollback) {
		t.Fatalf("err = %v", err)
	}
	if n := phase2Count(t, f, `select count(*) from applied_snapshots`); n != 0 {
		t.Fatalf("applied snapshots after rollback = %d", n)
	}
	if n := phase2Count(t, f, `select count(*) from config_revisions where id = ? and snapshot_content_hash is null`, revisionID); n != 1 {
		t.Fatal("revision proof leaked past rollback")
	}
}

func TestSQLitePhase2PublishConflicts(t *testing.T) {
	f, store := newSQLitePhase2Fixture(t)
	ctx := context.Background()
	revisionID, jobID := insertCanonicalApply(t, f, 1)
	otherRevision, otherJob := insertCanonicalApply(t, f, 2)
	body, hash := publicationEnvelopeBody(t, f.primaryID)
	if err := store.WithCanonicalSnapshot(ctx, func(tx CanonicalSnapshotTransaction) error {
		return tx.PublishCanonicalSnapshot(ctx, canonicalWriteFor(f, revisionID, jobID, hash, body))
	}); err != nil {
		t.Fatal(err)
	}
	cases := map[string]CanonicalSnapshotWrite{
		"empty body": canonicalWriteFor(f, otherRevision, otherJob, hash+"x", nil),
		"bad id": func() CanonicalSnapshotWrite {
			w := canonicalWriteFor(f, otherRevision, otherJob, hash+"x", body)
			w.ID = "nope"
			return w
		}(),
		"zero generation": func() CanonicalSnapshotWrite {
			w := canonicalWriteFor(f, otherRevision, otherJob, hash+"x", body)
			w.LeadershipGeneration = 0
			return w
		}(),
		"zero applied at": func() CanonicalSnapshotWrite {
			w := canonicalWriteFor(f, otherRevision, otherJob, hash+"x", body)
			w.AppliedAt = time.Time{}
			return w
		}(),
		"unknown revision": canonicalWriteFor(f, uuid.NewString(), otherJob, hash+"y", body),
		"unknown job":      canonicalWriteFor(f, otherRevision, uuid.NewString(), hash+"z", body),
	}
	for name, write := range cases {
		err := store.WithCanonicalSnapshot(ctx, func(tx CanonicalSnapshotTransaction) error {
			return tx.PublishCanonicalSnapshot(ctx, write)
		})
		if !errors.Is(err, ErrCanonicalSnapshotConflict) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if n := phase2Count(t, f, `select count(*) from applied_snapshots`); n != 1 {
		t.Fatalf("applied snapshots = %d", n)
	}
	// A failed publish leaves the sibling revision untouched.
	if n := phase2Count(t, f, `select count(*) from config_revisions where id = ? and snapshot_content_hash is null`, otherRevision); n != 1 {
		t.Fatal("conflicting publish mutated revision")
	}
}

// A revert publishes an already-used content hash. The published snapshot is the
// latest applied one by revision recency, never by hash uniqueness.
func TestSQLitePhase2PublishRevertToEarlierHashBecomesLatest(t *testing.T) {
	f, store := newSQLitePhase2Fixture(t)
	ctx := context.Background()
	body, hashA := publicationEnvelopeBody(t, f.primaryID)
	hashB := strings.Repeat("b", 64)
	publish := func(number int, hash string) (string, string) {
		revisionID, jobID := insertCanonicalApply(t, f, number)
		if err := store.WithCanonicalSnapshot(ctx, func(tx CanonicalSnapshotTransaction) error {
			write := canonicalWriteFor(f, revisionID, jobID, hash, body)
			write.AppliedAt = time.Date(2026, 3, 1, 2, 3, number, 0, time.UTC)
			return tx.PublishCanonicalSnapshot(ctx, write)
		}); err != nil {
			t.Fatalf("publish %d: %v", number, err)
		}
		return revisionID, jobID
	}
	publish(1, hashA)
	publish(2, hashB)
	if got, ok, err := f.store.LatestPublishedContentHash(ctx); err != nil || !ok || got != hashB {
		t.Fatalf("latest after B = %q, %v, %v", got, ok, err)
	}
	revertRevision, revertJob := publish(3, hashA)
	if got, ok, err := f.store.LatestPublishedContentHash(ctx); err != nil || !ok || got != hashA {
		t.Fatalf("latest after revert = %q, %v, %v", got, ok, err)
	}
	if n := phase2Count(t, f, `select count(*) from applied_snapshots where content_hash = ? and revision_id = ? and apply_job_id = ?`, hashA, revertRevision, revertJob); n != 1 {
		t.Fatalf("revert row not bound to the new revision/job: %d", n)
	}
	// A node on B receives A; a node already on A short-circuits.
	request := publicationRequest(f)
	request.AfterContentHash = hashB
	got, err := f.store.ReadSnapshotPublication(ctx, request)
	if err != nil || got.Current || got.Snapshot.ContentHash != hashA || len(got.Snapshot.Bytes) == 0 {
		t.Fatalf("node on B = %#v, %v", got, err)
	}
	request.AfterContentHash = hashA
	got, err = f.store.ReadSnapshotPublication(ctx, request)
	if err != nil || !got.Current || got.Snapshot.ContentHash != hashA {
		t.Fatalf("node on A = %#v, %v", got, err)
	}
	// The producer must not re-offer the superseded job of the earlier A.
	if candidates, err := store.ListCanonicalSnapshotCandidates(ctx); err != nil || len(candidates) != 0 {
		t.Fatalf("candidates = %v, %v", candidates, err)
	}
}

// X -> Y -> X: re-applying X reuses X's revision (checksum is unique), so its
// revision number (5) is lower than Y's (6). The most recently applied
// configuration is still the one published as latest.
func TestSQLitePhase2PublishReapplyReusingRevisionNumberBecomesLatest(t *testing.T) {
	f, store := newSQLitePhase2Fixture(t)
	ctx := context.Background()
	body, hashX := publicationEnvelopeBody(t, f.primaryID)
	hashY := strings.Repeat("c", 64)
	at := func(sec int) time.Time { return time.Date(2026, 3, 1, 2, 3, sec, 0, time.UTC) }
	publish := func(revisionID, jobID, hash string, sec int) {
		t.Helper()
		if err := store.WithCanonicalSnapshot(ctx, func(tx CanonicalSnapshotTransaction) error {
			write := canonicalWriteFor(f, revisionID, jobID, hash, body)
			write.AppliedAt = at(sec)
			return tx.PublishCanonicalSnapshot(ctx, write)
		}); err != nil {
			t.Fatalf("publish %s: %v", hash[:4], err)
		}
	}
	revX, jobX1 := insertCanonicalApply(t, f, 5)
	revY, jobY := insertCanonicalApply(t, f, 6)
	publish(revX, jobX1, hashX, 10)
	publish(revY, jobY, hashY, 20)
	if got, ok, err := f.store.LatestPublishedContentHash(ctx); err != nil || !ok || got != hashY {
		t.Fatalf("latest after Y = %q, %v, %v", got, ok, err)
	}
	// The re-apply of X: a new job on the SAME revision, finished later than Y.
	jobX2 := uuid.NewString()
	finished := tokenKey(t, at(30))
	settingsExec(t, f.db, `insert into apply_jobs(id,revision_id,target,status,correlation_id,created_at,finished_at,source) values(?,?,'combined','applied',?,?,?,'ordinary')`,
		jobX2, revX, uuid.NewString(), finished, finished)
	if candidates, err := store.ListCanonicalSnapshotCandidates(ctx); err != nil || len(candidates) != 1 || candidates[0].JobID != jobX2 {
		t.Fatalf("candidates = %v, %v", candidates, err)
	}
	publish(revX, jobX2, hashX, 30)
	if got, ok, err := f.store.LatestPublishedContentHash(ctx); err != nil || !ok || got != hashX {
		t.Fatalf("latest after re-apply of X = %q, %v, %v", got, ok, err)
	}
	// A node on Y receives X; a node already on X short-circuits.
	request := publicationRequest(f)
	request.AfterContentHash = hashY
	got, err := f.store.ReadSnapshotPublication(ctx, request)
	if err != nil || got.Current || got.Snapshot.ContentHash != hashX || len(got.Snapshot.Bytes) == 0 {
		t.Fatalf("node on Y = %#v, %v", got, err)
	}
	request.AfterContentHash = hashX
	if got, err = f.store.ReadSnapshotPublication(ctx, request); err != nil || !got.Current || got.Snapshot.ContentHash != hashX {
		t.Fatalf("node on X = %#v, %v", got, err)
	}
	// Replaying the older apply of X (or Y) afterwards does not rewind latest,
	// even though Y has the higher revision number.
	publish(revX, jobX1, hashX, 10)
	publish(revY, jobY, hashY, 20)
	if got, ok, err := f.store.LatestPublishedContentHash(ctx); err != nil || !ok || got != hashX {
		t.Fatalf("latest after stale replays = %q, %v, %v", got, ok, err)
	}
	if candidates, err := store.ListCanonicalSnapshotCandidates(ctx); err != nil || len(candidates) != 0 {
		t.Fatalf("candidates after = %v, %v", candidates, err)
	}
}

// Publishing an older revision for a hash that a newer revision already holds is
// a stale replay and must not move the published snapshot backwards.
func TestSQLitePhase2PublishStaleRevisionDoesNotRewindLatest(t *testing.T) {
	f, store := newSQLitePhase2Fixture(t)
	ctx := context.Background()
	body, hash := publicationEnvelopeBody(t, f.primaryID)
	oldRevision, oldJob := insertCanonicalApply(t, f, 1)
	newRevision, newJob := insertCanonicalApply(t, f, 2)
	for _, w := range []CanonicalSnapshotWrite{
		canonicalWriteFor(f, newRevision, newJob, hash, body),
		canonicalWriteFor(f, oldRevision, oldJob, hash, body),
	} {
		if err := store.WithCanonicalSnapshot(ctx, func(tx CanonicalSnapshotTransaction) error {
			return tx.PublishCanonicalSnapshot(ctx, w)
		}); err != nil {
			t.Fatal(err)
		}
	}
	if n := phase2Count(t, f, `select count(*) from applied_snapshots where revision_id = ? and apply_job_id = ?`, newRevision, newJob); n != 1 {
		t.Fatalf("snapshot rewound: %d", n)
	}
}

func TestSQLitePhase2TransactionErrors(t *testing.T) {
	f, store := newSQLitePhase2Fixture(t)
	ctx := context.Background()
	err := store.WithCanonicalSnapshot(ctx, func(tx CanonicalSnapshotTransaction) error {
		if _, err := tx.GetCanonicalSnapshotApply(ctx, uuid.NewString()); !errors.Is(err, ErrCanonicalSnapshotNotFound) {
			t.Errorf("missing apply err = %v", err)
		}
		if _, err := tx.GetCanonicalSnapshotApply(ctx, "not-a-uuid"); err == nil || errors.Is(err, ErrCanonicalSnapshotNotFound) {
			t.Errorf("malformed apply err = %v", err)
		}
		if _, err := tx.LoadOrCreateClusterKEK(ctx, "not-a-uuid"); !errors.Is(err, ErrEnrollmentGrantStore) {
			t.Errorf("malformed kek err = %v", err)
		}
		if _, err := tx.LoadOrCreateClusterKEK(ctx, uuid.Nil.String()); !errors.Is(err, ErrEnrollmentGrantStore) {
			t.Errorf("nil kek err = %v", err)
		}
		if _, err := tx.LoadOrCreateClusterKEK(ctx, uuid.NewString()); !errors.Is(err, ErrEnrollmentGrantStore) {
			t.Errorf("unknown kek err = %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	settingsExec(t, f.db, `update installation_identity set leadership_generation = 0`)
	err = store.WithCanonicalSnapshot(ctx, func(tx CanonicalSnapshotTransaction) error {
		_, err := tx.LockPrimaryIdentity(ctx)
		return err
	})
	if !errors.Is(err, ErrEnrollmentGrantStore) {
		t.Fatalf("zero generation err = %v", err)
	}
	settingsExec(t, f.db, `delete from installation_identity`)
	err = store.WithCanonicalSnapshot(ctx, func(tx CanonicalSnapshotTransaction) error {
		_, err := tx.LockPrimaryIdentity(ctx)
		return err
	})
	if !errors.Is(err, ErrEnrollmentGrantStore) {
		t.Fatalf("missing identity err = %v", err)
	}
}

func TestSQLitePhase2LoadOrCreateKEKBindsIdentityAndRollsBack(t *testing.T) {
	f, store := newSQLitePhase2Fixture(t)
	ctx := context.Background()
	settingsExec(t, f.db, `update installation_identity set cluster_key_id = null`)
	err := store.WithCanonicalSnapshot(ctx, func(tx CanonicalSnapshotTransaction) error {
		material, err := tx.LoadOrCreateClusterKEK(ctx, "")
		if err != nil {
			return err
		}
		material.Destroy()
		return errPhase2Rollback
	})
	if !errors.Is(err, errPhase2Rollback) {
		t.Fatal(err)
	}
	if n := phase2Count(t, f, `select count(*) from installation_identity where cluster_key_id is null`); n != 1 {
		t.Fatal("identity key binding leaked past rollback")
	}
	err = store.WithCanonicalSnapshot(ctx, func(tx CanonicalSnapshotTransaction) error {
		material, err := tx.LoadOrCreateClusterKEK(ctx, "")
		if err != nil {
			return err
		}
		defer material.Destroy()
		if material.ID() != f.clusterKey.String() {
			t.Errorf("material id = %s", material.ID())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := phase2Count(t, f, `select count(*) from installation_identity where cluster_key_id = ?`, f.clusterKey.String()); n != 1 {
		t.Fatal("identity not bound to the key")
	}
}

func TestSQLitePhase2WithCanonicalSnapshotUnavailable(t *testing.T) {
	_, store := newSQLitePhase2Fixture(t)
	noop := func(CanonicalSnapshotTransaction) error { return nil }
	var nilStore *SQLitePhase2Store
	for name, err := range map[string]error{
		"nil store": nilStore.WithCanonicalSnapshot(context.Background(), noop),
		"nil work":  store.WithCanonicalSnapshot(context.Background(), nil),
		"zero":      (&SQLitePhase2Store{}).WithCanonicalSnapshot(context.Background(), noop),
	} {
		if !errors.Is(err, ErrSnapshotPublicationStore) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	var nilCtx context.Context
	if err := store.WithCanonicalSnapshot(nilCtx, noop); !errors.Is(err, ErrSnapshotPublicationStore) {
		t.Errorf("nil ctx err = %v", err)
	}
}

func TestSQLitePhase2CreateApplyJobFromSnapshot(t *testing.T) {
	f, store := newSQLitePhase2Fixture(t)
	body, hash := publicationEnvelopeBody(t, f.primaryID)
	jobID, err := store.CreateApplyJobFromSnapshot(context.Background(), string(PersistenceSourceImport), body)
	if err != nil {
		t.Fatal(err)
	}
	if n := phase2Count(t, f, `select count(*) from apply_jobs j join config_revisions r on r.id = j.revision_id
		where j.id = ? and j.status = 'queued' and j.target = 'combined' and j.source = 'import' and j.actor_user_id is null
		and r.source = 'import' and r.applied_at is null and r.revision_number = 1 and r.snapshot_content_hash = ?
		and j.snapshot_content_hash = ? and r.source_primary_id = ? and j.source_primary_id = ?
		and j.correlation_id is not null and j.leadership_generation = r.leadership_generation`, jobID.String(), hash, hash, f.primaryID, f.primaryID); n != 1 {
		t.Fatalf("imported revision/job pair rows = %d", n)
	}
	terminal, err := NewSQLiteApplyJobStore(f.db).GetApplyJobTerminal(context.Background(), jobID)
	if err != nil || terminal.Status != "queued" {
		t.Fatalf("terminal = %#v, %v", terminal, err)
	}
	// A second import allocates the next revision number.
	other, _ := publicationEnvelopeBody(t, uuid.NewString())
	if _, err := store.CreateApplyJobFromSnapshot(context.Background(), "import", other); err != nil {
		t.Fatal(err)
	}
	if n := phase2Count(t, f, `select max(revision_number) from config_revisions`); n != 2 {
		t.Fatalf("max revision = %d", n)
	}
}

func TestSQLitePhase2CreateApplyJobFromSnapshotRetryReusesLatestImportJob(t *testing.T) {
	f, store := newSQLitePhase2Fixture(t)
	ctx := context.Background()
	body, _ := publicationEnvelopeBody(t, f.primaryID)
	first, err := store.CreateApplyJobFromSnapshot(ctx, "import", body)
	if err != nil {
		t.Fatal(err)
	}
	// An interrupted conversion retries with the same snapshot: it must reuse the
	// queued job instead of tripping the unique checksum index.
	retry, err := store.CreateApplyJobFromSnapshot(ctx, "import", body)
	if err != nil || retry != first {
		t.Fatalf("retry = %s, %v; want %s", retry, err, first)
	}
	if n := phase2Count(t, f, `select count(*) from config_revisions`); n != 1 {
		t.Fatalf("revisions = %d", n)
	}
	if n := phase2Count(t, f, `select count(*) from apply_jobs`); n != 1 {
		t.Fatalf("jobs = %d", n)
	}
	settingsExec(t, f.db, `update apply_jobs set status = 'applied'`)
	if again, err := store.CreateApplyJobFromSnapshot(ctx, "import", body); err != nil || again != first {
		t.Fatalf("applied retry = %s, %v", again, err)
	}
	// A failed job is never silently reused: the next cycle imports again by
	// refreshing the revision that owns the unique checksum.
	settingsExec(t, f.db, `update apply_jobs set status = 'failed'`)
	retried, err := store.CreateApplyJobFromSnapshot(ctx, "import", body)
	if err != nil || retried == first {
		t.Fatalf("failed retry = %s, %v; want a new job", retried, err)
	}
	if n := phase2Count(t, f, `select count(*) from config_revisions`); n != 1 {
		t.Fatalf("revisions after failed retry = %d", n)
	}
	if n := phase2Count(t, f, `select count(*) from apply_jobs where id = ? and status = 'queued'`, retried.String()); n != 1 {
		t.Fatal("retry job is not queued")
	}
	settingsExec(t, f.db, `update apply_jobs set status = 'applied'`)
	// Once a newer revision exists the old content is no longer the latest, so
	// importing it again (a revert) refreshes its revision to be the latest and
	// re-materializes it instead of reusing the stale job.
	other, _ := publicationEnvelopeBody(t, uuid.NewString())
	otherJob, err := store.CreateApplyJobFromSnapshot(ctx, "import", other)
	if err != nil {
		t.Fatal(err)
	}
	settingsExec(t, f.db, `update apply_jobs set status = 'applied'`)
	revert, err := store.CreateApplyJobFromSnapshot(ctx, "import", body)
	if err != nil || revert == otherJob || revert == retried {
		t.Fatalf("revert = %s, %v", revert, err)
	}
	if n := phase2Count(t, f, `select count(*) from config_revisions`); n != 2 {
		t.Fatalf("revisions after revert = %d", n)
	}
	if n := phase2Count(t, f, `select count(*) from config_revisions r join apply_jobs j on j.revision_id = r.id
		where j.id = ? and r.revision_number = (select max(revision_number) from config_revisions) and r.applied_at is null`, revert.String()); n != 1 {
		t.Fatal("reverted revision is not the latest unapplied revision")
	}
	// Exactly one live job: the refreshed revision owns only its new queued job.
	if n := phase2Count(t, f, `select count(*) from apply_jobs where status = 'queued'`); n != 1 {
		t.Fatalf("queued jobs = %d", n)
	}
}

func TestSQLitePhase2CreateApplyJobFromSnapshotErrors(t *testing.T) {
	f, store := newSQLitePhase2Fixture(t)
	body, _ := publicationEnvelopeBody(t, f.primaryID)
	var nilCtx context.Context
	var nilStore *SQLitePhase2Store
	cases := map[string]struct {
		err  error
		want string
	}{
		"label":     {err: second(store.CreateApplyJobFromSnapshot(context.Background(), "ordinary", body)), want: `unsupported import source "ordinary"`},
		"empty":     {err: second(store.CreateApplyJobFromSnapshot(context.Background(), "import", nil)), want: "read imported snapshot metadata: import snapshot is empty"},
		"malformed": {err: second(store.CreateApplyJobFromSnapshot(context.Background(), "import", []byte("{"))), want: "read imported snapshot metadata"},
		"nil ctx":   {err: second(store.CreateApplyJobFromSnapshot(nilCtx, "import", body)), want: "import apply context is required"},
		"nil store": {err: second(nilStore.CreateApplyJobFromSnapshot(context.Background(), "import", body)), want: "import apply store is required"},
	}
	for name, tc := range cases {
		if tc.err == nil || !strings.Contains(tc.err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, tc.err, tc.want)
		}
	}
	if n := phase2Count(t, f, `select count(*) from config_revisions`); n != 0 {
		t.Fatalf("revisions = %d", n)
	}
}

func second(_ uuid.UUID, err error) error { return err }

func TestSQLitePhase2CreateApplyJobFromSnapshotIsAtomic(t *testing.T) {
	f, store := newSQLitePhase2Fixture(t)
	body, _ := publicationEnvelopeBody(t, f.primaryID)
	settingsExec(t, f.db, `create trigger phase2_job_fail before insert on apply_jobs begin select raise(abort, 'job insert refused'); end`)
	if _, err := store.CreateApplyJobFromSnapshot(context.Background(), "import", body); err == nil || !strings.Contains(err.Error(), "job insert refused") {
		t.Fatalf("err = %v", err)
	}
	if n := phase2Count(t, f, `select count(*) from config_revisions`); n != 0 {
		t.Fatalf("revision leaked without its job: %d", n)
	}
}

func TestSQLitePhase2StandaloneArchive(t *testing.T) {
	f, store := newSQLitePhase2Fixture(t)
	ctx := context.Background()
	now := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	expired := replicationsnapshot.StandaloneArchive{ID: uuid.New(), SnapshotData: []byte(`{"a":1}`), Reason: "node-conversion", CapturedAt: now.Add(-48 * time.Hour), ExpiresAt: now.Add(-time.Hour)}
	live := replicationsnapshot.StandaloneArchive{ID: uuid.New(), SnapshotData: []byte(`{"b":2}`), Reason: "node-conversion", CapturedAt: now, ExpiresAt: now.Add(time.Hour)}
	for _, archive := range []replicationsnapshot.StandaloneArchive{expired, live} {
		if err := store.SaveStandaloneArchive(ctx, archive); err != nil {
			t.Fatal(err)
		}
	}
	var blob, reason string
	if err := f.db.QueryRow(`select archive_blob, capture_reason from standalone_archives where id = ?`, live.ID.String()).Scan(&blob, &reason); err != nil || blob != `{"b":2}` || reason != "node-conversion" {
		t.Fatalf("archive = %q %q %v", blob, reason, err)
	}
	if err := store.SaveStandaloneArchive(ctx, live); err == nil {
		t.Fatal("duplicate archive id must fail")
	}
	removed, err := store.PurgeExpiredArchives(ctx, now)
	if err != nil || removed != 1 {
		t.Fatalf("purge = %d, %v", removed, err)
	}
	if n := phase2Count(t, f, `select count(*) from standalone_archives where id = ?`, live.ID.String()); n != 1 {
		t.Fatal("live archive purged")
	}
	if removed, err = store.PurgeExpiredArchives(ctx, now); err != nil || removed != 0 {
		t.Fatalf("second purge = %d, %v", removed, err)
	}
	// Zero now falls back to the wall clock, as in PostgreSQL.
	if removed, err = store.PurgeExpiredArchives(ctx, time.Time{}); err != nil || removed != 1 {
		t.Fatalf("wall clock purge = %d, %v", removed, err)
	}
}

func TestSQLitePhase2StandaloneArchiveValidation(t *testing.T) {
	_, store := newSQLitePhase2Fixture(t)
	ctx := context.Background()
	now := time.Now().UTC()
	valid := replicationsnapshot.StandaloneArchive{ID: uuid.New(), SnapshotData: []byte("{}"), Reason: "r", CapturedAt: now, ExpiresAt: now.Add(time.Hour)}
	mutate := func(fn func(*replicationsnapshot.StandaloneArchive)) replicationsnapshot.StandaloneArchive {
		archive := valid
		fn(&archive)
		return archive
	}
	for name, archive := range map[string]replicationsnapshot.StandaloneArchive{
		"no id":       mutate(func(a *replicationsnapshot.StandaloneArchive) { a.ID = uuid.Nil }),
		"no captured": mutate(func(a *replicationsnapshot.StandaloneArchive) { a.CapturedAt = time.Time{} }),
		"no expires":  mutate(func(a *replicationsnapshot.StandaloneArchive) { a.ExpiresAt = time.Time{} }),
		"no data":     mutate(func(a *replicationsnapshot.StandaloneArchive) { a.SnapshotData = nil }),
	} {
		if err := store.SaveStandaloneArchive(ctx, archive); err == nil || err.Error() != "archive is incomplete" {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	var nilCtx context.Context
	var nilStore *SQLitePhase2Store
	if err := store.SaveStandaloneArchive(nilCtx, valid); err == nil || err.Error() != "archive context is required" {
		t.Errorf("nil ctx err = %v", err)
	}
	if err := nilStore.SaveStandaloneArchive(ctx, valid); err == nil || err.Error() != "archive store is required" {
		t.Errorf("nil store err = %v", err)
	}
	if _, err := store.PurgeExpiredArchives(nilCtx, now); err == nil || err.Error() != "archive context is required" {
		t.Errorf("purge nil ctx err = %v", err)
	}
	if _, err := nilStore.PurgeExpiredArchives(ctx, now); err == nil || err.Error() != "archive store is required" {
		t.Errorf("purge nil store err = %v", err)
	}
}

func TestSQLitePhase2ListEnrollmentTokens(t *testing.T) {
	f, store := newSQLitePhase2Fixture(t)
	ctx := context.Background()
	ownerA, ownerB := uuid.NewString(), uuid.NewString()
	for i, owner := range []string{ownerA, ownerB} {
		settingsExec(t, f.db, `insert into users(id,username,password_hash,role,created_at,updated_at) values(?,?,'dummy','owner','0','0')`, owner, "owner-"+string(rune('a'+i)))
	}
	base := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	insertToken := func(id, owner string, created time.Time, extra string) {
		settingsExec(t, f.db, `insert into enrollment_tokens(id,token_selector,token_hash,hash_version,created_by_user_id,created_at,expires_at) values(?,?,'h',?,?,?,?)`,
			id, strings.ReplaceAll(id, "-", ""), EnrollmentTokenHashVersion, owner, tokenKey(t, created), tokenKey(t, created.Add(24*time.Hour)))
		if extra != "" {
			settingsExec(t, f.db, extra, id)
		}
	}
	older := "00000000-0000-4000-8000-000000000001"
	tieLow := "00000000-0000-4000-8000-000000000002"
	tieHigh := "00000000-0000-4000-8000-000000000003"
	foreign := "00000000-0000-4000-8000-000000000004"
	insertToken(older, ownerA, base, "")
	insertToken(tieLow, ownerA, base.Add(time.Hour), `update enrollment_tokens set revoked_at = '`+tokenKey(t, base.Add(2*time.Hour))+`' where id = ?`)
	insertToken(tieHigh, ownerA, base.Add(time.Hour), `update enrollment_tokens set consumed_at = '`+tokenKey(t, base.Add(3*time.Hour))+`' where id = ?`)
	insertToken(foreign, ownerB, base.Add(5*time.Hour), "")
	grantAt := base.Add(9 * time.Minute)
	settingsExec(t, f.db, `insert into enrollment_grants(attempt_id,token_id,installation_id,node_id,primary_id,primary_generation,node_ephemeral_public_key,sealed_bootstrap_payload,payload_hash,created_at,expires_at) values(?,?,?,?,?,1,'k','p','h',?,?)`,
		uuid.NewString(), older, uuid.NewString(), uuid.NewString(), f.primaryID, tokenKey(t, grantAt), tokenKey(t, grantAt.Add(time.Hour)))

	records, err := store.ListEnrollmentTokens(ctx, ownerA)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, record := range records {
		ids = append(ids, record.ID)
	}
	if got, want := strings.Join(ids, ","), strings.Join([]string{tieHigh, tieLow, older}, ","); got != want {
		t.Fatalf("order = %s, want %s", got, want)
	}
	if records[0].ConsumedAt == nil || !records[0].ConsumedAt.Equal(base.Add(3*time.Hour)) || records[0].RevokedAt != nil || records[0].AttemptCount != 0 || records[0].LastAttemptAt != nil {
		t.Errorf("consumed record = %#v", records[0])
	}
	if records[1].RevokedAt == nil || !records[1].RevokedAt.Equal(base.Add(2*time.Hour)) || records[1].ConsumedAt != nil {
		t.Errorf("revoked record = %#v", records[1])
	}
	last := records[2]
	if last.AttemptCount != 1 || last.LastAttemptAt == nil || !last.LastAttemptAt.Equal(base.Add(9*time.Minute)) ||
		last.Selector != strings.ReplaceAll(older, "-", "") || !last.CreatedAt.Equal(base) || !last.ExpiresAt.Equal(base.Add(24*time.Hour)) {
		t.Errorf("attempt record = %#v", last)
	}
	only, err := store.ListEnrollmentTokens(ctx, strings.ToUpper(ownerB))
	if err != nil || len(only) != 1 || only[0].ID != foreign {
		t.Fatalf("owner B = %v, %v", only, err)
	}
	none, err := store.ListEnrollmentTokens(ctx, uuid.NewString())
	if err != nil || none == nil || len(none) != 0 {
		t.Fatalf("unknown owner = %#v, %v", none, err)
	}
}

func TestSQLitePhase2ListEnrollmentTokensErrors(t *testing.T) {
	_, store := newSQLitePhase2Fixture(t)
	var nilCtx context.Context
	var nilStore *SQLitePhase2Store
	for name, err := range map[string]error{
		"nil ctx":   second2(store.ListEnrollmentTokens(nilCtx, uuid.NewString())),
		"nil store": second2(nilStore.ListEnrollmentTokens(context.Background(), uuid.NewString())),
		"blank":     second2(store.ListEnrollmentTokens(context.Background(), "  ")),
	} {
		if err == nil || err.Error() != "enrollment token list unavailable" {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if _, err := store.ListEnrollmentTokens(context.Background(), "not-a-uuid"); err == nil || err.Error() == "enrollment token list unavailable" {
		t.Errorf("malformed owner err = %v", err)
	}
}

func second2[T any](_ T, err error) error { return err }

func TestSQLitePhase2ClosedDatabase(t *testing.T) {
	f, store := newSQLitePhase2Fixture(t)
	ctx := context.Background()
	body, _ := publicationEnvelopeBody(t, f.primaryID)
	if err := f.db.Close(); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	archive := replicationsnapshot.StandaloneArchive{ID: uuid.New(), SnapshotData: []byte("{}"), Reason: "r", CapturedAt: now, ExpiresAt: now.Add(time.Hour)}
	for name, err := range map[string]error{
		"with canonical":  store.WithCanonicalSnapshot(ctx, func(CanonicalSnapshotTransaction) error { return nil }),
		"create job":      second(store.CreateApplyJobFromSnapshot(ctx, "import", body)),
		"save archive":    store.SaveStandaloneArchive(ctx, archive),
		"purge archives":  second2(store.PurgeExpiredArchives(ctx, now)),
		"list tokens":     second2(store.ListEnrollmentTokens(ctx, uuid.NewString())),
		"list candidates": second2(store.ListCanonicalSnapshotCandidates(ctx)),
	} {
		if err == nil {
			t.Errorf("%s: closed database must fail", name)
		}
	}
}
