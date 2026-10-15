package httpserver_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/httpserver"
	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
	syncpublication "github.com/zerkc/ProxyCore/apps/api/internal/sync"
)

type topologySyncFixture struct {
	env     *sqliteHTTPEnv
	owner   *http.Cookie
	store   *configuration.SQLiteNodeSyncStore
	retry   func(context.Context) error
	retries int
}

func newTopologySyncFixture(t *testing.T, published string) *topologySyncFixture {
	t.Helper()
	f := &topologySyncFixture{}
	var store *configuration.SQLiteNodeSyncStore
	f.env = newSQLiteHTTPTestServer(t, httpserver.WithTopologySync(httpserver.TopologySyncOptions{
		LoadState: func(ctx context.Context) (configuration.NodeSyncState, bool, error) {
			return store.LoadNodeSyncState(ctx)
		},
		PublishedHash: func(context.Context) (string, bool, error) {
			return published, published != "", nil
		},
		Retry: func(ctx context.Context) error {
			f.retries++
			if f.retry != nil {
				return f.retry(ctx)
			}
			return nil
		},
		Interval: 30 * time.Second,
	}))
	store = configuration.NewSQLiteNodeSyncStore(sqlitedb.NewWriter(f.env.DB, nil))
	f.store = store
	f.owner = sqliteOwnerCookie(t, f.env.Handler)
	return f
}

func (f *topologySyncFixture) becomeNode(t *testing.T) {
	t.Helper()
	if _, err := f.env.Identity.TransitionTo(context.Background(), domain.TopologyRoleNode); err != nil {
		t.Fatal(err)
	}
}

func (f *topologySyncFixture) bind(t *testing.T, applied, primary string) {
	t.Helper()
	ctx := context.Background()
	if err := f.store.BindNodeSync(ctx, configuration.NodeSyncBinding{
		PrimaryURL: "https://primary.example:3443", CredentialID: uuid.NewString(),
	}, applied, time.Unix(1_800_000_000, 0).UTC()); err != nil {
		t.Fatalf("bind: %v", err)
	}
	state, _, err := f.store.LoadNodeSyncState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	state.PrimaryHash = primary
	state.LastAttemptAt = time.Unix(1_800_000_100, 0).UTC()
	state.NextAttemptAt = time.Unix(1_800_000_130, 0).UTC()
	if err := f.store.SaveNodeSyncProgress(ctx, state.NodeSyncProgress); err != nil {
		t.Fatal(err)
	}
}

func hashOf(c string) string { return strings.Repeat(c, 64) }

func TestTopologySyncStatusRequiresOwner(t *testing.T) {
	f := newTopologySyncFixture(t, "")
	if rec := doJSON(t, f.env.Handler, http.MethodGet, httpserver.TopologySyncPath, nil, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous status=%d", rec.Code)
	}
	if rec := doJSON(t, f.env.Handler, http.MethodPost, httpserver.TopologySyncRetryPath, map[string]any{}, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous retry status=%d", rec.Code)
	}
}

func TestTopologySyncStatusReportsOutdatedNode(t *testing.T) {
	f := newTopologySyncFixture(t, "")
	f.becomeNode(t)
	f.bind(t, hashOf("a"), hashOf("b"))

	rec := doJSON(t, f.env.Handler, http.MethodGet, httpserver.TopologySyncPath, nil, f.owner)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := decodeBody(t, rec)
	if body["role"] != "node" || body["appliedHash"] != hashOf("a") || body["primaryHash"] != hashOf("b") || body["outdated"] != true {
		t.Fatalf("body=%v", body)
	}
	for _, key := range []string{"lastSyncAt", "lastAttemptAt", "nextAttemptAt"} {
		if s, _ := body[key].(string); s == "" {
			t.Fatalf("%s missing in %v", key, body)
		}
	}
	if body["lastError"] != nil {
		t.Fatalf("lastError=%v", body["lastError"])
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("cache-control=%q", rec.Header().Get("Cache-Control"))
	}
}

func TestTopologySyncStatusCurrentNodeIsNotOutdated(t *testing.T) {
	f := newTopologySyncFixture(t, "")
	f.becomeNode(t)
	f.bind(t, hashOf("a"), hashOf("a"))
	body := decodeBody(t, doJSON(t, f.env.Handler, http.MethodGet, httpserver.TopologySyncPath, nil, f.owner))
	if body["outdated"] != false || body["appliedHash"] != hashOf("a") {
		t.Fatalf("body=%v", body)
	}
}

func TestTopologySyncStatusPrimaryExposesPublishedHash(t *testing.T) {
	f := newTopologySyncFixture(t, hashOf("c"))
	rec := doJSON(t, f.env.Handler, http.MethodGet, httpserver.TopologySyncPath, nil, f.owner)
	body := decodeBody(t, rec)
	if rec.Code != http.StatusOK || body["role"] == "node" || body["primaryHash"] != hashOf("c") || body["outdated"] != false {
		t.Fatalf("status=%d body=%v", rec.Code, body)
	}
}

func TestTopologySyncRetryRunsOneCycle(t *testing.T) {
	f := newTopologySyncFixture(t, "")
	f.becomeNode(t)
	f.bind(t, hashOf("a"), hashOf("a"))
	rec := doJSON(t, f.env.Handler, http.MethodPost, httpserver.TopologySyncRetryPath, map[string]any{}, f.owner)
	if rec.Code != http.StatusOK || f.retries != 1 {
		t.Fatalf("status=%d retries=%d body=%s", rec.Code, f.retries, rec.Body.String())
	}
	if body := decodeBody(t, rec); body["ok"] != true || body["sync"] == nil {
		t.Fatalf("body=%v", body)
	}
}

func TestTopologySyncRetryMapsOutcomes(t *testing.T) {
	cases := []struct {
		name string
		err  error
		code int
		want string
	}{
		{"not node", syncpublication.ErrNodeSyncNotNode, http.StatusConflict, "NOT_A_NODE"},
		{"unbound", syncpublication.ErrNodeSyncUnbound, http.StatusConflict, "NODE_NOT_BOUND"},
		{"busy", syncpublication.ErrNodeSyncBusy, http.StatusConflict, "SYNC_IN_PROGRESS"},
		{"failure", &syncpublication.NodeSyncFailure{Code: "primary_unavailable", Message: "The primary could not be reached."}, http.StatusBadGateway, "primary_unavailable"},
		{"unexpected", errors.New("secret pcnode1_xxx leaked"), http.StatusInternalServerError, "SYNC_FAILED"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newTopologySyncFixture(t, "")
			f.retry = func(context.Context) error { return tc.err }
			rec := doJSON(t, f.env.Handler, http.MethodPost, httpserver.TopologySyncRetryPath, map[string]any{}, f.owner)
			body := decodeBody(t, rec)
			if rec.Code != tc.code || body["code"] != tc.want || body["ok"] != false {
				t.Fatalf("status=%d body=%v", rec.Code, body)
			}
			if strings.Contains(rec.Body.String(), "pcnode1_") {
				t.Fatalf("response leaked error text: %s", rec.Body.String())
			}
		})
	}
}

func TestTopologyPromoteWarnsWhenNodeIsOutdatedOrNeverSynced(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(*testing.T, *topologySyncFixture)
		want    string
	}{
		{"outdated", func(t *testing.T, f *topologySyncFixture) { f.bind(t, hashOf("a"), hashOf("b")) }, "NODE_OUTDATED"},
		{"never synced", func(t *testing.T, f *topologySyncFixture) {}, "NODE_NEVER_SYNCED"},
		{"current", func(t *testing.T, f *topologySyncFixture) { f.bind(t, hashOf("a"), hashOf("a")) }, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newTopologySyncFixture(t, "")
			f.becomeNode(t)
			tc.prepare(t, f)
			rec := doJSON(t, f.env.Handler, http.MethodPost, httpserver.TopologyPromotePath, map[string]any{"confirm": true}, f.owner)
			if rec.Code != http.StatusOK {
				t.Fatalf("promote must not be blocked: status=%d body=%s", rec.Code, rec.Body.String())
			}
			body := decodeBody(t, rec)
			warning, _ := body["syncWarning"].(map[string]any)
			if tc.want == "" {
				if body["syncWarning"] != nil {
					t.Fatalf("unexpected syncWarning %v", body["syncWarning"])
				}
				return
			}
			if warning["code"] != tc.want || warning["message"] == "" {
				t.Fatalf("syncWarning=%v", body["syncWarning"])
			}
			if body["warning"] == nil {
				t.Fatal("split-brain warning must still be present")
			}
		})
	}
}

func TestPromoteClearsNodeSyncBinding(t *testing.T) {
	f := newTopologySyncFixture(t, "")
	f.becomeNode(t)
	f.bind(t, hashOf("a"), hashOf("a"))
	if rec := doJSON(t, f.env.Handler, http.MethodPost, httpserver.TopologyPromotePath, map[string]any{"confirm": true}, f.owner); rec.Code != http.StatusOK {
		t.Fatalf("promote status=%d", rec.Code)
	}
	if _, bound, err := f.store.LoadNodeSyncState(context.Background()); err != nil || bound {
		t.Fatalf("bound=%v err=%v after promote", bound, err)
	}
	var n int
	if err := f.env.DB.QueryRow(`select count(*) from health_observations where component = 'node-sync'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("progress rows after promote = %d err=%v", n, err)
	}
}

func TestTopologySyncStatusExposesUnlinkedOnlyWhenPersisted(t *testing.T) {
	f := newTopologySyncFixture(t, "")
	f.becomeNode(t)
	f.bind(t, hashOf("a"), hashOf("a"))
	body := decodeBody(t, doJSON(t, f.env.Handler, http.MethodGet, httpserver.TopologySyncPath, nil, f.owner))
	if body["unlinked"] != false || body["unlinkedAt"] != nil {
		t.Fatalf("linked node body=%v", body)
	}

	state, _, err := f.store.LoadNodeSyncState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	state.Unlinked, state.UnlinkedAt = true, time.Unix(1_800_000_500, 0).UTC()
	state.LastError = "The primary unlinked this node."
	if err := f.store.SaveNodeSyncProgress(context.Background(), state.NodeSyncProgress); err != nil {
		t.Fatal(err)
	}
	body = decodeBody(t, doJSON(t, f.env.Handler, http.MethodGet, httpserver.TopologySyncPath, nil, f.owner))
	if body["unlinked"] != true || body["bound"] != true || body["appliedHash"] != hashOf("a") {
		t.Fatalf("unlinked node body=%v", body)
	}
	if at, _ := body["unlinkedAt"].(string); !strings.HasPrefix(at, "2027-01-") {
		t.Fatalf("unlinkedAt=%v", body["unlinkedAt"])
	}
}
