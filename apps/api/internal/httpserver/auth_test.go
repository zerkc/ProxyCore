package httpserver_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestAuthHandlersBootstrapLoginLogout(t *testing.T) {
	env := newSQLiteHTTPTestServer(t)
	handler := env.Handler

	bootstrap := doJSON(t, handler, http.MethodPost, "/api/auth/bootstrap", map[string]string{
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

	secondBootstrap := doJSON(t, handler, http.MethodPost, "/api/auth/bootstrap", map[string]string{
		"username": "other",
		"password": "correct horse battery staple",
	}, nil)
	if secondBootstrap.Code != http.StatusConflict {
		t.Fatalf("second bootstrap status=%d body=%s", secondBootstrap.Code, secondBootstrap.Body.String())
	}

	login := doJSON(t, handler, http.MethodPost, "/api/auth/login", map[string]string{
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

	// The session works before logout.
	if before := doJSON(t, handler, http.MethodGet, "/api/users", nil, sessionCookie); before.Code != http.StatusOK {
		t.Fatalf("pre-logout users status=%d body=%s", before.Code, before.Body.String())
	}

	logout := doJSON(t, handler, http.MethodPost, "/api/auth/logout", nil, sessionCookie)
	if logout.Code != http.StatusNoContent {
		t.Fatalf("logout status=%d body=%s", logout.Code, logout.Body.String())
	}
	clearCookies := logout.Result().Cookies()
	if len(clearCookies) != 1 || clearCookies[0].Name != "proxycore_session" || clearCookies[0].MaxAge != -1 {
		t.Fatalf("logout clear cookies=%v", clearCookies)
	}

	// Logout invalidates the server-side session, not just the browser cookie.
	if after := doJSON(t, handler, http.MethodGet, "/api/users", nil, sessionCookie); after.Code != http.StatusUnauthorized {
		t.Fatalf("post-logout users status=%d body=%s", after.Code, after.Body.String())
	}
}

func TestPasswordResetRestrictedSessionAndChange(t *testing.T) {
	env := newSQLiteHTTPTestServer(t)
	handler := env.Handler
	bootstrap := doJSON(t, handler, http.MethodPost, "/api/auth/bootstrap", map[string]string{"username": "owner", "password": "initial-password"}, nil)
	if bootstrap.Code != http.StatusCreated {
		t.Fatalf("bootstrap status=%d body=%s", bootstrap.Code, bootstrap.Body.String())
	}
	temporary, err := env.Auth.ResetPassword(context.Background(), " OWNER ")
	if err != nil {
		t.Fatalf("reset password: %v", err)
	}
	login := doJSON(t, handler, http.MethodPost, "/api/auth/login", map[string]string{"username": "owner", "password": temporary}, nil)
	if login.Code != http.StatusOK {
		t.Fatalf("login status=%d body=%s", login.Code, login.Body.String())
	}
	cookie := login.Result().Cookies()[0]

	restricted := doJSON(t, handler, http.MethodGet, "/api/users", nil, cookie)
	if restricted.Code != http.StatusForbidden || !strings.Contains(restricted.Body.String(), "PASSWORD_CHANGE_REQUIRED") {
		t.Fatalf("restricted status=%d body=%s", restricted.Code, restricted.Body.String())
	}

	changed := doJSON(t, handler, http.MethodPost, "/api/auth/change-password", map[string]string{"password": "new-secure-password"}, cookie)
	if changed.Code != http.StatusOK {
		t.Fatalf("change status=%d body=%s", changed.Code, changed.Body.String())
	}
	allowed := doJSON(t, handler, http.MethodGet, "/api/users", nil, cookie)
	if allowed.Code != http.StatusOK {
		t.Fatalf("allowed status=%d body=%s", allowed.Code, allowed.Body.String())
	}
}
