package httpserver_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/enrollment"
	"github.com/zerkc/ProxyCore/apps/api/internal/httpserver"
)

func TestSQLiteEnrollmentTokenHTTPWorkflowPersistsOnlyHashAndListsRedactedMetadata(t *testing.T) {
	env := newSQLiteHTTPTestServer(t)
	cookie := sqliteOwnerCookie(t, env.Handler)

	createRequest := httptest.NewRequest(http.MethodPost, httpserver.EnrollmentTokensPath, strings.NewReader(`{}`))
	createRequest.AddCookie(cookie)
	createdResponse := httptest.NewRecorder()
	env.Handler.ServeHTTP(createdResponse, createRequest)
	if createdResponse.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", createdResponse.Code, createdResponse.Body.String())
	}
	var created httpserver.EnrollmentTokenCreation
	if err := json.Unmarshal(createdResponse.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}

	var storedSelector, storedHash string
	if err := env.DB.QueryRow(`select token_selector, token_hash from enrollment_tokens where id = ?`, created.ID).Scan(&storedSelector, &storedHash); err != nil {
		t.Fatal(err)
	}
	if storedSelector != created.Selector || storedHash == "" || strings.Contains(storedHash, created.Token) || strings.Contains(storedHash, "PRIVATE") {
		t.Fatalf("stored token projection selector=%q hash=%q", storedSelector, storedHash)
	}

	listRequest := httptest.NewRequest(http.MethodGet, httpserver.EnrollmentTokensPath, nil)
	listRequest.AddCookie(cookie)
	listResponse := httptest.NewRecorder()
	env.Handler.ServeHTTP(listResponse, listRequest)
	if listResponse.Code != http.StatusOK || strings.Contains(listResponse.Body.String(), created.Token) {
		t.Fatalf("list status/body=%d %s", listResponse.Code, listResponse.Body.String())
	}

	revokeRequest := httptest.NewRequest(http.MethodPost, httpserver.EnrollmentTokensPath+"/"+created.ID+"/revoke?confirm=replace-and-revoke", nil)
	revokeRequest.AddCookie(cookie)
	revokeResponse := httptest.NewRecorder()
	env.Handler.ServeHTTP(revokeResponse, revokeRequest)
	if revokeResponse.Code != http.StatusNoContent {
		t.Fatalf("revoke status/body=%d %s", revokeResponse.Code, revokeResponse.Body.String())
	}

	var revokedAt *string
	if err := env.DB.QueryRow(`select revoked_at from enrollment_tokens where id = ?`, created.ID).Scan(&revokedAt); err != nil {
		t.Fatal(err)
	}
	if revokedAt == nil {
		t.Fatal("revoke did not persist lifecycle metadata")
	}
}

// establishV2Trust configures enrollment hostnames and issues the enrollment
// TLS material so token creation can embed a CA and verify host coverage.
func establishV2Trust(t *testing.T, env *sqliteHTTPEnv, hostnames ...string) configuration.EnrollmentTLSMaterial {
	t.Helper()
	ctx := context.Background()
	if _, err := env.Config.UpdateEnrollmentHostnames(ctx, hostnames); err != nil {
		t.Fatalf("configure enrollment hostnames: %v", err)
	}
	material, err := env.Config.EnsureEnrollmentTLSMaterial(ctx)
	if err != nil {
		t.Fatalf("issue enrollment material: %v", err)
	}
	return material
}

func postEnrollmentToken(t *testing.T, env *sqliteHTTPEnv, cookie *http.Cookie, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, httpserver.EnrollmentTokensPath, strings.NewReader(body))
	request.AddCookie(cookie)
	response := httptest.NewRecorder()
	env.Handler.ServeHTTP(response, request)
	return response
}

func TestSQLiteSelfContainedTokenEmbedsDefaultURLAndEnrollmentCA(t *testing.T) {
	env := newSQLiteHTTPTestServer(t, httpserver.WithSelfContainedEnrollmentTokens())
	cookie := sqliteOwnerCookie(t, env.Handler)
	material := establishV2Trust(t, env, "192.0.2.10", "enroll.example")

	response := postEnrollmentToken(t, env, cookie, `{}`)
	if response.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", response.Code, response.Body.String())
	}
	var created httpserver.EnrollmentTokenCreation
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	claims, ok := enrollment.ParseTokenClaims(created.Token)
	if !ok || claims.Version != 2 || claims.Selector != created.Selector {
		t.Fatalf("token claims=%+v ok=%v", claims, ok)
	}
	if claims.PrimaryURL != "https://192.0.2.10:3443/" {
		t.Fatalf("default URL = %q, want gateway ip + default port", claims.PrimaryURL)
	}
	if claims.CAPEM != material.CACertificatePEM {
		t.Fatalf("embedded CA does not match the enrollment CA")
	}
	// The stored hash is the v1 hash of the secret: nothing else is persisted.
	var storedHash string
	if err := env.DB.QueryRow(`select token_hash from enrollment_tokens where id = ?`, created.ID).Scan(&storedHash); err != nil {
		t.Fatal(err)
	}
	if storedHash == "" || strings.Contains(storedHash, "CERTIFICATE") || strings.Contains(storedHash, claims.PrimaryURL) {
		t.Fatalf("stored hash leaks routing data: %q", storedHash)
	}

	custom := postEnrollmentToken(t, env, cookie, `{"primaryUrl":"https://enroll.example:9443"}`)
	if custom.Code != http.StatusCreated {
		t.Fatalf("custom status=%d body=%s", custom.Code, custom.Body.String())
	}
	var customCreated httpserver.EnrollmentTokenCreation
	_ = json.Unmarshal(custom.Body.Bytes(), &customCreated)
	if got, _ := enrollment.ParseTokenClaims(customCreated.Token); got.PrimaryURL != "https://enroll.example:9443/" {
		t.Fatalf("custom URL = %q", got.PrimaryURL)
	}
}

func TestSQLiteSelfContainedTokenRefusesWithoutMintingToken(t *testing.T) {
	assertRefusal := func(t *testing.T, env *sqliteHTTPEnv, cookie *http.Cookie, body string, status int, code string) {
		t.Helper()
		response := postEnrollmentToken(t, env, cookie, body)
		var payload struct {
			Code  string `json:"code"`
			Token string `json:"token"`
		}
		_ = json.Unmarshal(response.Body.Bytes(), &payload)
		if response.Code != status || payload.Code != code || payload.Token != "" {
			t.Fatalf("status=%d code=%q token=%q body=%s, want %d %s", response.Code, payload.Code, payload.Token, response.Body.String(), status, code)
		}
		var count int
		if err := env.DB.QueryRow(`select count(*) from enrollment_tokens`).Scan(&count); err != nil || count != 0 {
			t.Fatalf("tokens persisted = %d err=%v, want 0", count, err)
		}
	}

	t.Run("trust not ready", func(t *testing.T) {
		env := newSQLiteHTTPTestServer(t, httpserver.WithSelfContainedEnrollmentTokens())
		assertRefusal(t, env, sqliteOwnerCookie(t, env.Handler), `{}`, http.StatusConflict, "ENROLLMENT_TRUST_NOT_READY")
	})
	t.Run("gateway ip not in certificate", func(t *testing.T) {
		env := newSQLiteHTTPTestServer(t, httpserver.WithSelfContainedEnrollmentTokens())
		cookie := sqliteOwnerCookie(t, env.Handler)
		establishV2Trust(t, env, "enroll.example")
		assertRefusal(t, env, cookie, `{}`, http.StatusUnprocessableEntity, "PRIMARY_URL_NOT_IN_CERT")
	})
	t.Run("invalid url", func(t *testing.T) {
		env := newSQLiteHTTPTestServer(t, httpserver.WithSelfContainedEnrollmentTokens())
		cookie := sqliteOwnerCookie(t, env.Handler)
		establishV2Trust(t, env, "192.0.2.10")
		assertRefusal(t, env, cookie, `{"primaryUrl":"http://192.0.2.10:3443/"}`, http.StatusUnprocessableEntity, "PRIMARY_URL_INVALID")
		assertRefusal(t, env, cookie, `{"primaryUrl":7}`, http.StatusBadRequest, "")
	})
	t.Run("no default and none supplied", func(t *testing.T) {
		env := newSQLiteHTTPTestServer(t, httpserver.WithSelfContainedEnrollmentTokens(), httpserver.WithDefaultIngress(domain.Ingress{}))
		cookie := sqliteOwnerCookie(t, env.Handler)
		establishV2Trust(t, env, "enroll.example")
		assertRefusal(t, env, cookie, `{}`, http.StatusUnprocessableEntity, "PRIMARY_URL_REQUIRED")
	})
}

func TestSQLiteEnrollmentTokenDefaultsEndpoint(t *testing.T) {
	get := func(env *sqliteHTTPEnv, cookie *http.Cookie) (*httptest.ResponseRecorder, map[string]any) {
		request := httptest.NewRequest(http.MethodGet, httpserver.EnrollmentTokensPath+"/defaults", nil)
		if cookie != nil {
			request.AddCookie(cookie)
		}
		response := httptest.NewRecorder()
		env.Handler.ServeHTTP(response, request)
		var body map[string]any
		_ = json.Unmarshal(response.Body.Bytes(), &body)
		return response, body
	}
	t.Run("requires an owner session", func(t *testing.T) {
		env := newSQLiteHTTPTestServer(t)
		if response, _ := get(env, nil); response.Code != http.StatusUnauthorized {
			t.Fatalf("status=%d", response.Code)
		}
	})
	t.Run("not ready is not an error", func(t *testing.T) {
		env := newSQLiteHTTPTestServer(t)
		response, body := get(env, sqliteOwnerCookie(t, env.Handler))
		if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" ||
			body["primaryUrl"] != "https://192.0.2.10:3443/" || body["gatewayIPv4"] != "192.0.2.10" ||
			body["port"] != float64(3443) || body["trustReady"] != false || body["hostCovered"] != false {
			t.Fatalf("status=%d headers=%q body=%v", response.Code, response.Header().Get("Cache-Control"), body)
		}
	})
	t.Run("ready and covered", func(t *testing.T) {
		env := newSQLiteHTTPTestServer(t)
		cookie := sqliteOwnerCookie(t, env.Handler)
		establishV2Trust(t, env, "192.0.2.10")
		_, body := get(env, cookie)
		if body["trustReady"] != true || body["hostCovered"] != true {
			t.Fatalf("body=%v", body)
		}
	})
	t.Run("ready but gateway ip uncovered", func(t *testing.T) {
		env := newSQLiteHTTPTestServer(t)
		cookie := sqliteOwnerCookie(t, env.Handler)
		establishV2Trust(t, env, "enroll.example")
		_, body := get(env, cookie)
		if body["trustReady"] != true || body["hostCovered"] != false {
			t.Fatalf("body=%v", body)
		}
	})
	t.Run("no ingress configured", func(t *testing.T) {
		env := newSQLiteHTTPTestServer(t, httpserver.WithDefaultIngress(domain.Ingress{}))
		response, body := get(env, sqliteOwnerCookie(t, env.Handler))
		if response.Code != http.StatusOK || body["primaryUrl"] != "" || body["gatewayIPv4"] != "" ||
			body["trustReady"] != false || body["hostCovered"] != false {
			t.Fatalf("status=%d body=%v", response.Code, body)
		}
	})
}
