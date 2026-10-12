package httpserver_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

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
