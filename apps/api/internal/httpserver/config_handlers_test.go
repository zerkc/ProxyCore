package httpserver_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/zerkc/ProxyCore/apps/api/internal/auth"
	"github.com/zerkc/ProxyCore/apps/api/internal/config"
	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/httpserver"
)

func TestConfigurationRoutesFlow(t *testing.T) {
	srv := newConfigTestServer(t)
	handler := srv.Handler()

	// Unauthenticated status is rejected with the Node {error} shape.
	unauth := doJSON(t, handler, http.MethodGet, "/api/status", nil, nil)
	if unauth.Code != http.StatusUnauthorized {
		t.Fatalf("unauth status=%d body=%s", unauth.Code, unauth.Body.String())
	}
	var unauthBody map[string]any
	_ = json.Unmarshal(unauth.Body.Bytes(), &unauthBody)
	if unauthBody["error"] != "Authentication required" {
		t.Fatalf("unauth body=%v", unauthBody)
	}

	// Bootstrap owner and log in.
	bootstrap := doJSON(t, handler, http.MethodPost, "/api/auth/bootstrap", map[string]any{
		"username": "owner",
		"password": "correct horse battery staple",
	}, nil)
	if bootstrap.Code != http.StatusCreated {
		t.Fatalf("bootstrap status=%d body=%s", bootstrap.Code, bootstrap.Body.String())
	}
	login := doJSON(t, handler, http.MethodPost, "/api/auth/login", map[string]any{
		"username": "owner",
		"password": "correct horse battery staple",
	}, nil)
	if login.Code != http.StatusOK {
		t.Fatalf("login status=%d body=%s", login.Code, login.Body.String())
	}
	cookie := login.Result().Cookies()[0]

	// Settings round-trip with a default resolver pool.
	putSettings := doJSON(t, handler, http.MethodPut, "/api/settings", map[string]any{
		"defaultPool": map[string]any{
			"id":        "default",
			"endpoints": []map[string]any{{"host": "1.1.1.1", "port": 53}},
		},
	}, cookie)
	if putSettings.Code != http.StatusOK {
		t.Fatalf("put settings status=%d body=%s", putSettings.Code, putSettings.Body.String())
	}

	getSettings := doJSON(t, handler, http.MethodGet, "/api/settings", nil, cookie)
	if getSettings.Code != http.StatusOK {
		t.Fatalf("get settings status=%d body=%s", getSettings.Code, getSettings.Body.String())
	}
	var settingsBody struct {
		Settings domain.Settings `json:"settings"`
	}
	if err := json.Unmarshal(getSettings.Body.Bytes(), &settingsBody); err != nil {
		t.Fatalf("decode settings: %v", err)
	}
	if settingsBody.Settings.DefaultPool == nil || settingsBody.Settings.DefaultPool.ID != "default" {
		t.Fatalf("settings=%+v", settingsBody.Settings)
	}

	// Create a zone; the response carries the enqueued apply job.
	createZone := doJSON(t, handler, http.MethodPost, "/api/zones", map[string]any{"name": "example.test"}, cookie)
	if createZone.Code != http.StatusCreated {
		t.Fatalf("create zone status=%d body=%s", createZone.Code, createZone.Body.String())
	}
	var zoneBody struct {
		Zone  domain.ZoneState          `json:"zone"`
		Apply configuration.ApplyResult `json:"apply"`
	}
	if err := json.Unmarshal(createZone.Body.Bytes(), &zoneBody); err != nil {
		t.Fatalf("decode zone: %v", err)
	}
	if zoneBody.Zone.Name != "example.test" || zoneBody.Apply.Job.ID == "" {
		t.Fatalf("zone=%+v apply=%+v", zoneBody.Zone, zoneBody.Apply)
	}

	// Add an A record to the zone.
	addRecord := doJSON(t, handler, http.MethodPost, "/api/zones/"+zoneBody.Zone.ID+"/records", map[string]any{
		"name":  "www",
		"type":  "A",
		"value": "10.0.0.5",
	}, cookie)
	if addRecord.Code != http.StatusCreated {
		t.Fatalf("add record status=%d body=%s", addRecord.Code, addRecord.Body.String())
	}

	// Streams: create then patch without triggering an apply job.
	createStream := doJSON(t, handler, http.MethodPost, "/api/streams", map[string]any{
		"protocol":      "tcp",
		"listenAddress": "0.0.0.0",
		"listenPort":    8443,
		"upstream":      map[string]any{"ip": "10.0.0.9", "port": 443, "protocol": "tcp"},
	}, cookie)
	if createStream.Code != http.StatusCreated {
		t.Fatalf("create stream status=%d body=%s", createStream.Code, createStream.Body.String())
	}
	var streamBody struct {
		Stream domain.StreamRoute `json:"stream"`
	}
	if err := json.Unmarshal(createStream.Body.Bytes(), &streamBody); err != nil {
		t.Fatalf("decode stream: %v", err)
	}
	patchStream := doJSON(t, handler, http.MethodPatch, "/api/streams/"+streamBody.Stream.ID, map[string]any{"enabled": false}, cookie)
	if patchStream.Code != http.StatusOK {
		t.Fatalf("patch stream status=%d body=%s", patchStream.Code, patchStream.Body.String())
	}

	// Apply returns 202 with a revision + job.
	apply := doJSON(t, handler, http.MethodPost, "/api/apply", nil, cookie)
	if apply.Code != http.StatusAccepted {
		t.Fatalf("apply status=%d body=%s", apply.Code, apply.Body.String())
	}
	var applyBody configuration.ApplyResult
	if err := json.Unmarshal(apply.Body.Bytes(), &applyBody); err != nil {
		t.Fatalf("decode apply: %v", err)
	}
	if applyBody.RevisionID == "" || applyBody.Job.ID == "" {
		t.Fatalf("apply body=%+v", applyBody)
	}

	// Owner-only users listing works for the owner.
	listUsers := doJSON(t, handler, http.MethodGet, "/api/users", nil, cookie)
	if listUsers.Code != http.StatusOK {
		t.Fatalf("list users status=%d body=%s", listUsers.Code, listUsers.Body.String())
	}

	// Status aggregates the current state.
	status := doJSON(t, handler, http.MethodGet, "/api/status", nil, cookie)
	if status.Code != http.StatusOK {
		t.Fatalf("status status=%d body=%s", status.Code, status.Body.String())
	}
}

func newConfigTestServer(t *testing.T) *httpserver.Server {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping database-backed configuration handler tests in short mode")
	}
	dsn := os.Getenv("DATABASE_URL")
	if strings.TrimSpace(dsn) == "" {
		t.Skip("DATABASE_URL is not set")
	}

	ctx := context.Background()
	adminPool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect admin db: %v", err)
	}
	t.Cleanup(adminPool.Close)

	schema := "proxycore_go_config_test_" +
		regexp.MustCompile(`[^a-z0-9]+`).ReplaceAllString(strings.ToLower(t.Name()), "_") +
		"_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	if _, err := adminPool.Exec(ctx, "create schema "+(pgx.Identifier{schema}).Sanitize()); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = adminPool.Exec(context.Background(), "drop schema if exists "+(pgx.Identifier{schema}).Sanitize()+" cascade")
	})

	poolCfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse db config: %v", err)
	}
	if poolCfg.ConnConfig.RuntimeParams == nil {
		poolCfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	poolCfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		t.Fatalf("connect schema db: %v", err)
	}
	t.Cleanup(pool.Close)

	authStore := auth.NewPostgresStore(pool)
	if err := authStore.EnsureSchema(ctx); err != nil {
		t.Fatalf("ensure auth schema: %v", err)
	}
	if err := configuration.EnsureSchema(ctx, pool); err != nil {
		t.Fatalf("ensure configuration schema: %v", err)
	}
	if _, err := pool.Exec(ctx, `insert into installation_settings (id) values ('default') on conflict do nothing`); err != nil {
		t.Fatalf("seed installation settings: %v", err)
	}

	svc := auth.NewService(authStore, auth.ServiceOptions{SessionTTL: time.Hour})
	configStore := configuration.New(pool, "", domain.Ingress{})
	return httpserver.New(config.Config{
		UIDist:            t.TempDir(),
		SessionCookieName: "proxycore_session",
		SessionTTL:        time.Hour,
	}, log.New(io.Discard, "", 0),
		httpserver.WithAuthService(svc),
		httpserver.WithConfigurationStore(configStore),
	)
}

func doJSON(t *testing.T, handler http.Handler, method, target string, body any, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(payload)
	}
	req := httptest.NewRequest(method, target, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}
