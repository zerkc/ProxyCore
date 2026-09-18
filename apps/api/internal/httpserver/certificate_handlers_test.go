package httpserver_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
)

func TestEnrollmentTrustEndpointsAreOwnerOnly(t *testing.T) {
	srv := newConfigTestServer(t)
	for _, path := range []string{enrollmentTrustPath, enrollmentTrustCAPEM} {
		unauthenticated := doJSON(t, srv.Handler(), http.MethodGet, path, nil, nil)
		if unauthenticated.Code != http.StatusUnauthorized {
			t.Fatalf("%s unauthenticated status=%d body=%s", path, unauthenticated.Code, unauthenticated.Body.String())
		}
	}

	ownerCookie := loginOwner(t, srv)
	created := doJSON(t, srv.Handler(), http.MethodPost, "/api/users", map[string]any{
		"username": "operator",
		"password": "correct horse battery staple",
		"role":     "operator",
	}, ownerCookie)
	if created.Code != http.StatusCreated {
		t.Fatalf("create operator status=%d body=%s", created.Code, created.Body.String())
	}
	operatorCookie := loginUser(t, srv, "operator", "correct horse battery staple")
	for _, path := range []string{enrollmentTrustPath, enrollmentTrustCAPEM} {
		operator := doJSON(t, srv.Handler(), http.MethodGet, path, nil, operatorCookie)
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
			fixture := newEstablishedTrustFixture(t, tc.role)
			if tc.staleGeneration {
				if _, err := fixture.pool.Exec(t.Context(), `update installation_identity set latest_known_generation = leadership_generation + 1 where id = $1`, "default"); err != nil {
					t.Fatalf("make identity stale by generation: %v", err)
				}
				if _, err := fixture.identity.Load(t.Context()); err != nil {
					t.Fatalf("reload stale identity: %v", err)
				}
			}
			srv := fixture.server
			if tc.unloaded {
				srv = fixture.serverWithUnloadedIdentity()
			}
			cookie := loginOwner(t, srv)
			if !tc.allowed {
				if _, err := fixture.pool.Exec(t.Context(), `update internal_ca set certificate_pem = 'tampered-private-key-token' where id = $1`, "default"); err != nil {
					t.Fatalf("tamper denied-role material: %v", err)
				}
			}
			before := trustDatabaseState(t, fixture.pool)
			for _, path := range []string{enrollmentTrustPath, enrollmentTrustAliasPath} {
				response := doJSON(t, srv.Handler(), http.MethodGet, path, nil, cookie)
				if tc.allowed {
					assertTrustState(t, response, "ready", true, true)
				} else {
					assertEnrollmentTrustDenied(t, response)
				}
			}
			for _, path := range []string{enrollmentTrustCAPEM, enrollmentTrustAliasCAPEM} {
				response := doJSON(t, srv.Handler(), http.MethodGet, path, nil, cookie)
				if tc.allowed {
					if response.Code != http.StatusOK || response.Body.String() != fixture.material.CACertificatePEM || response.Header().Get("Location") != "" {
						t.Fatalf("%s allowed PEM status/body/location=%d %q %q", path, response.Code, response.Body.String(), response.Header().Get("Location"))
					}
				} else {
					assertEnrollmentTrustDenied(t, response)
				}
			}
			if got := trustDatabaseState(t, fixture.pool); got != before {
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
		srv := newConfigTestServer(t)
		cookie := loginOwner(t, srv)
		response := doJSON(t, srv.Handler(), http.MethodGet, enrollmentTrustPath, nil, cookie)
		assertTrustState(t, response, "unconfigured", false, false)
		if location := response.Header().Get("Location"); location != "" {
			t.Fatalf("unconfigured response redirected to %q", location)
		}
		for _, path := range []string{enrollmentTrustPath + "/", enrollmentTrustCAPEM + "/", enrollmentTrustAliasPath + "/", enrollmentTrustAliasCAPEM + "/"} {
			trailing := doJSON(t, srv.Handler(), http.MethodGet, path, nil, cookie)
			if trailing.Code != http.StatusNotFound || trailing.Header().Get("Location") != "" {
				t.Fatalf("fixed route %s status/location=%d %q", path, trailing.Code, trailing.Header().Get("Location"))
			}
		}
	})

	t.Run("not-ready", func(t *testing.T) {
		srv := newConfigTestServerForRoleWithEnrollment(t, domain.TopologyRoleStandalone, []string{"enroll.example"})
		cookie := loginOwner(t, srv)
		response := doJSON(t, srv.Handler(), http.MethodGet, enrollmentTrustPath, nil, cookie)
		assertTrustState(t, response, "not-ready", true, false)
		pem := doJSON(t, srv.Handler(), http.MethodGet, enrollmentTrustCAPEM, nil, cookie)
		if pem.Code != http.StatusConflict || pem.Header().Get("Location") != "" {
			t.Fatalf("not-ready PEM status=%d location=%q body=%s", pem.Code, pem.Header().Get("Location"), pem.Body.String())
		}
		if !strings.Contains(pem.Body.String(), "ENROLLMENT_TRUST_NOT_READY") {
			t.Fatalf("not-ready PEM error=%s", pem.Body.String())
		}
	})

	for _, role := range []domain.TopologyRole{domain.TopologyRoleNode, domain.TopologyRoleStalePrimary} {
		t.Run(string(role), func(t *testing.T) {
			srv := newConfigTestServerForRoleWithEnrollment(t, role, []string{"enroll.example"})
			response := doJSON(t, srv.Handler(), http.MethodGet, enrollmentTrustPath, nil, loginOwner(t, srv))
			assertEnrollmentTrustDenied(t, response)
		})
	}
}

func TestEnrollmentTrustExportsValidatedPublicMaterialAndDoesNotMutate(t *testing.T) {
	fixture := newEstablishedTrustFixture(t, domain.TopologyRoleStandalone)
	cookie := loginOwner(t, fixture.server)
	before := trustDatabaseState(t, fixture.pool)

	metadata := doJSON(t, fixture.server.Handler(), http.MethodGet, enrollmentTrustPath, nil, cookie)
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

	pem := doJSON(t, fixture.server.Handler(), http.MethodGet, enrollmentTrustCAPEM, nil, cookie)
	if pem.Code != http.StatusOK || pem.Body.String() != fixture.material.CACertificatePEM || pem.Header().Get("Cache-Control") != "no-store" || pem.Header().Get("Content-Type") != "application/x-pem-file" {
		t.Fatalf("PEM status/body/headers=%d %q %q %s", pem.Code, pem.Header().Get("Content-Type"), pem.Header().Get("Cache-Control"), pem.Body.String())
	}
	if got := pem.Header().Get("Content-Disposition"); got != `attachment; filename="proxycore-enrollment-ca.pem"` {
		t.Fatalf("PEM disposition=%q", got)
	}
	if location := pem.Header().Get("Location"); location != "" {
		t.Fatalf("PEM response redirected to %q", location)
	}
	if got := trustDatabaseState(t, fixture.pool); got != before {
		t.Fatalf("GET changed certificate/configuration persistence: before=%+v after=%+v", before, got)
	}
}

func TestEnrollmentTrustFailsClosedWithGenericTamperError(t *testing.T) {
	fixture := newEstablishedTrustFixture(t, domain.TopologyRoleStandalone)
	if _, err := fixture.pool.Exec(t.Context(), `update internal_ca set certificate_pem = 'tampered-private-token' where id = $1`, "default"); err != nil {
		t.Fatalf("tamper established CA: %v", err)
	}
	response := doJSON(t, fixture.server.Handler(), http.MethodGet, enrollmentTrustPath, nil, loginOwner(t, fixture.server))
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
