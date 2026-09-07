package httpserver_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	"github.com/zerkc/ProxyCore/apps/api/internal/httpserver"
	"github.com/zerkc/ProxyCore/apps/api/internal/update"
	"github.com/zerkc/ProxyCore/apps/api/internal/version"
)

// mockUpdaterClient implements httpserver.UpdaterClient for tests.
type mockUpdaterClient struct {
	applyFunc func(ctx context.Context, targetVersion string) (int, error)
}

func (m *mockUpdaterClient) Apply(ctx context.Context, targetVersion string) (int, error) {
	return m.applyFunc(ctx, targetVersion)
}

func TestUpdatesWithoutCheckerAreDisabled(t *testing.T) {
	server := httpserver.New(config.Config{UIDist: t.TempDir()}, nil)
	request := httptest.NewRequest(http.MethodGet, "/api/updates", nil)
	response := httptest.NewRecorder()

	server.Handler().ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status=%d", response.Code)
	}
	var result update.Result
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != update.StatusDisabled {
		t.Fatalf("status=%q, want %q", result.Status, update.StatusDisabled)
	}
	if result.CurrentVersion != version.Version {
		t.Fatalf("currentVersion=%q, want %q", result.CurrentVersion, version.Version)
	}
}

func TestUpdatesEndpointReturnsCheckerResult(t *testing.T) {
	github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/zerkc/ProxyCore/releases/latest" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"tag_name":     "v0.2.0",
			"html_url":     "https://github.com/zerkc/ProxyCore/releases/tag/v0.2.0",
			"published_at": "2026-08-24T12:00:00Z",
		})
	}))
	t.Cleanup(github.Close)

	checker := update.NewChecker(update.CheckerOptions{
		CurrentVersion: "0.1.0",
		Enabled:        true,
		BaseURL:        github.URL,
		Client:         github.Client(),
	})
	server := httpserver.New(
		config.Config{UIDist: t.TempDir()},
		nil,
		httpserver.WithUpdateChecker(checker),
	)
	request := httptest.NewRequest(http.MethodGet, "/api/updates", nil)
	response := httptest.NewRecorder()

	server.Handler().ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var result update.Result
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != update.StatusUpdateAvailable {
		t.Fatalf("status=%q, want %q", result.Status, update.StatusUpdateAvailable)
	}
	if !result.UpdateAvailable {
		t.Fatal("updateAvailable=false, want true")
	}
	if result.Latest == nil || result.Latest.Version != "0.2.0" {
		t.Fatalf("latest=%+v", result.Latest)
	}
	if result.CheckedAt == nil {
		t.Fatal("checkedAt is nil")
	}
	if result.Latest.PublishedAt == nil {
		t.Fatal("publishedAt is nil")
	}
	if result.Latest.URL == "" {
		t.Fatal("latest URL is empty")
	}
}

// applyTestEnv sets up an isolated auth schema and returns components needed
// to construct a server with auth + optional updater mock. Returns nil if
// DATABASE_URL is not available so callers skip.
type applyTestEnv struct {
	pool   *pgxpool.Pool
	schema string
}

func setupApplyEnv(t *testing.T) *applyTestEnv {
	if testing.Short() {
		t.Skip("skipping database-backed test in short mode")
	}
	dsn := strings.TrimSpace(strings.SplitN(os.Getenv("DATABASE_URL"), "\x00", 2)[0])
	if dsn == "" {
		t.Skip("DATABASE_URL is not set")
	}

	ctx := context.Background()
	adminPool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Skip("cannot connect to database: " + err.Error())
	}

	schema := "proxycore_go_apply_test_" +
		regexp.MustCompile(`[^a-z0-9]+`).ReplaceAllString(strings.ToLower(t.Name()), "_") +
		"_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	if _, err := adminPool.Exec(ctx, "create schema "+(pgx.Identifier{schema}).Sanitize()); err != nil {
		adminPool.Close()
		t.Skip("cannot create schema: " + err.Error())
	}

	poolCfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		_, _ = adminPool.Exec(context.Background(), "drop schema if exists "+(pgx.Identifier{schema}).Sanitize()+" cascade")
		adminPool.Close()
		t.Fatalf("parse db config: %v", err)
	}
	if poolCfg.ConnConfig.RuntimeParams == nil {
		poolCfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	poolCfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		_, _ = adminPool.Exec(context.Background(), "drop schema if exists "+(pgx.Identifier{schema}).Sanitize()+" cascade")
		adminPool.Close()
		t.Fatalf("connect schema db: %v", err)
	}

	store := auth.NewPostgresStore(pool)
	if err := store.EnsureSchema(ctx); err != nil {
		pool.Close()
		_, _ = adminPool.Exec(context.Background(), "drop schema if exists "+(pgx.Identifier{schema}).Sanitize()+" cascade")
		adminPool.Close()
		t.Fatalf("ensure auth schema: %v", err)
	}

	t.Cleanup(func() {
		pool.Close()
		_, _ = adminPool.Exec(context.Background(), "drop schema if exists "+(pgx.Identifier{schema}).Sanitize()+" cascade")
		adminPool.Close()
	})

	return &applyTestEnv{pool: pool, schema: schema}
}

// buildApplyServer creates a server with auth + optional updater mock using
// an isolated schema. ownerRole controls whether the bootstrapped user is
// an owner or operator.
func buildApplyServer(t *testing.T, env *applyTestEnv, updaterClient httpserver.UpdaterClient, ownerRole bool) http.Handler {
	t.Helper()
	svc := auth.NewService(auth.NewPostgresStore(env.pool), auth.ServiceOptions{SessionTTL: time.Hour})

	username := "owneruser"
	role := auth.RoleOwner
	if !ownerRole {
		username = "operatoruser"
		role = auth.RoleOperator
	}

	// Bootstrap the user directly into the store so we control the role.
	_, err := svc.CreateUser(context.Background(), auth.User{}, username, "correct horse battery staple", role)
	if err != nil {
		t.Fatalf("create %s user: %v", role, err)
	}

	opts := []httpserver.Option{
		httpserver.WithAuthService(svc),
	}
	if updaterClient != nil {
		opts = append(opts, httpserver.WithUpdaterClient(updaterClient))
	}

	return httpserver.New(config.Config{
		UIDist:            t.TempDir(),
		SessionCookieName: "proxycore_session",
		SessionTTL:        time.Hour,
	}, nil, opts...).Handler()
}

// loginAs performs a login and returns the session cookie.
func loginAs(t *testing.T, handler http.Handler, username, password string) *http.Cookie {
	t.Helper()
	payload, _ := json.Marshal(map[string]string{"username": username, "password": password})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("login status=%d body=%s", rec.Code, rec.Body.String())
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("expected 1 cookie, got %d", len(cookies))
	}
	return cookies[0]
}

// applyRequest builds a POST /api/updates/apply request with the given version.
func applyRequest(targetVersion string) (*http.Request, *httptest.ResponseRecorder) {
	payload, _ := json.Marshal(map[string]string{"targetVersion": targetVersion})
	req := httptest.NewRequest(http.MethodPost, "/api/updates/apply", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	return req, httptest.NewRecorder()
}

func TestUpdateApplyUnauthenticatedReturns401(t *testing.T) {
	env := setupApplyEnv(t)
	if env == nil {
		return
	}
	// Auth service is configured so requireUser reaches the session-check path
	// (returns 401 for missing session), rather than short-circuiting with 503
	// "auth is not configured".
	handler := buildApplyServer(t, env, nil, true)

	req, rec := applyRequest("v0.2.0") // no session cookie
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestUpdateApplyOperatorRoleReturns403(t *testing.T) {
	env := setupApplyEnv(t)
	if env == nil {
		return
	}
	mockClient := &mockUpdaterClient{applyFunc: func(ctx context.Context, s string) (int, error) {
		return http.StatusAccepted, nil
	}}
	handler := buildApplyServer(t, env, mockClient, false) // operator role

	cookie := loginAs(t, handler, "operatoruser", "correct horse battery staple")

	req, rec := applyRequest("v0.3.0")
	req.AddCookie(cookie)
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want %d body=%s", rec.Code, http.StatusForbidden, rec.Body.String())
	}
}

func TestUpdateApplyNoClientReturns503(t *testing.T) {
	env := setupApplyEnv(t)
	if env == nil {
		return
	}
	// Auth service must be configured with an owner session so the handler
	// reaches the nil-updaterClient check (503 "updater is not configured")
	// rather than short-circuiting at auth (401/503 "auth is not configured").
	handler := buildApplyServer(t, env, nil, true) // nil updater, owner role
	cookie := loginAs(t, handler, "owneruser", "correct horse battery staple")

	req, rec := applyRequest("v0.2.0")
	req.AddCookie(cookie)
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want %d body=%s", rec.Code, http.StatusServiceUnavailable, rec.Body.String())
	}
}

func TestUpdateApplyInvalidJSONReturns400(t *testing.T) {
	// requireUser is called before body decode; auth must be configured.
	env := setupApplyEnv(t)
	if env == nil {
		return
	}
	handler := buildApplyServer(t, env, nil, true)
	cookie := loginAs(t, handler, "owneruser", "correct horse battery staple")

	req := httptest.NewRequest(http.MethodPost, "/api/updates/apply", strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want %d body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}

func TestUpdateApplyMissingTargetVersionReturns400(t *testing.T) {
	// requireUser is called before body decode; auth must be configured.
	env := setupApplyEnv(t)
	if env == nil {
		return
	}
	handler := buildApplyServer(t, env, nil, true)
	cookie := loginAs(t, handler, "owneruser", "correct horse battery staple")

	req := httptest.NewRequest(http.MethodPost, "/api/updates/apply", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want %d body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}

func TestUpdateApplySuccessReturns202AndPreservesVersion(t *testing.T) {
	env := setupApplyEnv(t)
	if env == nil {
		return
	}

	var receivedVersion string
	mockClient := &mockUpdaterClient{applyFunc: func(ctx context.Context, targetVersion string) (int, error) {
		receivedVersion = targetVersion
		return http.StatusAccepted, nil
	}}

	handler := buildApplyServer(t, env, mockClient, true)
	cookie := loginAs(t, handler, "owneruser", "correct horse battery staple")

	req, rec := applyRequest("v0.3.0")
	req.AddCookie(cookie)
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if receivedVersion != "v0.3.0" {
		t.Fatalf("receivedVersion=%q, want %q", receivedVersion, "v0.3.0")
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["ok"] != true {
		t.Fatalf("ok=%v", resp["ok"])
	}
	if resp["targetVersion"] != "v0.3.0" {
		t.Fatalf("targetVersion=%v", resp["targetVersion"])
	}
}

func TestUpdateApplyUpdaterConflictReturns409(t *testing.T) {
	env := setupApplyEnv(t)
	if env == nil {
		return
	}

	mockClient := &mockUpdaterClient{applyFunc: func(ctx context.Context, s string) (int, error) {
		return http.StatusConflict, nil
	}}

	handler := buildApplyServer(t, env, mockClient, true)
	cookie := loginAs(t, handler, "owneruser", "correct horse battery staple")

	req, rec := applyRequest("v0.3.0")
	req.AddCookie(cookie)
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d, want %d body=%s", rec.Code, http.StatusConflict, rec.Body.String())
	}
}

func TestUpdateApplyUpdaterErrorReturns502(t *testing.T) {
	env := setupApplyEnv(t)
	if env == nil {
		return
	}

	mockClient := &mockUpdaterClient{applyFunc: func(ctx context.Context, s string) (int, error) {
		return http.StatusInternalServerError, nil
	}}

	handler := buildApplyServer(t, env, mockClient, true)
	cookie := loginAs(t, handler, "owneruser", "correct horse battery staple")

	req, rec := applyRequest("v0.3.0")
	req.AddCookie(cookie)
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d, want %d body=%s", rec.Code, http.StatusBadGateway, rec.Body.String())
	}
}

func TestUpdateApplyUpdaterUnavailableReturns502(t *testing.T) {
	env := setupApplyEnv(t)
	if env == nil {
		return
	}

	mockClient := &mockUpdaterClient{applyFunc: func(ctx context.Context, s string) (int, error) {
		return 0, errors.New("connection refused")
	}}

	handler := buildApplyServer(t, env, mockClient, true)
	cookie := loginAs(t, handler, "owneruser", "correct horse battery staple")

	req, rec := applyRequest("v0.3.0")
	req.AddCookie(cookie)
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d, want %d body=%s", rec.Code, http.StatusBadGateway, rec.Body.String())
	}
}

func TestUpdateApplyRealHTTPClientForwards202(t *testing.T) {
	// Integration-style test using httptest.Server as the fake updater.
	updater := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method=%s, want POST", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type=%q, want application/json", ct)
		}
		var body struct {
			TargetVersion string `json:"targetVersion"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		if body.TargetVersion != "v0.4.0" {
			t.Errorf("targetVersion=%q, want v0.4.0", body.TargetVersion)
		}
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	t.Cleanup(updater.Close)

	env := setupApplyEnv(t)
	if env == nil {
		return
	}

	realClient := &httpserver.UpdaterHTTPClient{
		URL:    updater.URL,
		Client: updater.Client(),
	}

	handler := buildApplyServer(t, env, realClient, true)
	cookie := loginAs(t, handler, "owneruser", "correct horse battery staple")

	req, rec := applyRequest("v0.4.0")
	req.AddCookie(cookie)
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["targetVersion"] != "v0.4.0" {
		t.Fatalf("targetVersion=%v", resp["targetVersion"])
	}
}
