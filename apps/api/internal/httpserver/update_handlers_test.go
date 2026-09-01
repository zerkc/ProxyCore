package httpserver_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zerkc/ProxyCore/apps/api/internal/config"
	"github.com/zerkc/ProxyCore/apps/api/internal/httpserver"
	"github.com/zerkc/ProxyCore/apps/api/internal/update"
	"github.com/zerkc/ProxyCore/apps/api/internal/version"
)

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
