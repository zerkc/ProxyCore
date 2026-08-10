package httpserver_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zerkc/ProxyCore/apps/api/internal/acme"
	"github.com/zerkc/ProxyCore/apps/api/internal/config"
	"github.com/zerkc/ProxyCore/apps/api/internal/httpserver"
)

func TestAcmeChallengeRoute(t *testing.T) {
	srv := httpserver.New(config.Config{UIDist: t.TempDir()}, nil)

	token := "test-token-" + t.Name()
	keyAuthorization := "token.thumbprint"
	acme.GlobalHttp01Store().Put(token, keyAuthorization, nil)
	t.Cleanup(func() { acme.GlobalHttp01Store().Remove(token) })

	req := httptest.NewRequest(http.MethodGet, "/api/acme-challenge/"+token, nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != keyAuthorization {
		t.Fatalf("body=%q want=%q", got, keyAuthorization)
	}
	if cacheControl := rec.Header().Get("cache-control"); cacheControl != "no-store" {
		t.Fatalf("cache-control=%q", cacheControl)
	}

	missing := httptest.NewRequest(http.MethodGet, "/api/acme-challenge/unknown", nil)
	missingRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(missingRec, missing)
	if missingRec.Code != http.StatusNotFound {
		t.Fatalf("missing status=%d", missingRec.Code)
	}
	if cacheControl := missingRec.Header().Get("cache-control"); cacheControl != "no-store" {
		t.Fatalf("missing cache-control=%q", cacheControl)
	}
}
