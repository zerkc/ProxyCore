package httpserver_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/auth"
	"github.com/zerkc/ProxyCore/apps/api/internal/cluster"
	"github.com/zerkc/ProxyCore/apps/api/internal/config"
	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/httpserver"
	"github.com/zerkc/ProxyCore/apps/api/internal/identity"
	"github.com/zerkc/ProxyCore/apps/api/internal/secrets"
	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

// sqliteHTTPTestMasterKey is a 32-byte master key, base64 encoded, shared by
// every SQLite store the HTTP tests wire.
var sqliteHTTPTestMasterKey = base64.StdEncoding.EncodeToString([]byte(strings.Repeat("m", 32)))

// sqliteHTTPEnv is one HTTP server over a fresh migrated SQLite database. It
// mirrors cmd/server's sqliteStack composition, which package main keeps
// private, so the handlers see the same stores production wires.
type sqliteHTTPEnv struct {
	Server   *httpserver.Server
	Handler  http.Handler
	DB       *sql.DB
	Auth     *auth.Service
	Config   *configuration.SQLiteConfigurationBackend
	Phase2   *configuration.SQLitePhase2Store
	Keys     *cluster.SQLiteKeyStore
	Identity *identity.Service
}

// Extra options are appended after the standard wiring so tests can add, for
// example, an updater client.
func newSQLiteHTTPTestServer(t *testing.T, extra ...httpserver.Option) *sqliteHTTPEnv {
	t.Helper()
	ctx := context.Background()
	db, writer := openSQLiteHTTPTestDB(t)
	ingress := domain.Ingress{IPv4: "192.0.2.10", IPv6: "2001:db8::10"}
	secretStore := secrets.NewSQLiteStore(db, sqliteHTTPTestMasterKey)
	keys := cluster.NewSQLiteKeyStore(db, writer, sqliteHTTPTestMasterKey)
	authSvc := auth.NewService(auth.NewSQLiteStore(db, writer), auth.ServiceOptions{SessionTTL: time.Hour})
	configStore := configuration.NewSQLiteConfigurationBackend(db, writer, ingress, secretStore)
	phase2 := configuration.NewSQLitePhase2Store(db, writer, keys, sqliteHTTPTestMasterKey)

	identitySvc := identity.NewService(identity.NewSQLiteStore(db, writer))
	if _, _, err := identitySvc.EnsureBootstrapped(ctx); err != nil {
		t.Fatalf("bootstrap identity: %v", err)
	}

	tokenAuthority := httpserver.NewEnrollmentTokenAuthority(phase2, identitySvc, httpserver.EnrollmentTokenAuthorityOptions{
		List: func(ctx context.Context, ownerID string) ([]httpserver.EnrollmentTokenRecord, error) {
			records, err := phase2.ListEnrollmentTokens(ctx, ownerID)
			if err != nil {
				return nil, err
			}
			out := make([]httpserver.EnrollmentTokenRecord, 0, len(records))
			for _, record := range records {
				out = append(out, httpserver.EnrollmentTokenRecord(record))
			}
			return out, nil
		},
	})

	options := []httpserver.Option{
		httpserver.WithAuthService(authSvc),
		httpserver.WithConfigurationStore(configStore),
		httpserver.WithDefaultIngress(ingress),
		httpserver.WithIdentityService(identitySvc),
		httpserver.WithEnrollmentTokenAuthority(tokenAuthority),
		httpserver.WithNodeUnlinker(phase2),
		httpserver.WithNodeLister(phase2),
	}
	server := httpserver.New(config.Config{
		UIDist:            t.TempDir(),
		SessionCookieName: "proxycore_session",
		SessionTTL:        time.Hour,
	}, log.New(io.Discard, "", 0), append(options, extra...)...)
	return &sqliteHTTPEnv{
		Server:   server,
		Handler:  server.Handler(),
		DB:       db,
		Auth:     authSvc,
		Config:   configStore,
		Phase2:   phase2,
		Keys:     keys,
		Identity: identitySvc,
	}
}

// openSQLiteHTTPTestDB opens and migrates a fresh SQLite file under t.TempDir.
func openSQLiteHTTPTestDB(t *testing.T) (*sql.DB, *sqlitedb.Writer) {
	t.Helper()
	db, err := sqlitedb.Open(filepath.Join(t.TempDir(), "proxycore.sqlite"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := sqlitedb.Migrate(context.Background(), db); err != nil {
		t.Fatalf("migrate sqlite: %v", err)
	}
	return db, sqlitedb.NewWriter(db, nil)
}

// sqliteOwnerCookie bootstraps an owner and returns a session cookie.
func sqliteOwnerCookie(t *testing.T, handler http.Handler) *http.Cookie {
	t.Helper()
	const password = "correct horse battery staple"
	bootstrap := doJSON(t, handler, http.MethodPost, "/api/auth/bootstrap", map[string]any{
		"username": "owner",
		"password": password,
	}, nil)
	if bootstrap.Code != http.StatusCreated {
		t.Fatalf("bootstrap status=%d body=%s", bootstrap.Code, bootstrap.Body.String())
	}
	login := doJSON(t, handler, http.MethodPost, "/api/auth/login", map[string]any{
		"username": "owner",
		"password": password,
	}, nil)
	if login.Code != http.StatusOK {
		t.Fatalf("login status=%d body=%s", login.Code, login.Body.String())
	}
	cookies := login.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("login set no session cookie")
	}
	return cookies[0]
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
