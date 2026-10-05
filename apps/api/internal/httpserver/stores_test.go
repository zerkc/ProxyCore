package httpserver_test

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/auth"
	"github.com/zerkc/ProxyCore/apps/api/internal/config"
	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/httpserver"
)

// fakeConfigurationBackend embeds the nil interface so any method the handlers
// call beyond the overridden ones panics, proving the minimal surface in use.
type fakeConfigurationBackend struct {
	httpserver.ConfigurationBackend
	ingressCalls int
	settingsCall int
	caCalls      int
}

func (f *fakeConfigurationBackend) InitializeIngress(context.Context, domain.Ingress) (bool, error) {
	f.ingressCalls++
	return false, nil
}

func (f *fakeConfigurationBackend) GetSettings(context.Context) (domain.Settings, error) {
	f.settingsCall++
	return domain.Settings{RetentionMaxAgeDays: 7}, nil
}

func (f *fakeConfigurationBackend) EnsureInternalCACertificatePEM(context.Context) (string, error) {
	f.caCalls++
	return "fake-ca-pem", nil
}

func newStoreSeamServer(t *testing.T, opt httpserver.Option) (*httpserver.Server, *http.Cookie) {
	t.Helper()
	authService := auth.NewService(newBackupHTTPAuthStore(), auth.ServiceOptions{SessionTTL: time.Hour})
	owner, err := authService.Bootstrap(context.Background(), "owner", "owner-password")
	if err != nil {
		t.Fatalf("bootstrap owner: %v", err)
	}
	session, err := authService.Login(context.Background(), owner.Username, "owner-password")
	if err != nil {
		t.Fatalf("login owner: %v", err)
	}
	server := httpserver.New(config.Config{UIDist: t.TempDir(), SessionCookieName: "proxycore_session", SessionTTL: time.Hour},
		log.New(io.Discard, "", 0), httpserver.WithAuthService(authService), opt)
	return server, &http.Cookie{Name: "proxycore_session", Value: session.Token}
}

func TestHandlersWorkAgainstMinimalConfigurationBackend(t *testing.T) {
	fake := &fakeConfigurationBackend{}
	server, cookie := newStoreSeamServer(t, httpserver.WithConfigurationStore(fake))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/settings", nil)
	req.AddCookie(cookie)
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"retentionMaxAgeDays":7`) {
		t.Fatalf("GET /api/settings = %d %s", rec.Code, rec.Body.String())
	}
	if fake.ingressCalls != 1 || fake.settingsCall != 1 {
		t.Fatalf("calls ingress=%d settings=%d, want 1/1", fake.ingressCalls, fake.settingsCall)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/certificates/ca", nil)
	req.AddCookie(cookie)
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "fake-ca-pem") || fake.caCalls != 1 {
		t.Fatalf("GET /api/certificates/ca = %d %s calls=%d", rec.Code, rec.Body.String(), fake.caCalls)
	}
}

func TestTypedNilConfigurationStoreMeansNotConfigured(t *testing.T) {
	server, cookie := newStoreSeamServer(t, httpserver.WithConfigurationStore((*configuration.Store)(nil)))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/settings", nil)
	req.AddCookie(cookie)
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET /api/settings with typed-nil store = %d, want 503", rec.Code)
	}
}
