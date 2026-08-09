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

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/zerkc/ProxyCore/apps/api/internal/auth"
	"github.com/zerkc/ProxyCore/apps/api/internal/config"
	"github.com/zerkc/ProxyCore/apps/api/internal/httpserver"
)

func TestAuthHandlersBootstrapLoginLogout(t *testing.T) {
	srv := newAuthTestServer(t)

	bootstrap := postJSON(t, srv.Handler(), "/api/auth/bootstrap", map[string]string{
		"username": " Owner ",
		"password": "correct horse battery staple",
	}, nil)
	if bootstrap.Code != http.StatusCreated {
		t.Fatalf("bootstrap status=%d body=%s", bootstrap.Code, bootstrap.Body.String())
	}
	var bootstrapBody struct {
		User struct {
			Username string `json:"username"`
			Role     string `json:"role"`
		} `json:"user"`
	}
	if err := json.Unmarshal(bootstrap.Body.Bytes(), &bootstrapBody); err != nil {
		t.Fatalf("decode bootstrap: %v", err)
	}
	if bootstrapBody.User.Username != "owner" || bootstrapBody.User.Role != "owner" {
		t.Fatalf("bootstrap user=%+v", bootstrapBody.User)
	}

	secondBootstrap := postJSON(t, srv.Handler(), "/api/auth/bootstrap", map[string]string{
		"username": "other",
		"password": "correct horse battery staple",
	}, nil)
	if secondBootstrap.Code != http.StatusConflict {
		t.Fatalf("second bootstrap status=%d body=%s", secondBootstrap.Code, secondBootstrap.Body.String())
	}

	login := postJSON(t, srv.Handler(), "/api/auth/login", map[string]string{
		"username": "OWNER",
		"password": "correct horse battery staple",
	}, nil)
	if login.Code != http.StatusOK {
		t.Fatalf("login status=%d body=%s", login.Code, login.Body.String())
	}
	cookies := login.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("login cookies=%v", cookies)
	}
	sessionCookie := cookies[0]
	if sessionCookie.Name != "proxycore_session" {
		t.Fatalf("cookie name=%q", sessionCookie.Name)
	}
	if !sessionCookie.HttpOnly || sessionCookie.Path != "/" || sessionCookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("cookie attributes=%+v", sessionCookie)
	}
	if sessionCookie.Value == "" || strings.Contains(sessionCookie.Value, "=") {
		t.Fatalf("unexpected cookie token=%q", sessionCookie.Value)
	}

	logout := postJSON(t, srv.Handler(), "/api/auth/logout", nil, sessionCookie)
	if logout.Code != http.StatusNoContent {
		t.Fatalf("logout status=%d body=%s", logout.Code, logout.Body.String())
	}
	clearCookies := logout.Result().Cookies()
	if len(clearCookies) != 1 || clearCookies[0].Name != "proxycore_session" || clearCookies[0].MaxAge != -1 {
		t.Fatalf("logout clear cookies=%v", clearCookies)
	}
}

func newAuthTestServer(t *testing.T) *httpserver.Server {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping database-backed auth handler tests in short mode")
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

	schema := "proxycore_go_auth_test_" +
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

	store := auth.NewPostgresStore(pool)
	if err := store.EnsureSchema(ctx); err != nil {
		t.Fatalf("ensure auth schema: %v", err)
	}

	svc := auth.NewService(store, auth.ServiceOptions{SessionTTL: time.Hour})
	return httpserver.New(config.Config{
		UIDist:            t.TempDir(),
		SessionCookieName: "proxycore_session",
		SessionTTL:        time.Hour,
	}, log.New(io.Discard, "", 0), httpserver.WithAuthService(svc))
}

func postJSON(t *testing.T, handler http.Handler, target string, body any, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
	}
	req := httptest.NewRequest(http.MethodPost, target, bytes.NewReader(payload))
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
