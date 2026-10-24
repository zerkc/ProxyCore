package httpserver_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/auth"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/httpserver"
	"github.com/zerkc/ProxyCore/apps/api/internal/version"
)

// statusUpdateBody decodes only the update-related fields of GET /api/status.
type statusUpdateBody struct {
	Version      string `json:"version"`
	Updating     *bool  `json:"updating"`
	UpdateTarget string `json:"updateTarget"`
}

func getStatusUpdate(t *testing.T, env *sqliteHTTPEnv, cookie *http.Cookie) (statusUpdateBody, map[string]json.RawMessage) {
	t.Helper()
	response := doJSON(t, env.Handler, http.MethodGet, "/api/status", nil, cookie)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var body statusUpdateBody
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	return body, raw
}

func TestStatusReportsVersionAndUpdaterState(t *testing.T) {
	tests := []struct {
		name         string
		status       httpserver.UpdaterStatus
		err          error
		wantUpdating *bool
		wantTarget   string
	}{
		{
			name:         "running",
			status:       httpserver.UpdaterStatus{Status: "running", TargetVersion: " v0.2.0 "},
			wantUpdating: boolPtr(true),
			wantTarget:   "0.2.0",
		},
		{
			name:         "idle",
			status:       httpserver.UpdaterStatus{Status: "ok", TargetVersion: "v0.2.0"},
			wantUpdating: boolPtr(false),
		},
		{
			name:         "running without target",
			status:       httpserver.UpdaterStatus{Status: "running"},
			wantUpdating: boolPtr(true),
		},
		{
			name: "updater error is unknown",
			err:  errors.New("connection refused"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			updater := &mockUpdaterWithStatusClient{
				statusFunc: func(context.Context) (httpserver.UpdaterStatus, error) { return tt.status, tt.err },
			}
			env := newSQLiteHTTPTestServer(t, httpserver.WithUpdaterClient(updater))
			cookie := sqliteOwnerCookie(t, env.Handler)

			body, raw := getStatusUpdate(t, env, cookie)
			if body.Version != version.Version {
				t.Fatalf("version=%q, want %q", body.Version, version.Version)
			}
			if string(raw["updating"]) == "" {
				t.Fatal("updating key is missing; it must be present (null when unknown)")
			}
			if (body.Updating == nil) != (tt.wantUpdating == nil) || (body.Updating != nil && *body.Updating != *tt.wantUpdating) {
				t.Fatalf("updating=%v, want %v", body.Updating, tt.wantUpdating)
			}
			if body.UpdateTarget != tt.wantTarget {
				t.Fatalf("updateTarget=%q, want %q", body.UpdateTarget, tt.wantTarget)
			}
			if tt.wantTarget == "" {
				if _, present := raw["updateTarget"]; present {
					t.Fatalf("updateTarget must be omitted, got %s", raw["updateTarget"])
				}
			}
		})
	}
}

func TestStatusUpdatingIsNullWithoutUpdater(t *testing.T) {
	env := newSQLiteHTTPTestServer(t)
	cookie := sqliteOwnerCookie(t, env.Handler)

	body, raw := getStatusUpdate(t, env, cookie)
	if body.Version != version.Version {
		t.Fatalf("version=%q, want %q", body.Version, version.Version)
	}
	if string(raw["updating"]) != "null" {
		t.Fatalf("updating=%s, want null", raw["updating"])
	}
}

func TestStatusUpdaterStateIsReadableByNodeAndNonOwner(t *testing.T) {
	updater := &mockUpdaterWithStatusClient{
		statusFunc: func(context.Context) (httpserver.UpdaterStatus, error) {
			return httpserver.UpdaterStatus{Status: "running", TargetVersion: "v0.2.0"}, nil
		},
	}

	t.Run("node", func(t *testing.T) {
		env := newSQLiteHTTPTestServer(t, httpserver.WithUpdaterClient(updater))
		cookie := sqliteOwnerCookie(t, env.Handler)
		moveIdentityToRole(t, env, domain.TopologyRoleNode)

		body, _ := getStatusUpdate(t, env, cookie)
		if body.Updating == nil || !*body.Updating || body.UpdateTarget != "0.2.0" {
			t.Fatalf("updating=%v target=%q", body.Updating, body.UpdateTarget)
		}
	})

	t.Run("operator", func(t *testing.T) {
		env := newSQLiteHTTPTestServer(t, httpserver.WithUpdaterClient(updater))
		_ = sqliteOwnerCookie(t, env.Handler)
		if _, err := env.Auth.CreateUser(context.Background(), auth.User{}, "operatoruser", "correct horse battery staple", auth.RoleOperator); err != nil {
			t.Fatalf("create operator: %v", err)
		}
		cookie := sqliteLoginCookie(t, env.Handler, "operatoruser", "correct horse battery staple")

		body, _ := getStatusUpdate(t, env, cookie)
		if body.Updating == nil || !*body.Updating || body.UpdateTarget != "0.2.0" {
			t.Fatalf("updating=%v target=%q", body.Updating, body.UpdateTarget)
		}
	})
}

func TestStatusDoesNotBlockOnSlowUpdater(t *testing.T) {
	updater := &mockUpdaterWithStatusClient{
		statusFunc: func(ctx context.Context) (httpserver.UpdaterStatus, error) {
			<-ctx.Done()
			return httpserver.UpdaterStatus{}, ctx.Err()
		},
	}
	env := newSQLiteHTTPTestServer(t, httpserver.WithUpdaterClient(updater))
	cookie := sqliteOwnerCookie(t, env.Handler)

	started := time.Now()
	body, raw := getStatusUpdate(t, env, cookie)
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("status took %s, want a short updater timeout", elapsed)
	}
	if body.Updating != nil || string(raw["updating"]) != "null" {
		t.Fatalf("updating=%s, want null", raw["updating"])
	}
}

func boolPtr(v bool) *bool { return &v }

// updateProgressBody decodes the optional updateProgress object of GET /api/status.
type updateProgressBody struct {
	Step      string `json:"step"`
	Completed int    `json:"completed"`
	Total     int    `json:"total"`
}

func TestStatusReportsUpdateProgressOnlyWhileUpdating(t *testing.T) {
	tests := []struct {
		name         string
		status       httpserver.UpdaterStatus
		err          error
		wantProgress *updateProgressBody
	}{
		{
			name: "running with progress",
			status: httpserver.UpdaterStatus{
				Status: "running", TargetVersion: "v0.2.0",
				CurrentStep: "restart", CompletedSteps: 2, TotalSteps: 5,
			},
			wantProgress: &updateProgressBody{Step: "restart", Completed: 2, Total: 5},
		},
		{
			name:         "running at the first step has zero completed",
			status:       httpserver.UpdaterStatus{Status: "running", CurrentStep: "pull", TotalSteps: 5},
			wantProgress: &updateProgressBody{Step: "pull", Completed: 0, Total: 5},
		},
		{
			name:   "running from an old updater without progress fields",
			status: httpserver.UpdaterStatus{Status: "running", TargetVersion: "v0.2.0"},
		},
		{
			name:   "finished update does not report progress",
			status: httpserver.UpdaterStatus{Status: "ok", CurrentStep: "bootstrap", CompletedSteps: 5, TotalSteps: 5},
		},
		{
			name: "unknown updater state does not report progress",
			err:  errors.New("connection refused"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			updater := &mockUpdaterWithStatusClient{
				statusFunc: func(context.Context) (httpserver.UpdaterStatus, error) { return tt.status, tt.err },
			}
			env := newSQLiteHTTPTestServer(t, httpserver.WithUpdaterClient(updater))
			cookie := sqliteOwnerCookie(t, env.Handler)

			_, raw := getStatusUpdate(t, env, cookie)
			encoded, present := raw["updateProgress"]
			if tt.wantProgress == nil {
				if present {
					t.Fatalf("updateProgress must be absent, got %s", encoded)
				}
				return
			}
			if !present {
				t.Fatal("updateProgress is missing")
			}
			var got updateProgressBody
			if err := json.Unmarshal(encoded, &got); err != nil {
				t.Fatal(err)
			}
			if got != *tt.wantProgress {
				t.Fatalf("updateProgress=%+v, want %+v", got, *tt.wantProgress)
			}
		})
	}
}

func TestStatusUpdateProgressDecodesOldAndNewUpdaterPayloads(t *testing.T) {
	payloads := map[string]string{
		"old updater": `{"status":"running","targetVersion":"v0.2.0","steps":[]}`,
		"new updater": `{"status":"running","targetVersion":"v0.2.0","steps":[],"currentStep":"verify","completedSteps":3,"totalSteps":5}`,
	}
	for name, payload := range payloads {
		t.Run(name, func(t *testing.T) {
			updaterServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(payload))
			}))
			t.Cleanup(updaterServer.Close)
			client := &httpserver.UpdaterHTTPClient{URL: updaterServer.URL + "/internal/apply", Client: updaterServer.Client()}
			env := newSQLiteHTTPTestServer(t, httpserver.WithUpdaterClient(client))
			cookie := sqliteOwnerCookie(t, env.Handler)

			body, raw := getStatusUpdate(t, env, cookie)
			if body.Updating == nil || !*body.Updating {
				t.Fatalf("updating=%v, want true", body.Updating)
			}
			_, present := raw["updateProgress"]
			if want := name == "new updater"; present != want {
				t.Fatalf("updateProgress present=%v, want %v (%s)", present, want, raw["updateProgress"])
			}
		})
	}
}

func TestStatusUpdateProgressIsReadableByNodeAndNonOwner(t *testing.T) {
	updater := &mockUpdaterWithStatusClient{
		statusFunc: func(context.Context) (httpserver.UpdaterStatus, error) {
			return httpserver.UpdaterStatus{Status: "running", TargetVersion: "v0.2.0", CurrentStep: "pull", TotalSteps: 5}, nil
		},
	}

	t.Run("node", func(t *testing.T) {
		env := newSQLiteHTTPTestServer(t, httpserver.WithUpdaterClient(updater))
		cookie := sqliteOwnerCookie(t, env.Handler)
		moveIdentityToRole(t, env, domain.TopologyRoleNode)

		if _, raw := getStatusUpdate(t, env, cookie); len(raw["updateProgress"]) == 0 {
			t.Fatal("updateProgress is missing for a node")
		}
	})

	t.Run("operator", func(t *testing.T) {
		env := newSQLiteHTTPTestServer(t, httpserver.WithUpdaterClient(updater))
		_ = sqliteOwnerCookie(t, env.Handler)
		if _, err := env.Auth.CreateUser(context.Background(), auth.User{}, "operatoruser", "correct horse battery staple", auth.RoleOperator); err != nil {
			t.Fatalf("create operator: %v", err)
		}
		cookie := sqliteLoginCookie(t, env.Handler, "operatoruser", "correct horse battery staple")

		if _, raw := getStatusUpdate(t, env, cookie); len(raw["updateProgress"]) == 0 {
			t.Fatal("updateProgress is missing for a non-owner")
		}
	})
}
