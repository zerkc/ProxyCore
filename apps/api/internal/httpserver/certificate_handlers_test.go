package httpserver_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// The self-signed lifecycle runs fully offline, so these tests exercise the
// certificate routes over the SQLite stack without ACME.

func TestCertificateRoutesRequireAuthentication(t *testing.T) {
	env := newSQLiteHTTPTestServer(t)
	requests := []struct {
		method string
		path   string
		body   any
	}{
		{http.MethodGet, "/api/certificates", nil},
		{http.MethodGet, "/api/certificates/ca", nil},
		{http.MethodPost, "/api/certificates", map[string]any{"hostnames": []string{"example.test"}, "issuer": "self-signed", "challenge": "none"}},
		{http.MethodPost, "/api/certificates/any-id/renew", nil},
		{http.MethodDelete, "/api/certificates/any-id", nil},
	}
	for _, tc := range requests {
		response := doJSON(t, env.Handler, tc.method, tc.path, tc.body, nil)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s unauthenticated status=%d body=%s", tc.method, tc.path, response.Code, response.Body.String())
		}
	}
}

func TestCertificateSelfSignedIssueRenewDeleteFlow(t *testing.T) {
	env := newSQLiteHTTPTestServer(t)
	cookie := sqliteOwnerCookie(t, env.Handler)

	// Renew enqueues an apply job, which requires a default resolver pool.
	pool := doJSON(t, env.Handler, http.MethodPut, "/api/settings", map[string]any{
		"defaultPool": map[string]any{
			"id":        "default",
			"endpoints": []map[string]any{{"host": "1.1.1.1", "port": 53}},
		},
	}, cookie)
	if pool.Code != http.StatusOK {
		t.Fatalf("seed default pool status=%d body=%s", pool.Code, pool.Body.String())
	}

	issued := doJSON(t, env.Handler, http.MethodPost, "/api/certificates", map[string]any{
		"hostnames": []string{"example.test"},
		"issuer":    "self-signed",
		"challenge": "none",
	}, cookie)
	if issued.Code != http.StatusCreated {
		t.Fatalf("issue status=%d body=%s", issued.Code, issued.Body.String())
	}
	var created struct {
		Certificate struct {
			ID        string   `json:"id"`
			Hostnames []string `json:"hostnames"`
			Issuer    string   `json:"issuer"`
			Status    string   `json:"status"`
		} `json:"certificate"`
	}
	if err := json.Unmarshal(issued.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode issue: %v", err)
	}
	id := created.Certificate.ID
	if id == "" || created.Certificate.Issuer != "self-signed" || created.Certificate.Status != "active" || len(created.Certificate.Hostnames) != 1 || created.Certificate.Hostnames[0] != "example.test" {
		t.Fatalf("issued certificate=%+v", created.Certificate)
	}
	if strings.Contains(issued.Body.String(), "PRIVATE KEY") || strings.Contains(issued.Body.String(), "privateKey") {
		t.Fatalf("issue response leaked private key material: %s", issued.Body.String())
	}

	listed := doJSON(t, env.Handler, http.MethodGet, "/api/certificates", nil, cookie)
	if listed.Code != http.StatusOK || !strings.Contains(listed.Body.String(), id) {
		t.Fatalf("list status=%d body=%s", listed.Code, listed.Body.String())
	}

	renewed := doJSON(t, env.Handler, http.MethodPost, "/api/certificates/"+id+"/renew", nil, cookie)
	if renewed.Code != http.StatusOK {
		t.Fatalf("renew status=%d body=%s", renewed.Code, renewed.Body.String())
	}
	var renewBody struct {
		Certificate struct {
			ID string `json:"id"`
		} `json:"certificate"`
	}
	if err := json.Unmarshal(renewed.Body.Bytes(), &renewBody); err != nil {
		t.Fatalf("decode renew: %v", err)
	}
	if renewBody.Certificate.ID == "" {
		t.Fatalf("renew returned no certificate id: %s", renewed.Body.String())
	}

	deleted := doJSON(t, env.Handler, http.MethodDelete, "/api/certificates/"+id, nil, cookie)
	if deleted.Code != http.StatusOK {
		t.Fatalf("delete status=%d body=%s", deleted.Code, deleted.Body.String())
	}
	again := doJSON(t, env.Handler, http.MethodDelete, "/api/certificates/"+id, nil, cookie)
	if again.Code != http.StatusNotFound {
		t.Fatalf("second delete status=%d body=%s", again.Code, again.Body.String())
	}
	renewMissing := doJSON(t, env.Handler, http.MethodPost, "/api/certificates/"+id+"/renew", nil, cookie)
	if renewMissing.Code != http.StatusNotFound {
		t.Fatalf("renew after delete status=%d body=%s", renewMissing.Code, renewMissing.Body.String())
	}
}

func TestCertificateIssueRejectsInvalidRequests(t *testing.T) {
	env := newSQLiteHTTPTestServer(t)
	cookie := sqliteOwnerCookie(t, env.Handler)

	invalidJSON := doJSON(t, env.Handler, http.MethodPost, "/api/certificates", nil, cookie)
	if invalidJSON.Code != http.StatusBadRequest {
		t.Fatalf("empty body status=%d body=%s", invalidJSON.Code, invalidJSON.Body.String())
	}
	missingHostnames := doJSON(t, env.Handler, http.MethodPost, "/api/certificates", map[string]any{
		"issuer":    "self-signed",
		"challenge": "none",
	}, cookie)
	if missingHostnames.Code != http.StatusBadRequest {
		t.Fatalf("missing hostnames status=%d body=%s", missingHostnames.Code, missingHostnames.Body.String())
	}
}

func TestDownloadInternalCAReturnsPEMWithHeaders(t *testing.T) {
	env := newSQLiteHTTPTestServer(t)
	cookie := sqliteOwnerCookie(t, env.Handler)

	response := doJSON(t, env.Handler, http.MethodGet, "/api/certificates/ca", nil, cookie)
	if response.Code != http.StatusOK {
		t.Fatalf("CA download status=%d body=%s", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Content-Type"); got != "application/x-x509-ca-cert" {
		t.Fatalf("content-type=%q", got)
	}
	if got := response.Header().Get("Content-Disposition"); got != `attachment; filename="proxycore-ca.crt"` {
		t.Fatalf("content-disposition=%q", got)
	}
	if got := response.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("cache-control=%q", got)
	}
	if !strings.HasPrefix(response.Body.String(), "-----BEGIN CERTIFICATE-----") {
		t.Fatalf("CA body is not a certificate PEM: %q", response.Body.String())
	}
	if strings.Contains(response.Body.String(), "PRIVATE KEY") {
		t.Fatal("CA download leaked private key material")
	}
}
