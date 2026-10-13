package httpserver_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
