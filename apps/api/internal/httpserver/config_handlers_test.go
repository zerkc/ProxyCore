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

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/zerkc/ProxyCore/apps/api/internal/auth"
	"github.com/zerkc/ProxyCore/apps/api/internal/config"
	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/httpserver"
	"github.com/zerkc/ProxyCore/apps/api/internal/identity"
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
	var statusBody struct {
		Identity map[string]any `json:"identity"`
	}
	if err := json.Unmarshal(status.Body.Bytes(), &statusBody); err != nil {
		t.Fatalf("decode status identity: %v", err)
	}
	for _, field := range []string{
		"installationId",
		"nodeId",
		"role",
		"leadershipGeneration",
		"latestKnownGeneration",
		"stalePrimary",
		"writable",
	} {
		if _, ok := statusBody.Identity[field]; !ok {
			t.Fatalf("status identity missing %q: %v", field, statusBody.Identity)
		}
	}
	if statusBody.Identity["role"] != string(domain.TopologyRoleStandalone) || statusBody.Identity["writable"] != true {
		t.Fatalf("unexpected standalone identity: %v", statusBody.Identity)
	}
	if _, ok := statusBody.Identity["clusterKeyId"]; ok {
		t.Fatalf("status identity leaked cluster key id: %v", statusBody.Identity)
	}
}

func TestStandaloneConfigurationMutationRemainsWritable(t *testing.T) {
	srv := newConfigTestServer(t)
	cookie := loginOwner(t, srv)

	response := doJSON(t, srv.Handler(), http.MethodPut, "/api/settings", map[string]any{
		"ingress": map[string]any{"ipv4": "192.168.1.10"},
	}, cookie)
	if response.Code != http.StatusOK {
		t.Fatalf("standalone settings status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestConfigurationMutationsBlockedForNonWritableIdentity(t *testing.T) {
	for _, role := range []domain.TopologyRole{
		domain.TopologyRoleNode,
		domain.TopologyRoleStalePrimary,
	} {
		t.Run(string(role), func(t *testing.T) {
			srv := newConfigTestServerForRole(t, role)
			cookie := loginOwner(t, srv)

			response := doJSON(t, srv.Handler(), http.MethodPut, "/api/settings", map[string]any{
				"ingress": map[string]any{"ipv4": "192.168.1.11"},
			}, cookie)
			if response.Code != http.StatusForbidden {
				t.Fatalf("%s settings status=%d body=%s", role, response.Code, response.Body.String())
			}
			var body map[string]any
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode %s rejection: %v", role, err)
			}
			if body["code"] != "TOPOLOGY_READ_ONLY" {
				t.Fatalf("%s rejection=%v", role, body)
			}

			enrollment := doJSON(t, srv.Handler(), http.MethodPut, "/api/settings/enrollment-hostnames", map[string]any{
				"hostnames": []string{"enroll.example.com"},
			}, cookie)
			if enrollment.Code != http.StatusForbidden {
				t.Fatalf("%s enrollment hostnames status=%d body=%s", role, enrollment.Code, enrollment.Body.String())
			}
			var enrollmentBody map[string]any
			if err := json.Unmarshal(enrollment.Body.Bytes(), &enrollmentBody); err != nil {
				t.Fatalf("decode %s enrollment rejection: %v", role, err)
			}
			if enrollmentBody["code"] != "TOPOLOGY_READ_ONLY" {
				t.Fatalf("%s enrollment rejection=%v", role, enrollmentBody)
			}
		})
	}
}

func TestEnrollmentHostnamesOwnerAPI(t *testing.T) {
	srv := newConfigTestServer(t)
	cookie := loginOwner(t, srv)

	get := doJSON(t, srv.Handler(), http.MethodGet, "/api/settings/enrollment-hostnames", nil, cookie)
	if get.Code != http.StatusOK {
		t.Fatalf("initial enrollment hostnames status=%d body=%s", get.Code, get.Body.String())
	}
	var initial struct {
		Configured bool     `json:"configured"`
		Hostnames  []string `json:"hostnames"`
	}
	if err := json.Unmarshal(get.Body.Bytes(), &initial); err != nil {
		t.Fatalf("decode initial enrollment hostnames: %v", err)
	}
	if initial.Configured || len(initial.Hostnames) != 0 {
		t.Fatalf("initial enrollment hostnames=%+v", initial)
	}

	invalid := doJSON(t, srv.Handler(), http.MethodPut, "/api/settings/enrollment-hostnames", map[string]any{
		"hostnames": []string{"https://primary.example.com"},
	}, cookie)
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("invalid enrollment hostnames status=%d body=%s", invalid.Code, invalid.Body.String())
	}

	put := doJSON(t, srv.Handler(), http.MethodPut, "/api/settings/enrollment-hostnames", map[string]any{
		"hostnames": []string{
			"Example.COM.",
			"2001:0DB8:0:0:0:0:0:1",
			"192.0.2.1",
			"example.com",
		},
	}, cookie)
	if put.Code != http.StatusOK {
		t.Fatalf("put enrollment hostnames status=%d body=%s", put.Code, put.Body.String())
	}
	var saved struct {
		Configured bool     `json:"configured"`
		Hostnames  []string `json:"hostnames"`
	}
	if err := json.Unmarshal(put.Body.Bytes(), &saved); err != nil {
		t.Fatalf("decode saved enrollment hostnames: %v", err)
	}
	want := []string{"192.0.2.1", "2001:db8::1", "example.com"}
	if !saved.Configured || strings.Join(saved.Hostnames, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("saved enrollment hostnames=%+v, want configured %v", saved, want)
	}

	clear := doJSON(t, srv.Handler(), http.MethodPut, "/api/settings/enrollment-hostnames", map[string]any{
		"hostnames": []string{},
	}, cookie)
	if clear.Code != http.StatusOK {
		t.Fatalf("clear enrollment hostnames status=%d body=%s", clear.Code, clear.Body.String())
	}
	var cleared struct {
		Configured bool     `json:"configured"`
		Hostnames  []string `json:"hostnames"`
	}
	if err := json.Unmarshal(clear.Body.Bytes(), &cleared); err != nil {
		t.Fatalf("decode cleared enrollment hostnames: %v", err)
	}
	if cleared.Configured || len(cleared.Hostnames) != 0 {
		t.Fatalf("cleared enrollment hostnames=%+v", cleared)
	}
}

func TestEnrollmentHostnamesOwnerOnly(t *testing.T) {
	srv := newConfigTestServer(t)
	ownerCookie := loginOwner(t, srv)
	createOperator := doJSON(t, srv.Handler(), http.MethodPost, "/api/users", map[string]any{
		"username": "operator",
		"password": "correct horse battery staple",
		"role":     "operator",
	}, ownerCookie)
	if createOperator.Code != http.StatusCreated {
		t.Fatalf("create operator status=%d body=%s", createOperator.Code, createOperator.Body.String())
	}
	operatorCookie := loginUser(t, srv, "operator", "correct horse battery staple")

	for _, method := range []string{http.MethodGet, http.MethodPut} {
		body := any(nil)
		if method == http.MethodPut {
			body = map[string]any{"hostnames": []string{"enroll.example.com"}}
		}
		response := doJSON(t, srv.Handler(), method, "/api/settings/enrollment-hostnames", body, operatorCookie)
		if response.Code != http.StatusForbidden {
			t.Fatalf("operator %s status=%d body=%s", method, response.Code, response.Body.String())
		}
	}
}

func TestEnrollmentHostnamesReadsRemainAvailableForReadOnlyRoles(t *testing.T) {
	cases := []struct {
		role       domain.TopologyRole
		hostnames  []string
		configured bool
	}{
		{role: domain.TopologyRoleNode, hostnames: []string{"node.enroll.example"}, configured: true},
		{role: domain.TopologyRoleStalePrimary, configured: false},
	}
	for _, tc := range cases {
		t.Run(string(tc.role), func(t *testing.T) {
			srv := newConfigTestServerForRoleWithEnrollment(t, tc.role, tc.hostnames)
			cookie := loginOwner(t, srv)
			response := doJSON(t, srv.Handler(), http.MethodGet, "/api/settings/enrollment-hostnames", nil, cookie)
			if response.Code != http.StatusOK {
				t.Fatalf("%s enrollment GET status=%d body=%s", tc.role, response.Code, response.Body.String())
			}
			var body struct {
				Configured bool     `json:"configured"`
				Hostnames  []string `json:"hostnames"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode %s enrollment GET: %v", tc.role, err)
			}
			if body.Configured != tc.configured {
				t.Fatalf("%s configured=%v, want %v", tc.role, body.Configured, tc.configured)
			}
			if tc.configured && strings.Join(body.Hostnames, "\x00") != strings.Join(tc.hostnames, "\x00") {
				t.Fatalf("%s hostnames=%v, want %v", tc.role, body.Hostnames, tc.hostnames)
			}
		})
	}
}

func TestEnrollmentHostnamesRequireAuthentication(t *testing.T) {
	srv := newConfigTestServer(t)
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		body := any(nil)
		if method == http.MethodPut {
			body = map[string]any{"hostnames": []string{"enroll.example.com"}}
		}
		response := doJSON(t, srv.Handler(), method, "/api/settings/enrollment-hostnames", body, nil)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("unauthenticated %s status=%d body=%s", method, response.Code, response.Body.String())
		}
	}
}

func loginOwner(t *testing.T, srv *httpserver.Server) *http.Cookie {
	t.Helper()
	bootstrap := doJSON(t, srv.Handler(), http.MethodPost, "/api/auth/bootstrap", map[string]any{
		"username": "owner",
		"password": "correct horse battery staple",
	}, nil)
	if bootstrap.Code != http.StatusCreated {
		t.Fatalf("bootstrap status=%d body=%s", bootstrap.Code, bootstrap.Body.String())
	}
	return loginUser(t, srv, "owner", "correct horse battery staple")
}

func loginUser(t *testing.T, srv *httpserver.Server, username, password string) *http.Cookie {
	t.Helper()
	login := doJSON(t, srv.Handler(), http.MethodPost, "/api/auth/login", map[string]any{
		"username": username,
		"password": password,
	}, nil)
	if login.Code != http.StatusOK {
		t.Fatalf("login %s status=%d body=%s", username, login.Code, login.Body.String())
	}
	cookies := login.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("login cookies=%v", cookies)
	}
	return cookies[0]
}

func newConfigTestServer(t *testing.T) *httpserver.Server {
	return newConfigTestServerForRole(t, domain.TopologyRoleStandalone)
}

func newConfigTestServerForRole(t *testing.T, role domain.TopologyRole) *httpserver.Server {
	return newConfigTestServerForRoleWithEnrollment(t, role, nil)
}

func newConfigTestServerForRoleWithEnrollment(t *testing.T, role domain.TopologyRole, hostnames []string) *httpserver.Server {
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

	authSvc := auth.NewService(authStore, auth.ServiceOptions{SessionTTL: time.Hour})
	configStore := configuration.New(pool, "", domain.Ingress{})
	if hostnames != nil {
		if _, err := configStore.UpdateEnrollmentHostnames(ctx, hostnames); err != nil {
			t.Fatalf("seed enrollment hostnames: %v", err)
		}
	}
	identitySvc := identity.NewService(identity.NewPgStore(pool))
	if _, _, err := identitySvc.EnsureBootstrapped(ctx); err != nil {
		t.Fatalf("bootstrap identity: %v", err)
	}
	if role != domain.TopologyRoleStandalone {
		switch role {
		case domain.TopologyRoleNode:
			if _, err := identitySvc.TransitionTo(ctx, role); err != nil {
				t.Fatalf("transition identity to node: %v", err)
			}
		case domain.TopologyRoleStalePrimary:
			if _, err := identitySvc.RecordImportedSnapshot(ctx, uuid.New(), 2); err != nil {
				t.Fatalf("mark identity stale: %v", err)
			}
		default:
			t.Fatalf("unsupported test role %s", role)
		}
	}
	return httpserver.New(config.Config{
		UIDist:            t.TempDir(),
		SessionCookieName: "proxycore_session",
		SessionTTL:        time.Hour,
	}, log.New(io.Discard, "", 0),
		httpserver.WithAuthService(authSvc),
		httpserver.WithConfigurationStore(configStore),
		httpserver.WithIdentityService(identitySvc),
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
