package httpserver_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/zerkc/ProxyCore/apps/api/internal/config"
	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/httpserver"
	"github.com/zerkc/ProxyCore/apps/api/internal/identity"
	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

const (
	enrollmentTrustPath       = httpserver.EnrollmentTrustPath
	enrollmentTrustCAPEM      = httpserver.EnrollmentTrustCAPEMPath
	enrollmentTrustAliasPath  = "/api/settings/enrollment-trust"
	enrollmentTrustAliasCAPEM = enrollmentTrustAliasPath + "/ca"
)

type enrollmentTrustResponse struct {
	Status           string    `json:"status"`
	Configured       bool      `json:"configured"`
	Ready            bool      `json:"ready"`
	CertificatePEM   string    `json:"certificatePem"`
	CACertificatePEM string    `json:"caCertificatePem"`
	CADERHashSHA256  string    `json:"caDerSha256"`
	ExpiresAt        time.Time `json:"expiresAt"`
}

func assertTrustState(t *testing.T, response *httptest.ResponseRecorder, status string, configured, ready bool) {
	t.Helper()
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Content-Type") != "application/json; charset=utf-8" {
		t.Fatalf("trust status/headers=%d %q %q body=%s", response.Code, response.Header().Get("Cache-Control"), response.Header().Get("Content-Type"), response.Body.String())
	}
	var body enrollmentTrustResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode trust state: %v", err)
	}
	if body.Status != status || body.Configured != configured || body.Ready != ready {
		t.Fatalf("trust state=%+v, want status=%q configured=%v ready=%v", body, status, configured, ready)
	}
	if !ready && (body.CertificatePEM != "" || body.CACertificatePEM != "" || body.CADERHashSHA256 != "" || !body.ExpiresAt.IsZero()) {
		t.Fatalf("not-ready response exposed material=%+v", body)
	}
}

type sqliteTrustFixture struct {
	env      *sqliteHTTPEnv
	material configuration.EnrollmentTLSMaterial
}

// newEstablishedSQLiteTrustFixture configures hostnames, issues the enrollment
// TLS material, and shapes the local identity into role.
func newEstablishedSQLiteTrustFixture(t *testing.T, role domain.TopologyRole) *sqliteTrustFixture {
	t.Helper()
	ctx := context.Background()
	env := newSQLiteHTTPTestServer(t)
	if _, err := env.Config.UpdateEnrollmentHostnames(ctx, []string{"enroll.example"}); err != nil {
		t.Fatalf("configure enrollment hostnames: %v", err)
	}
	material, err := env.Config.EnsureEnrollmentTLSMaterial(ctx)
	if err != nil {
		t.Fatalf("issue enrollment material: %v", err)
	}
	switch role {
	case domain.TopologyRoleNode:
		if _, err := env.Identity.TransitionTo(ctx, role); err != nil {
			t.Fatalf("transition identity: %v", err)
		}
	case domain.TopologyRoleStalePrimary:
		if _, err := env.Identity.RecordImportedSnapshot(ctx, uuid.New(), 2); err != nil {
			t.Fatalf("mark identity stale: %v", err)
		}
	case domain.TopologyRolePrimary, domain.TopologyRolePrimaryWithNodes:
		if err := identity.NewSQLiteStore(env.DB, sqlitedb.NewWriter(env.DB, nil)).UpdateRole(ctx, role); err != nil {
			t.Fatalf("set identity role %s: %v", role, err)
		}
		if _, err := env.Identity.Load(ctx); err != nil {
			t.Fatalf("reload identity role %s: %v", role, err)
		}
	case domain.TopologyRoleStandalone:
	default:
		t.Fatalf("unsupported test role %s", role)
	}
	return &sqliteTrustFixture{env: env, material: material}
}

// serverWithUnloadedIdentity serves the same stores with an identity service
// that never loaded its cached identity.
func (f *sqliteTrustFixture) serverWithUnloadedIdentity(t *testing.T) http.Handler {
	t.Helper()
	unloaded := identity.NewService(identity.NewSQLiteStore(f.env.DB, sqlitedb.NewWriter(f.env.DB, nil)))
	return httpserver.New(config.Config{UIDist: "", SessionCookieName: "proxycore_session", SessionTTL: time.Hour}, log.New(io.Discard, "", 0),
		httpserver.WithAuthService(f.env.Auth),
		httpserver.WithConfigurationStore(f.env.Config),
		httpserver.WithIdentityService(unloaded),
	).Handler()
}

type trustPersistenceState struct {
	SettingsUpdatedAt string
	CAUpdatedAt       string
	SecretCount       int
	StateCount        int
}

func trustDatabaseState(t *testing.T, db *sql.DB) trustPersistenceState {
	t.Helper()
	var state trustPersistenceState
	if err := db.QueryRow(`select updated_at from installation_settings where id = ?`, "default").Scan(&state.SettingsUpdatedAt); err != nil {
		t.Fatalf("read settings timestamp: %v", err)
	}
	if err := db.QueryRow(`select updated_at from internal_ca where id = ?`, "default").Scan(&state.CAUpdatedAt); err != nil {
		t.Fatalf("read CA timestamp: %v", err)
	}
	if err := db.QueryRow(`select count(*) from secrets`).Scan(&state.SecretCount); err != nil {
		t.Fatalf("count secrets: %v", err)
	}
	if err := db.QueryRow(`select count(*) from internal_ca_enrollment_state`).Scan(&state.StateCount); err != nil {
		t.Fatalf("count enrollment state: %v", err)
	}
	return state
}

func TestEnrollmentTrustEndpointsAreOwnerOnly(t *testing.T) {
	env := newSQLiteHTTPTestServer(t)
	handler := env.Handler
	for _, path := range []string{enrollmentTrustPath, enrollmentTrustCAPEM} {
		unauthenticated := doJSON(t, handler, http.MethodGet, path, nil, nil)
		if unauthenticated.Code != http.StatusUnauthorized {
			t.Fatalf("%s unauthenticated status=%d body=%s", path, unauthenticated.Code, unauthenticated.Body.String())
		}
	}

	ownerCookie := sqliteOwnerCookie(t, handler)
	created := doJSON(t, handler, http.MethodPost, "/api/users", map[string]any{
		"username": "operator",
		"password": "correct horse battery staple",
		"role":     "operator",
	}, ownerCookie)
	if created.Code != http.StatusCreated {
		t.Fatalf("create operator status=%d body=%s", created.Code, created.Body.String())
	}
	operatorCookie := sqliteLoginCookie(t, handler, "operator", "correct horse battery staple")
	for _, path := range []string{enrollmentTrustPath, enrollmentTrustCAPEM} {
		operator := doJSON(t, handler, http.MethodGet, path, nil, operatorCookie)
		if operator.Code != http.StatusForbidden {
			t.Fatalf("%s operator status=%d body=%s", path, operator.Code, operator.Body.String())
		}
	}
}

func TestEnrollmentTrustTopologyRoleMatrix(t *testing.T) {
	cases := []struct {
		name            string
		role            domain.TopologyRole
		staleGeneration bool
		unloaded        bool
		allowed         bool
	}{
		{name: "standalone-primary", role: domain.TopologyRoleStandalone, allowed: true},
		{name: "primary", role: domain.TopologyRolePrimary, allowed: true},
		{name: "primary-with-nodes", role: domain.TopologyRolePrimaryWithNodes, allowed: true},
		{name: "node", role: domain.TopologyRoleNode},
		{name: "stale-primary", role: domain.TopologyRoleStalePrimary},
		{name: "stale-generation", role: domain.TopologyRolePrimary, staleGeneration: true},
		{name: "unloaded", role: domain.TopologyRoleStandalone, unloaded: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newEstablishedSQLiteTrustFixture(t, tc.role)
			ctx := context.Background()
			if tc.staleGeneration {
				if _, err := fixture.env.DB.Exec(`update installation_identity set latest_known_generation = leadership_generation + 1 where id = ?`, "default"); err != nil {
					t.Fatalf("make identity stale by generation: %v", err)
				}
				if _, err := fixture.env.Identity.Load(ctx); err != nil {
					t.Fatalf("reload stale identity: %v", err)
				}
			}
			handler := fixture.env.Handler
			if tc.unloaded {
				handler = fixture.serverWithUnloadedIdentity(t)
			}
			cookie := sqliteOwnerCookie(t, handler)
			if !tc.allowed {
				if _, err := fixture.env.DB.Exec(`update internal_ca set certificate_pem = 'tampered-private-key-token' where id = ?`, "default"); err != nil {
					t.Fatalf("tamper denied-role material: %v", err)
				}
			}
			before := trustDatabaseState(t, fixture.env.DB)
			for _, path := range []string{enrollmentTrustPath, enrollmentTrustAliasPath} {
				response := doJSON(t, handler, http.MethodGet, path, nil, cookie)
				if tc.allowed {
					assertTrustState(t, response, "ready", true, true)
				} else {
					assertEnrollmentTrustDenied(t, response)
				}
			}
			for _, path := range []string{enrollmentTrustCAPEM, enrollmentTrustAliasCAPEM} {
				response := doJSON(t, handler, http.MethodGet, path, nil, cookie)
				if tc.allowed {
					if response.Code != http.StatusOK || response.Body.String() != fixture.material.CACertificatePEM || response.Header().Get("Location") != "" {
						t.Fatalf("%s allowed PEM status/body/location=%d %q %q", path, response.Code, response.Body.String(), response.Header().Get("Location"))
					}
				} else {
					assertEnrollmentTrustDenied(t, response)
				}
			}
			if got := trustDatabaseState(t, fixture.env.DB); got != before {
				t.Fatalf("%s denied/read-only GET changed material persistence: before=%+v after=%+v", tc.name, before, got)
			}
		})
	}
}

func assertEnrollmentTrustDenied(t *testing.T, response *httptest.ResponseRecorder) {
	t.Helper()
	if response.Code != http.StatusForbidden || response.Header().Get("Location") != "" {
		t.Fatalf("trust denial status/location=%d %q body=%s", response.Code, response.Header().Get("Location"), response.Body.String())
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode trust denial: %v", err)
	}
	if body.Error != "Permission denied" {
		t.Fatalf("trust denial was not generic: %s", response.Body.String())
	}
	if strings.Contains(response.Body.String(), "ENROLLMENT_TRUST_") || strings.Contains(response.Body.String(), "tampered-private-key-token") {
		t.Fatalf("trust denial exposed topology/material state: %s", response.Body.String())
	}
}

func TestEnrollmentTrustReportsConfiguredAndReadyStatesWithoutRedirects(t *testing.T) {
	t.Run("unconfigured", func(t *testing.T) {
		env := newSQLiteHTTPTestServer(t)
		cookie := sqliteOwnerCookie(t, env.Handler)
		response := doJSON(t, env.Handler, http.MethodGet, enrollmentTrustPath, nil, cookie)
		assertTrustState(t, response, "unconfigured", false, false)
		if location := response.Header().Get("Location"); location != "" {
			t.Fatalf("unconfigured response redirected to %q", location)
		}
		for _, path := range []string{enrollmentTrustPath + "/", enrollmentTrustCAPEM + "/", enrollmentTrustAliasPath + "/", enrollmentTrustAliasCAPEM + "/"} {
			trailing := doJSON(t, env.Handler, http.MethodGet, path, nil, cookie)
			if trailing.Code != http.StatusNotFound || trailing.Header().Get("Location") != "" {
				t.Fatalf("fixed route %s status/location=%d %q", path, trailing.Code, trailing.Header().Get("Location"))
			}
		}
	})

	t.Run("not-ready", func(t *testing.T) {
		env := newSQLiteHTTPTestServer(t)
		if _, err := env.Config.UpdateEnrollmentHostnames(context.Background(), []string{"enroll.example"}); err != nil {
			t.Fatalf("configure enrollment hostnames: %v", err)
		}
		cookie := sqliteOwnerCookie(t, env.Handler)
		response := doJSON(t, env.Handler, http.MethodGet, enrollmentTrustPath, nil, cookie)
		assertTrustState(t, response, "not-ready", true, false)
		pem := doJSON(t, env.Handler, http.MethodGet, enrollmentTrustCAPEM, nil, cookie)
		if pem.Code != http.StatusConflict || pem.Header().Get("Location") != "" {
			t.Fatalf("not-ready PEM status=%d location=%q body=%s", pem.Code, pem.Header().Get("Location"), pem.Body.String())
		}
		if !strings.Contains(pem.Body.String(), "ENROLLMENT_TRUST_NOT_READY") {
			t.Fatalf("not-ready PEM error=%s", pem.Body.String())
		}
	})

	for _, role := range []domain.TopologyRole{domain.TopologyRoleNode, domain.TopologyRoleStalePrimary} {
		t.Run(string(role), func(t *testing.T) {
			fixture := newEstablishedSQLiteTrustFixture(t, role)
			response := doJSON(t, fixture.env.Handler, http.MethodGet, enrollmentTrustPath, nil, sqliteOwnerCookie(t, fixture.env.Handler))
			assertEnrollmentTrustDenied(t, response)
		})
	}
}

func TestEnrollmentTrustExportsValidatedPublicMaterialAndDoesNotMutate(t *testing.T) {
	fixture := newEstablishedSQLiteTrustFixture(t, domain.TopologyRoleStandalone)
	handler := fixture.env.Handler
	cookie := sqliteOwnerCookie(t, handler)
	before := trustDatabaseState(t, fixture.env.DB)

	metadata := doJSON(t, handler, http.MethodGet, enrollmentTrustPath, nil, cookie)
	if metadata.Code != http.StatusOK || metadata.Header().Get("Cache-Control") != "no-store" || metadata.Header().Get("Content-Type") != "application/json; charset=utf-8" {
		t.Fatalf("metadata status/headers=%d %q %q body=%s", metadata.Code, metadata.Header().Get("Cache-Control"), metadata.Header().Get("Content-Type"), metadata.Body.String())
	}
	var body enrollmentTrustResponse
	if err := json.Unmarshal(metadata.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode metadata: %v", err)
	}
	if body.Status != "ready" || !body.Configured || !body.Ready || body.CADERHashSHA256 != fixture.material.CADERHashSHA256 || body.CACertificatePEM != fixture.material.CACertificatePEM || body.CertificatePEM != fixture.material.CertificatePEM || !body.ExpiresAt.Equal(fixture.material.ExpiresAt) {
		t.Fatalf("metadata=%+v material=%+v", body, fixture.material)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(metadata.Body.Bytes(), &fields); err != nil {
		t.Fatalf("decode metadata fields: %v", err)
	}
	for _, forbiddenField := range []string{"privateKeyPem", "secretId", "ciphertext", "token", "credential", "kek"} {
		if _, ok := fields[forbiddenField]; ok {
			t.Fatalf("metadata exposed forbidden field %q: %s", forbiddenField, metadata.Body.String())
		}
	}
	if strings.Contains(metadata.Body.String(), "PRIVATE KEY") {
		t.Fatalf("metadata leaked private key PEM: %s", metadata.Body.String())
	}

	pem := doJSON(t, handler, http.MethodGet, enrollmentTrustCAPEM, nil, cookie)
	if pem.Code != http.StatusOK || pem.Body.String() != fixture.material.CACertificatePEM || pem.Header().Get("Cache-Control") != "no-store" || pem.Header().Get("Content-Type") != "application/x-pem-file" {
		t.Fatalf("PEM status/body/headers=%d %q %q %s", pem.Code, pem.Header().Get("Content-Type"), pem.Header().Get("Cache-Control"), pem.Body.String())
	}
	if got := pem.Header().Get("Content-Disposition"); got != `attachment; filename="proxycore-enrollment-ca.pem"` {
		t.Fatalf("PEM disposition=%q", got)
	}
	if location := pem.Header().Get("Location"); location != "" {
		t.Fatalf("PEM response redirected to %q", location)
	}
	if got := trustDatabaseState(t, fixture.env.DB); got != before {
		t.Fatalf("GET changed certificate/configuration persistence: before=%+v after=%+v", before, got)
	}
}

func TestEnrollmentTrustFailsClosedWithGenericTamperError(t *testing.T) {
	fixture := newEstablishedSQLiteTrustFixture(t, domain.TopologyRoleStandalone)
	if _, err := fixture.env.DB.Exec(`update internal_ca set certificate_pem = 'tampered-private-token' where id = ?`, "default"); err != nil {
		t.Fatalf("tamper established CA: %v", err)
	}
	response := doJSON(t, fixture.env.Handler, http.MethodGet, enrollmentTrustPath, nil, sqliteOwnerCookie(t, fixture.env.Handler))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("tampered trust status=%d body=%s", response.Code, response.Body.String())
	}
	body := strings.ToLower(response.Body.String())
	for _, forbidden := range []string{"tampered-private-token", "private key", "secret", "token", "credential", "kek", "ciphertext"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("tamper response leaked %q: %s", forbidden, response.Body.String())
		}
	}
	if !strings.Contains(body, "enrollment trust unavailable") {
		t.Fatalf("tamper response is not generic: %s", response.Body.String())
	}
}
