package httpserver_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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

type mockUpdaterWithStatusClient struct {
	statusFunc func(ctx context.Context) (httpserver.UpdaterStatus, error)
}

func (m *mockUpdaterWithStatusClient) Apply(context.Context, string) (int, error) {
	return http.StatusAccepted, nil
}

func (m *mockUpdaterWithStatusClient) Status(ctx context.Context) (httpserver.UpdaterStatus, error) {
	return m.statusFunc(ctx)
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

func TestUpdatesWithoutCheckerStillReportsRunningUpdater(t *testing.T) {
	updater := &mockUpdaterWithStatusClient{
		statusFunc: func(context.Context) (httpserver.UpdaterStatus, error) {
			return httpserver.UpdaterStatus{
				Status:        "running",
				TargetVersion: "v0.2.0",
			}, nil
		},
	}
	server := httpserver.New(
		config.Config{UIDist: t.TempDir()},
		nil,
		httpserver.WithUpdaterClient(updater),
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
	if result.Status != update.StatusDisabled {
		t.Fatalf("status=%q, want %q", result.Status, update.StatusDisabled)
	}
	if !result.UpdateInProgress {
		t.Fatal("updateInProgress=false, want true")
	}
	if result.TargetVersion != "0.2.0" {
		t.Fatalf("targetVersion=%q, want 0.2.0", result.TargetVersion)
	}
}

func newUpdatesTestChecker(t *testing.T) *update.Checker {
	t.Helper()
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

	return update.NewChecker(update.CheckerOptions{
		CurrentVersion: "0.1.0",
		Enabled:        true,
		BaseURL:        github.URL,
		Client:         github.Client(),
	})
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

func TestUpdatesEndpointOverlaysRunningUpdaterStatus(t *testing.T) {
	updater := &mockUpdaterWithStatusClient{
		statusFunc: func(context.Context) (httpserver.UpdaterStatus, error) {
			return httpserver.UpdaterStatus{
				Status:        "running",
				TargetVersion: " v0.2.0 ",
			}, nil
		},
	}
	server := httpserver.New(
		config.Config{UIDist: t.TempDir()},
		nil,
		httpserver.WithUpdateChecker(newUpdatesTestChecker(t)),
		httpserver.WithUpdaterClient(updater),
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
	if !result.UpdateInProgress {
		t.Fatal("updateInProgress=false, want true")
	}
	if result.TargetVersion != "0.2.0" {
		t.Fatalf("targetVersion=%q, want %q", result.TargetVersion, "0.2.0")
	}
}

func TestUpdatesEndpointBestEffortUpdaterStatus(t *testing.T) {
	tests := []struct {
		name   string
		status httpserver.UpdaterStatus
		err    error
	}{
		{
			name: "unavailable",
			err:  errors.New("connection refused"),
		},
		{
			name: "not running",
			status: httpserver.UpdaterStatus{
				Status:        "ok",
				TargetVersion: "v0.2.0",
			},
		},
		{
			name:   "running without target",
			status: httpserver.UpdaterStatus{Status: "running"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			updater := &mockUpdaterWithStatusClient{
				statusFunc: func(context.Context) (httpserver.UpdaterStatus, error) {
					return tt.status, tt.err
				},
			}
			server := httpserver.New(
				config.Config{UIDist: t.TempDir()},
				nil,
				httpserver.WithUpdateChecker(newUpdatesTestChecker(t)),
				httpserver.WithUpdaterClient(updater),
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
			if result.UpdateInProgress {
				t.Fatal("updateInProgress=true, want false")
			}
			if result.TargetVersion != "" {
				t.Fatalf("targetVersion=%q, want empty", result.TargetVersion)
			}
		})
	}
}

func TestUpdaterHTTPClientStatusFallsBackFromApplyURL(t *testing.T) {
	updater := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method=%s, want GET", r.Method)
		}
		if r.URL.Path != "/internal/status" {
			t.Errorf("path=%s, want /internal/status", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(httpserver.UpdaterStatus{
			Status:        "running",
			TargetVersion: "v0.5.0",
		})
	}))
	t.Cleanup(updater.Close)

	client := &httpserver.UpdaterHTTPClient{
		URL:    updater.URL + "/internal/apply/",
		Client: updater.Client(),
	}
	status, err := client.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.Status != "running" {
		t.Fatalf("status=%q, want running", status.Status)
	}
	if status.TargetVersion != "v0.5.0" {
		t.Fatalf("targetVersion=%q, want v0.5.0", status.TargetVersion)
	}
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

// newApplyHTTPTestServer serves the SQLite-backed stack with one bootstrapped
// user: an owner when ownerRole is true, otherwise an operator. A nil updater
// leaves the apply route unconfigured.
func newApplyHTTPTestServer(t *testing.T, updater httpserver.UpdaterClient, ownerRole bool) http.Handler {
	t.Helper()
	var opts []httpserver.Option
	if updater != nil {
		opts = append(opts, httpserver.WithUpdaterClient(updater))
	}
	env := newSQLiteHTTPTestServer(t, opts...)
	username, role := "owneruser", auth.RoleOwner
	if !ownerRole {
		username, role = "operatoruser", auth.RoleOperator
	}
	if _, err := env.Auth.CreateUser(context.Background(), auth.User{}, username, "correct horse battery staple", role); err != nil {
		t.Fatalf("create %s user: %v", role, err)
	}
	return env.Handler
}

func TestUpdateApplyUnauthenticatedReturns401(t *testing.T) {
	handler := newApplyHTTPTestServer(t, nil, true)

	req, rec := applyRequest("v0.2.0") // no session cookie
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestUpdateApplyOperatorRoleReturns403(t *testing.T) {
	mockClient := &mockUpdaterClient{applyFunc: func(ctx context.Context, s string) (int, error) {
		return http.StatusAccepted, nil
	}}
	handler := newApplyHTTPTestServer(t, mockClient, false) // operator role

	cookie := loginAs(t, handler, "operatoruser", "correct horse battery staple")

	req, rec := applyRequest("v0.3.0")
	req.AddCookie(cookie)
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want %d body=%s", rec.Code, http.StatusForbidden, rec.Body.String())
	}
}

func TestUpdateApplyNoClientReturns503(t *testing.T) {
	handler := newApplyHTTPTestServer(t, nil, true) // nil updater, owner role
	cookie := loginAs(t, handler, "owneruser", "correct horse battery staple")

	req, rec := applyRequest("v0.2.0")
	req.AddCookie(cookie)
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want %d body=%s", rec.Code, http.StatusServiceUnavailable, rec.Body.String())
	}
}

func TestUpdateApplyInvalidJSONReturns400(t *testing.T) {
	handler := newApplyHTTPTestServer(t, nil, true)
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
	handler := newApplyHTTPTestServer(t, nil, true)
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
	var receivedVersion string
	mockClient := &mockUpdaterClient{applyFunc: func(ctx context.Context, targetVersion string) (int, error) {
		receivedVersion = targetVersion
		return http.StatusAccepted, nil
	}}

	handler := newApplyHTTPTestServer(t, mockClient, true)
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
	mockClient := &mockUpdaterClient{applyFunc: func(ctx context.Context, s string) (int, error) {
		return http.StatusConflict, nil
	}}

	handler := newApplyHTTPTestServer(t, mockClient, true)
	cookie := loginAs(t, handler, "owneruser", "correct horse battery staple")

	req, rec := applyRequest("v0.3.0")
	req.AddCookie(cookie)
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d, want %d body=%s", rec.Code, http.StatusConflict, rec.Body.String())
	}
}

func TestUpdateApplyUpdaterErrorReturns502(t *testing.T) {
	mockClient := &mockUpdaterClient{applyFunc: func(ctx context.Context, s string) (int, error) {
		return http.StatusInternalServerError, nil
	}}

	handler := newApplyHTTPTestServer(t, mockClient, true)
	cookie := loginAs(t, handler, "owneruser", "correct horse battery staple")

	req, rec := applyRequest("v0.3.0")
	req.AddCookie(cookie)
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d, want %d body=%s", rec.Code, http.StatusBadGateway, rec.Body.String())
	}
}

func TestUpdateApplyUpdaterUnavailableReturns502(t *testing.T) {
	mockClient := &mockUpdaterClient{applyFunc: func(ctx context.Context, s string) (int, error) {
		return 0, errors.New("connection refused")
	}}

	handler := newApplyHTTPTestServer(t, mockClient, true)
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

	realClient := &httpserver.UpdaterHTTPClient{
		URL:    updater.URL,
		Client: updater.Client(),
	}

	handler := newApplyHTTPTestServer(t, realClient, true)
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
