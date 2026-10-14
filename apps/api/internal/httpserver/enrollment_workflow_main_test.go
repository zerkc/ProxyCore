package httpserver_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/enrollment"
	"github.com/zerkc/ProxyCore/apps/api/internal/httpserver"
	replicationsnapshot "github.com/zerkc/ProxyCore/apps/api/internal/snapshot"
)

func mainWorkflowOptions(t *testing.T, called *bool) httpserver.EnrollmentWorkflowHandlerOptions {
	t.Helper()
	envelope := workflowTestEnvelope(t)
	return httpserver.EnrollmentWorkflowHandlerOptions{
		Cache:       enrollment.NewDraftCache(enrollment.DraftCacheOptions{Now: func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) }}),
		VerifyToken: func(context.Context, string) error { return nil },
		FetchSnapshot: func(context.Context, string, string) (*replicationsnapshot.Envelope, error) {
			*called = true
			return envelope, nil
		},
		Validator: snapshotValidatorFunc(func(context.Context, replicationsnapshot.Envelope) replicationsnapshot.ValidationResult {
			return replicationsnapshot.ValidationResult{OK: true}
		}),
		Identity: func(context.Context) (httpserver.EnrollmentLocalIdentity, error) {
			return httpserver.EnrollmentLocalIdentity{InstallationID: domain.NewInstallationID(), NodeID: domain.NewNodeID()}, nil
		},
		LocalIngress: func(context.Context) (domain.Ingress, error) {
			return domain.Ingress{IPv4: "192.0.2.7"}, nil
		},
	}
}

func mainWorkflowDraftBody() map[string]string {
	token := "pcenr1_0123456789abcdef0123456789abcdef_" + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x42}, 32))
	return map[string]string{"token": token, "primaryURL": "https://primary.example:3443/"}
}

func TestMainServerEnrollmentWorkflowOverPlainHTTP(t *testing.T) {
	called := false
	env := newSQLiteHTTPTestServer(t, httpserver.WithEnrollmentWorkflow(mainWorkflowOptions(t, &called)))
	owner := sqliteOwnerCookie(t, env.Handler)

	rec := doJSON(t, env.Handler, http.MethodPost, httpserver.EnrollmentDraftPath, mainWorkflowDraftBody(), owner)
	if rec.Code != http.StatusOK || !called {
		t.Fatalf("owner draft status=%d called=%t body=%s", rec.Code, called, rec.Body.String())
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("cache-control=%q", rec.Header().Get("Cache-Control"))
	}
	var preview struct {
		DraftID string `json:"draftId"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &preview); err != nil || preview.DraftID == "" {
		t.Fatalf("draft body=%s err=%v", rec.Body.String(), err)
	}
	if rec := doJSON(t, env.Handler, http.MethodPost, httpserver.EnrollmentPreviewPath, map[string]string{"draftId": preview.DraftID}, owner); rec.Code != http.StatusOK {
		t.Fatalf("preview status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := doJSON(t, env.Handler, http.MethodPost, httpserver.EnrollmentRecoverPath, map[string]string{"token": mainWorkflowDraftBody()["token"]}, owner); rec.Code != http.StatusOK {
		t.Fatalf("recover status=%d body=%s", rec.Code, rec.Body.String())
	}
	// Confirm is routed to the workflow handler; without a converter it is denied, never 404.
	if rec := doJSON(t, env.Handler, http.MethodPost, httpserver.EnrollmentConfirmPath, map[string]string{"draftId": preview.DraftID}, owner); rec.Code != http.StatusConflict {
		t.Fatalf("confirm status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestMainServerEnrollmentWorkflowAuthAndMethodGuards(t *testing.T) {
	called := false
	env := newSQLiteHTTPTestServer(t, httpserver.WithEnrollmentWorkflow(mainWorkflowOptions(t, &called)))
	owner := sqliteOwnerCookie(t, env.Handler)
	if rec := doJSON(t, env.Handler, http.MethodPost, "/api/users", map[string]any{
		"username": "operator", "password": "operator horse battery staple", "role": "operator",
	}, owner); rec.Code != http.StatusCreated {
		t.Fatalf("create operator status=%d body=%s", rec.Code, rec.Body.String())
	}
	login := doJSON(t, env.Handler, http.MethodPost, "/api/auth/login", map[string]any{
		"username": "operator", "password": "operator horse battery staple",
	}, nil)
	var operator *http.Cookie
	if cookies := login.Result().Cookies(); len(cookies) > 0 {
		operator = cookies[0]
	}

	for _, path := range []string{httpserver.EnrollmentDraftPath, httpserver.EnrollmentPreviewPath, httpserver.EnrollmentConfirmPath, httpserver.EnrollmentRecoverPath} {
		if rec := doJSON(t, env.Handler, http.MethodPost, path, mainWorkflowDraftBody(), nil); rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s anonymous status=%d", path, rec.Code)
		}
		if rec := doJSON(t, env.Handler, http.MethodPost, path, mainWorkflowDraftBody(), operator); rec.Code != http.StatusForbidden {
			t.Fatalf("%s non-owner status=%d", path, rec.Code)
		}
		rec := doJSON(t, env.Handler, http.MethodGet, path, nil, owner)
		if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodPost {
			t.Fatalf("%s GET status=%d allow=%q", path, rec.Code, rec.Header().Get("Allow"))
		}
	}
	if called {
		t.Fatal("snapshot fetched for a refused request")
	}
}

func TestMainServerEnrollmentWorkflowUnavailableReturns503(t *testing.T) {
	env := newSQLiteHTTPTestServer(t)
	owner := sqliteOwnerCookie(t, env.Handler)
	rec := doJSON(t, env.Handler, http.MethodPost, httpserver.EnrollmentDraftPath, mainWorkflowDraftBody(), owner)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestDedicatedEnrollmentListenerStillRequiresTLS13(t *testing.T) {
	called := false
	opts := mainWorkflowOptions(t, &called)
	handler := httpserver.NewEnrollmentIdentityMux(httpserver.EnrollmentIdentityHandlerOptions{Workflow: &opts})
	body, _ := json.Marshal(mainWorkflowDraftBody())
	req := httptest.NewRequest(http.MethodPost, httpserver.EnrollmentDraftPath, bytes.NewReader(body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || called {
		t.Fatalf("plain status=%d called=%t", rec.Code, called)
	}
	req = httptest.NewRequest(http.MethodPost, httpserver.EnrollmentDraftPath, bytes.NewReader(body))
	req.TLS = &tls.ConnectionState{Version: tls.VersionTLS12, HandshakeComplete: true}
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || called {
		t.Fatalf("tls1.2 status=%d called=%t", rec.Code, called)
	}
}
