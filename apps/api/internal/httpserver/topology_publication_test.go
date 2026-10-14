package httpserver_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/httpserver"
	syncpublication "github.com/zerkc/ProxyCore/apps/api/internal/sync"
)

// applyRecorder wraps the real backend and records the apply jobs the promote
// workflow enqueues, optionally failing them.
type applyRecorder struct {
	httpserver.ConfigurationBackend
	mu     sync.Mutex
	actors []string
	fail   error
}

func (r *applyRecorder) CreateApplyJob(ctx context.Context, actor string) (configuration.ApplyResult, error) {
	r.mu.Lock()
	r.actors = append(r.actors, actor)
	fail := r.fail
	r.mu.Unlock()
	if fail != nil {
		return configuration.ApplyResult{}, fail
	}
	return r.ConfigurationBackend.CreateApplyJob(ctx, actor)
}

func (r *applyRecorder) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.actors)
}

// promoteFixture is a node with one applied configuration (a default pool and
// a zone) whose promote is observed through an applyRecorder.
type promoteFixture struct {
	env      *sqliteHTTPEnv
	owner    *http.Cookie
	recorder *applyRecorder
}

func newPromoteFixture(t *testing.T, fail error) *promoteFixture {
	t.Helper()
	recorder := &applyRecorder{fail: fail}
	env := newSQLiteHTTPTestServer(t, httpserver.WithConfigurationStore(recorder))
	recorder.ConfigurationBackend = env.Config
	owner := sqliteOwnerCookie(t, env.Handler)
	if rec := doJSON(t, env.Handler, http.MethodPut, "/api/settings", map[string]any{
		"defaultPool": map[string]any{"id": "default", "endpoints": []map[string]any{{"host": "1.1.1.1", "port": 53}}},
	}, owner); rec.Code != http.StatusOK {
		t.Fatalf("settings status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := doJSON(t, env.Handler, http.MethodPost, "/api/zones", map[string]any{"name": "example.test"}, owner); rec.Code != http.StatusCreated {
		t.Fatalf("zone status=%d body=%s", rec.Code, rec.Body.String())
	}
	if _, err := env.Identity.TransitionTo(context.Background(), domain.TopologyRoleNode); err != nil {
		t.Fatal(err)
	}
	// Only the jobs enqueued by promote are of interest.
	recorder.mu.Lock()
	recorder.actors = nil
	recorder.mu.Unlock()
	return &promoteFixture{env: env, owner: owner, recorder: recorder}
}

func (f *promoteFixture) promote(t *testing.T) map[string]any {
	t.Helper()
	rec := doJSON(t, f.env.Handler, http.MethodPost, httpserver.TopologyPromotePath, map[string]any{"confirm": true}, f.owner)
	if rec.Code != http.StatusOK {
		t.Fatalf("promote status=%d body=%s", rec.Code, rec.Body.String())
	}
	return decodeBody(t, rec)
}

func TestPromoteEnqueuesApplyOfCurrentConfigurationForPublication(t *testing.T) {
	f := newPromoteFixture(t, nil)
	body := f.promote(t)
	if f.recorder.callCount() != 1 {
		t.Fatalf("apply jobs enqueued by promote = %d, want 1", f.recorder.callCount())
	}
	publication, _ := body["publication"].(map[string]any)
	if publication["status"] != "queued" {
		t.Fatalf("publication=%v", body["publication"])
	}
}

func TestPromotePublicationFailureDoesNotUndoPromoteAndIsReported(t *testing.T) {
	f := newPromoteFixture(t, errors.New("database is locked: secret-detail"))
	body := f.promote(t)
	if got := f.env.Identity.Current().Role; got != domain.TopologyRolePrimary {
		t.Fatalf("role after failed publication = %s", got)
	}
	if f.recorder.callCount() < 2 {
		t.Fatalf("enqueue must be retried, calls=%d", f.recorder.callCount())
	}
	publication, _ := body["publication"].(map[string]any)
	if publication["status"] != "failed" || publication["code"] != "PUBLICATION_ENQUEUE_FAILED" {
		t.Fatalf("publication=%v", body["publication"])
	}
	if message, _ := publication["message"].(string); strings.Contains(message, "secret-detail") {
		t.Fatalf("error text leaked: %v", publication)
	}
}

// After promote the pipeline must publish a snapshot for the new generation
// whose hash is the hash of the configuration the node had applied, without any
// further apply.
func TestPromotePublishesSnapshotOfAppliedConfigurationForNewGeneration(t *testing.T) {
	f := newPromoteFixture(t, nil)
	ctx := context.Background()
	f.env.Identity.SetClusterKeyProvisioner(func(ctx context.Context) (uuid.UUID, error) {
		material, err := f.env.Keys.LoadOrCreate(ctx, nil)
		if err != nil {
			return uuid.Nil, err
		}
		defer material.Destroy()
		return material.ID, nil
	})
	previous := f.env.Identity.Current().LeadershipGeneration
	var before int
	if err := f.env.DB.QueryRow(`select count(*) from apply_jobs`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	f.promote(t)
	var after int
	if err := f.env.DB.QueryRow(`select count(*) from apply_jobs`).Scan(&after); err != nil || after != before+1 {
		t.Fatalf("apply jobs before=%d after=%d err=%v, promote must enqueue exactly one", before, after, err)
	}
	current := f.env.Identity.Current()
	if current.ClusterKeyID == nil {
		t.Fatal("promote must leave the new generation with a cluster key")
	}

	// The worker is out of scope: complete the queued job the way it would.
	var jobID, revisionID string
	if err := f.env.DB.QueryRow(`select id, revision_id from apply_jobs where status = 'queued' order by created_at desc limit 1`).
		Scan(&jobID, &revisionID); err != nil {
		t.Fatalf("no queued apply job after promote: %v", err)
	}
	for _, stmt := range []string{
		`update apply_jobs set status = 'applied', started_at = created_at, finished_at = created_at where id = ?`,
		`update config_revisions set applied_at = coalesce(applied_at, created_at) where id = ?`,
	} {
		arg := jobID
		if stmt[:6] == "update" && stmt[7:23] == "config_revisions" {
			arg = revisionID
		}
		if _, err := f.env.DB.Exec(stmt, arg); err != nil {
			t.Fatal(err)
		}
	}

	producer := syncpublication.NewCanonicalSnapshotProducer(f.env.Phase2, syncpublication.CanonicalSnapshotProducerOptions{})
	published, err := producer.Reconcile(ctx)
	if err != nil || published < 1 {
		t.Fatalf("reconcile published=%d err=%v", published, err)
	}
	hash, ok, err := f.env.Phase2.LatestPublishedContentHash(ctx)
	if err != nil || !ok || hash == "" {
		t.Fatalf("latest published hash=%q ok=%v err=%v", hash, ok, err)
	}
	var generation int64
	var rowHash string
	if err := f.env.DB.QueryRow(`select leadership_generation, content_hash from applied_snapshots where apply_job_id = ?`, jobID).
		Scan(&generation, &rowHash); err != nil {
		t.Fatalf("no snapshot published for the promote job: %v", err)
	}
	if uint64(generation) != uint64(current.LeadershipGeneration) || uint64(generation) != uint64(previous)+1 || rowHash != hash {
		t.Fatalf("generation=%d (want %d) rowHash=%q latest=%q", generation, current.LeadershipGeneration, rowHash, hash)
	}
}
