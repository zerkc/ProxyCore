package update

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCheckerReportsReleaseState(t *testing.T) {
	tests := []struct {
		name            string
		currentVersion  string
		tag             string
		status          string
		updateAvailable bool
	}{
		{
			name:            "same release",
			currentVersion:  "0.1.0",
			tag:             "v0.1.0",
			status:          StatusCurrent,
			updateAvailable: false,
		},
		{
			name:            "minor update",
			currentVersion:  "0.1.0",
			tag:             "v0.2.0",
			status:          StatusUpdateAvailable,
			updateAvailable: true,
		},
		{
			name:            "numeric minor ordering",
			currentVersion:  "0.9.0",
			tag:             "v0.10.0",
			status:          StatusUpdateAvailable,
			updateAvailable: true,
		},
		{
			name:            "current prerelease is older than stable",
			currentVersion:  "1.0.0-rc.1",
			tag:             "v1.0.0",
			status:          StatusUpdateAvailable,
			updateAvailable: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var requestCount atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requestCount.Add(1)
				if r.Method != http.MethodGet {
					t.Errorf("method=%s, want GET", r.Method)
				}
				if r.URL.Path != "/repos/zerkc/ProxyCore/releases/latest" {
					t.Errorf("path=%s", r.URL.Path)
				}
				if got := r.Header.Get("Accept"); got != "application/vnd.github+json" {
					t.Errorf("Accept=%q", got)
				}
				if got := r.Header.Get("X-GitHub-Api-Version"); got != "2022-11-28" {
					t.Errorf("X-GitHub-Api-Version=%q", got)
				}
				if got := r.Header.Get("User-Agent"); got != "ProxyCore/"+test.currentVersion {
					t.Errorf("User-Agent=%q", got)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"tag_name":     test.tag,
					"html_url":     "https://github.com/zerkc/ProxyCore/releases/tag/" + test.tag,
					"published_at": "2026-08-24T12:00:00Z",
				})
			}))
			t.Cleanup(server.Close)

			checker := NewChecker(CheckerOptions{
				CurrentVersion: test.currentVersion,
				Enabled:        true,
				BaseURL:        server.URL,
				Client:         server.Client(),
			})
			result := checker.Check(context.Background())

			if result.Status != test.status {
				t.Fatalf("status=%q, want %q", result.Status, test.status)
			}
			if result.CurrentVersion != test.currentVersion {
				t.Fatalf("currentVersion=%q", result.CurrentVersion)
			}
			if result.UpdateAvailable != test.updateAvailable {
				t.Fatalf("updateAvailable=%t, want %t", result.UpdateAvailable, test.updateAvailable)
			}
			if result.Latest == nil {
				t.Fatal("latest release is nil")
			}
			if result.Latest.Version != normalizeVersion(test.tag) {
				t.Fatalf("latest version=%q", result.Latest.Version)
			}
			if result.Latest.Tag != test.tag {
				t.Fatalf("latest tag=%q", result.Latest.Tag)
			}
			if result.Latest.PublishedAt == nil {
				t.Fatal("publishedAt is nil")
			}
			if result.CheckedAt == nil {
				t.Fatal("checkedAt is nil")
			}
			if got := requestCount.Load(); got != 1 {
				t.Fatalf("request count=%d, want 1", got)
			}
		})
	}
}

func TestCheckerRejectsUnavailableOrUnstableReleases(t *testing.T) {
	tests := []struct {
		name           string
		responseStatus int
		body           string
		current        string
	}{
		{
			name:           "not found",
			responseStatus: http.StatusNotFound,
			body:           `{"message":"Not Found"}`,
			current:        "0.1.0",
		},
		{
			name:           "forbidden",
			responseStatus: http.StatusForbidden,
			body:           `{"message":"Forbidden"}`,
			current:        "0.1.0",
		},
		{
			name:           "rate limited",
			responseStatus: http.StatusTooManyRequests,
			body:           `{"message":"API rate limit exceeded"}`,
			current:        "0.1.0",
		},
		{
			name:           "server error",
			responseStatus: http.StatusBadGateway,
			body:           `{"message":"Bad Gateway"}`,
			current:        "0.1.0",
		},
		{
			name:           "invalid tag",
			responseStatus: http.StatusOK,
			body:           `{"tag_name":"latest","html_url":"https://example.test/release"}`,
			current:        "0.1.0",
		},
		{
			name:           "prerelease tag",
			responseStatus: http.StatusOK,
			body:           `{"tag_name":"v0.2.0-rc.1","html_url":"https://example.test/release"}`,
			current:        "0.1.0",
		},
		{
			name:           "invalid current version",
			responseStatus: http.StatusOK,
			body:           `{"tag_name":"v0.2.0","html_url":"https://example.test/release"}`,
			current:        "development",
		},
		{
			name:           "invalid JSON",
			responseStatus: http.StatusOK,
			body:           `{`,
			current:        "0.1.0",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.responseStatus)
				_, _ = w.Write([]byte(test.body))
			}))
			t.Cleanup(server.Close)

			checker := NewChecker(CheckerOptions{
				CurrentVersion: test.current,
				Enabled:        true,
				BaseURL:        server.URL,
				Client:         server.Client(),
			})
			result := checker.Check(context.Background())

			if result.Status != StatusUnavailable {
				t.Fatalf("status=%q, want %q", result.Status, StatusUnavailable)
			}
			if result.UpdateAvailable {
				t.Fatal("unavailable result must not report an update")
			}
			if result.Latest != nil {
				t.Fatalf("latest=%+v, want nil", result.Latest)
			}
		})
	}
}

func TestCheckerCachesResultsAndServesStaleData(t *testing.T) {
	currentTime := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	var requestCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		switch requestCount.Add(1) {
		case 1:
			_ = json.NewEncoder(w).Encode(map[string]string{
				"tag_name": "v0.2.0",
				"html_url": "https://github.com/zerkc/ProxyCore/releases/tag/v0.2.0",
			})
		case 2:
			http.Error(w, "temporarily unavailable", http.StatusBadGateway)
		default:
			_ = json.NewEncoder(w).Encode(map[string]string{
				"tag_name": "v0.3.0",
				"html_url": "https://github.com/zerkc/ProxyCore/releases/tag/v0.3.0",
			})
		}
	}))
	t.Cleanup(server.Close)

	checker := NewChecker(CheckerOptions{
		CurrentVersion: "0.1.0",
		Enabled:        true,
		TTL:            time.Hour,
		BaseURL:        server.URL,
		Client:         server.Client(),
		Now:            func() time.Time { return currentTime },
	})

	first := checker.Check(context.Background())
	if first.Status != StatusUpdateAvailable || first.Latest == nil || first.Latest.Version != "0.2.0" {
		t.Fatalf("first result=%+v", first)
	}
	currentTime = currentTime.Add(30 * time.Minute)
	cached := checker.Check(context.Background())
	if cached.Status != StatusUpdateAvailable || cached.Latest == nil || cached.Latest.Version != "0.2.0" {
		t.Fatalf("cached result=%+v", cached)
	}
	if got := requestCount.Load(); got != 1 {
		t.Fatalf("request count after cache=%d, want 1", got)
	}

	currentTime = currentTime.Add(31 * time.Minute)
	stale := checker.Check(context.Background())
	if stale.Status != StatusStale || stale.Latest == nil || stale.Latest.Version != "0.2.0" {
		t.Fatalf("stale result=%+v", stale)
	}
	if stale.CheckedAt == nil || !stale.CheckedAt.Equal(*first.CheckedAt) {
		t.Fatalf("stale checkedAt=%v, want %v", stale.CheckedAt, first.CheckedAt)
	}
	if got := requestCount.Load(); got != 2 {
		t.Fatalf("request count after stale=%d, want 2", got)
	}

	currentTime = currentTime.Add(4 * time.Minute)
	staleAgain := checker.Check(context.Background())
	if staleAgain.Status != StatusStale {
		t.Fatalf("staleAgain status=%q", staleAgain.Status)
	}
	if got := requestCount.Load(); got != 2 {
		t.Fatalf("request count during failure cooldown=%d, want 2", got)
	}

	currentTime = currentTime.Add(2 * time.Minute)
	refreshed := checker.Check(context.Background())
	if refreshed.Status != StatusUpdateAvailable || refreshed.Latest == nil || refreshed.Latest.Version != "0.3.0" {
		t.Fatalf("refreshed result=%+v", refreshed)
	}
	if got := requestCount.Load(); got != 3 {
		t.Fatalf("request count after refresh=%d, want 3", got)
	}
}

func TestCheckerDeduplicatesConcurrentRequests(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var requestCount atomic.Int32
	var once sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requestCount.Add(1)
		once.Do(func() { close(started) })
		<-release
		_ = json.NewEncoder(w).Encode(map[string]string{
			"tag_name": "v0.2.0",
			"html_url": "https://github.com/zerkc/ProxyCore/releases/tag/v0.2.0",
		})
	}))
	t.Cleanup(server.Close)

	checker := NewChecker(CheckerOptions{
		CurrentVersion: "0.1.0",
		Enabled:        true,
		BaseURL:        server.URL,
		Client:         server.Client(),
	})
	results := make(chan Result, 2)
	go func() { results <- checker.Check(context.Background()) }()
	<-started
	go func() { results <- checker.Check(context.Background()) }()
	close(release)

	for range 2 {
		result := <-results
		if result.Status != StatusUpdateAvailable {
			t.Fatalf("status=%q", result.Status)
		}
	}
	if got := requestCount.Load(); got != 1 {
		t.Fatalf("request count=%d, want 1", got)
	}
}

func TestCheckerDisabledDoesNotCallGitHub(t *testing.T) {
	var requestCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requestCount.Add(1)
	}))
	t.Cleanup(server.Close)

	checker := NewChecker(CheckerOptions{
		CurrentVersion: "0.1.0",
		BaseURL:        server.URL,
		Client:         server.Client(),
	})
	result := checker.Check(context.Background())

	if result.Status != StatusDisabled {
		t.Fatalf("status=%q, want %q", result.Status, StatusDisabled)
	}
	if result.CurrentVersion != "0.1.0" {
		t.Fatalf("currentVersion=%q", result.CurrentVersion)
	}
	if got := requestCount.Load(); got != 0 {
		t.Fatalf("request count=%d, want 0", got)
	}
}

func TestCheckerTimeoutReturnsUnavailable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(100 * time.Millisecond)
		_, _ = w.Write([]byte(`{"tag_name":"v0.2.0","html_url":"https://example.test/release"}`))
	}))
	t.Cleanup(server.Close)

	checker := NewChecker(CheckerOptions{
		CurrentVersion: "0.1.0",
		Enabled:        true,
		Timeout:        10 * time.Millisecond,
		BaseURL:        server.URL,
	})
	result := checker.Check(context.Background())

	if result.Status != StatusUnavailable {
		t.Fatalf("status=%q, want %q", result.Status, StatusUnavailable)
	}
}

func TestCompareSemVer(t *testing.T) {
	tests := []struct {
		name  string
		left  string
		right string
		want  int
	}{
		{name: "numeric minor", left: "0.10.0", right: "0.9.0", want: 1},
		{name: "release after prerelease", left: "1.0.0", right: "1.0.0-rc.1", want: 1},
		{name: "prerelease numeric order", left: "1.0.0-rc.2", right: "1.0.0-rc.10", want: -1},
		{name: "prerelease length", left: "1.0.0-rc.1", right: "1.0.0-rc.1.1", want: -1},
		{name: "build metadata ignored", left: "1.0.0+linux", right: "1.0.0+arm64", want: 0},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			left, err := parseSemVer(test.left)
			if err != nil {
				t.Fatalf("parse left: %v", err)
			}
			right, err := parseSemVer(test.right)
			if err != nil {
				t.Fatalf("parse right: %v", err)
			}
			if got := compareSemVer(left, right); got != test.want {
				t.Fatalf("compare=%d, want %d", got, test.want)
			}
		})
	}
}
