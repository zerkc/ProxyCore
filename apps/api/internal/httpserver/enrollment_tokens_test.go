package httpserver_test

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/auth"
	"github.com/zerkc/ProxyCore/apps/api/internal/config"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/enrollment"
	"github.com/zerkc/ProxyCore/apps/api/internal/httpserver"
)

type tokenHTTPAuthStore struct {
	users    map[string]auth.User
	sessions map[string]auth.Session
}

func newTokenHTTPAuthStore() *tokenHTTPAuthStore {
	return &tokenHTTPAuthStore{users: map[string]auth.User{}, sessions: map[string]auth.Session{}}
}

func (s *tokenHTTPAuthStore) ListUsers(context.Context) ([]auth.User, error) {
	users := make([]auth.User, 0, len(s.users))
	for _, user := range s.users {
		users = append(users, user)
	}
	return users, nil
}
func (s *tokenHTTPAuthStore) FindUserByUsername(_ context.Context, username string) (*auth.User, error) {
	for _, user := range s.users {
		if user.Username == username {
			copy := user
			return &copy, nil
		}
	}
	return nil, nil
}
func (s *tokenHTTPAuthStore) FindUserByID(_ context.Context, id string) (*auth.User, error) {
	user, ok := s.users[id]
	if !ok {
		return nil, nil
	}
	copy := user
	return &copy, nil
}
func (s *tokenHTTPAuthStore) CreateUser(_ context.Context, user auth.User) (auth.User, error) {
	s.users[user.ID] = user
	return user, nil
}
func (s *tokenHTTPAuthStore) UpdateUser(_ context.Context, id string, patch auth.UserPatch) (auth.User, error) {
	user, ok := s.users[id]
	if !ok {
		return auth.User{}, errors.New("user not found")
	}
	if patch.Role != nil {
		user.Role = *patch.Role
	}
	if patch.Active != nil {
		user.Active = *patch.Active
	}
	if patch.PasswordHash != nil {
		user.PasswordHash = *patch.PasswordHash
	}
	s.users[id] = user
	return user, nil
}
func (s *tokenHTTPAuthStore) DeleteUser(_ context.Context, id string) error {
	delete(s.users, id)
	return nil
}
func (s *tokenHTTPAuthStore) CreateSession(_ context.Context, session auth.Session) (auth.Session, error) {
	s.sessions[session.TokenHash] = session
	return session, nil
}
func (s *tokenHTTPAuthStore) FindSessionByTokenHash(_ context.Context, hash string) (*auth.Session, error) {
	session, ok := s.sessions[hash]
	if !ok {
		return nil, nil
	}
	copy := session
	return &copy, nil
}
func (s *tokenHTTPAuthStore) RevokeSession(_ context.Context, id string, at time.Time) error {
	for hash, session := range s.sessions {
		if session.ID == id {
			session.RevokedAt = &at
			s.sessions[hash] = session
		}
	}
	return nil
}
func (s *tokenHTTPAuthStore) TouchSession(_ context.Context, id string, at time.Time) error {
	for hash, session := range s.sessions {
		if session.ID == id {
			session.LastSeenAt = &at
			s.sessions[hash] = session
		}
	}
	return nil
}
func (s *tokenHTTPAuthStore) AddAudit(context.Context, auth.AuditEvent) error { return nil }
func (s *tokenHTTPAuthStore) ResetPassword(context.Context, string, string, time.Time, auth.AuditEvent) (auth.User, error) {
	return auth.User{}, errors.New("not implemented")
}
func (s *tokenHTTPAuthStore) CompletePasswordChange(context.Context, string, string, string, time.Time, auth.AuditEvent) (auth.User, error) {
	return auth.User{}, errors.New("not implemented")
}

// tokenHTTPEnrollmentStore is intentionally only a test authority; production
// uses the PostgreSQL implementation supplied by configuration.
type tokenHTTPEnrollmentStore struct {
	records map[string]tokenHTTPEnrollmentRecord
	granted map[string]bool
}

type tokenHTTPEnrollmentRecord struct {
	id, selector, hash, owner string
	expiresAt, createdAt      time.Time
	consumedAt, revokedAt     *time.Time
}

func newTokenHTTPEnrollmentStore() *tokenHTTPEnrollmentStore {
	return &tokenHTTPEnrollmentStore{records: map[string]tokenHTTPEnrollmentRecord{}, granted: map[string]bool{}}
}
func (s *tokenHTTPEnrollmentStore) CreateEnrollmentToken(_ context.Context, id, selector, hash, _ string, ownerID string, createdAt, expiresAt time.Time) (domain.TopologyRole, error) {
	s.records[id] = tokenHTTPEnrollmentRecord{id: id, selector: selector, hash: hash, owner: ownerID, createdAt: createdAt, expiresAt: expiresAt}
	return domain.TopologyRolePrimary, nil
}
func (s *tokenHTTPEnrollmentStore) CheckEnrollmentToken(_ context.Context, selector, hash string, now time.Time, consume bool) error {
	for id, record := range s.records {
		if record.selector != selector || record.hash != hash || !record.expiresAt.After(now) || record.revokedAt != nil {
			continue
		}
		if record.consumedAt != nil {
			return enrollment.ErrEnrollmentDenied
		}
		if consume {
			record.consumedAt = &now
			s.records[id] = record
		}
		return nil
	}
	return enrollment.ErrEnrollmentDenied
}
func (s *tokenHTTPEnrollmentStore) RevokeEnrollmentToken(_ context.Context, id, ownerID string, now time.Time) error {
	record, ok := s.records[id]
	if ok && record.owner == ownerID && record.revokedAt == nil {
		record.revokedAt = &now
		s.records[id] = record
	}
	return nil
}

func (s *tokenHTTPEnrollmentStore) DeleteEnrollmentToken(_ context.Context, id, ownerID string, now time.Time) error {
	record, ok := s.records[id]
	if !ok || record.owner != ownerID {
		return enrollment.ErrTokenNotFound
	}
	switch {
	case s.granted[id]:
		return enrollment.ErrTokenInUse
	case record.revokedAt != nil:
	case record.consumedAt != nil:
		return enrollment.ErrTokenInUse
	case record.expiresAt.After(now):
		return enrollment.ErrTokenActive
	}
	delete(s.records, id)
	return nil
}

func TestEnrollmentTokenRoutesRequireOwnerAndConfirmation(t *testing.T) {
	authStore := newTokenHTTPAuthStore()
	authService := auth.NewService(authStore, auth.ServiceOptions{SessionTTL: time.Hour})
	tokenStore := newTokenHTTPEnrollmentStore()
	authority := httpserver.NewEnrollmentTokenAuthority(tokenStore, nil, httpserver.EnrollmentTokenAuthorityOptions{
		List: func(_ context.Context, ownerID string) ([]httpserver.EnrollmentTokenRecord, error) {
			result := make([]httpserver.EnrollmentTokenRecord, 0)
			for _, record := range tokenStore.records {
				if record.owner == ownerID {
					result = append(result, httpserver.EnrollmentTokenRecord{ID: record.id, Selector: record.selector, CreatedAt: record.createdAt, ExpiresAt: record.expiresAt, ConsumedAt: record.consumedAt, RevokedAt: record.revokedAt})
				}
			}
			return result, nil
		},
	})
	srv := httpserver.New(config.Config{UIDist: t.TempDir(), SessionCookieName: "proxycore_session", SessionTTL: time.Hour}, nil,
		httpserver.WithAuthService(authService), httpserver.WithEnrollmentTokenAuthority(authority))

	unauthenticated := httptest.NewRecorder()
	srv.Handler().ServeHTTP(unauthenticated, httptest.NewRequest(http.MethodPost, httpserver.EnrollmentTokensPath, strings.NewReader(`{}`)))
	if unauthenticated.Code != http.StatusUnauthorized || unauthenticated.Header().Get("Location") != "" {
		t.Fatalf("unauthenticated status/location=%d %q", unauthenticated.Code, unauthenticated.Header().Get("Location"))
	}

	owner, err := authService.Bootstrap(context.Background(), "owner", "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	session, err := authService.Login(context.Background(), owner.Username, "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: "proxycore_session", Value: session.Token}

	create := httptest.NewRecorder()
	createRequest := httptest.NewRequest(http.MethodPost, httpserver.EnrollmentTokensPath, strings.NewReader(`{}`))
	createRequest.AddCookie(cookie)
	srv.Handler().ServeHTTP(create, createRequest)
	if create.Code != http.StatusCreated || create.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("create status/headers=%d %q body=%s", create.Code, create.Header().Get("Cache-Control"), create.Body.String())
	}
	var created httpserver.EnrollmentTokenCreation
	if err := json.Unmarshal(create.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Token == "" || created.Selector == "" || created.ID == "" {
		t.Fatalf("creation projection=%+v", created)
	}
	unknown := httptest.NewRecorder()
	unknownRequest := httptest.NewRequest(http.MethodPost, httpserver.EnrollmentTokensPath, strings.NewReader(`{"unexpected":true}`))
	unknownRequest.AddCookie(cookie)
	srv.Handler().ServeHTTP(unknown, unknownRequest)
	if unknown.Code != http.StatusBadRequest || strings.Contains(unknown.Body.String(), created.Token) {
		t.Fatalf("unknown-field status/body=%d %s", unknown.Code, unknown.Body.String())
	}

	list := httptest.NewRecorder()
	listRequest := httptest.NewRequest(http.MethodGet, httpserver.EnrollmentTokensPath, nil)
	listRequest.AddCookie(cookie)
	srv.Handler().ServeHTTP(list, listRequest)
	if list.Code != http.StatusOK || strings.Contains(list.Body.String(), created.Token) || strings.Contains(list.Body.String(), `"token"`) {
		t.Fatalf("list leaked token: status=%d body=%s", list.Code, list.Body.String())
	}

	revoke := httptest.NewRecorder()
	revokeRequest := httptest.NewRequest(http.MethodPost, httpserver.EnrollmentTokensPath+"/"+created.ID+"/revoke", nil)
	revokeRequest.AddCookie(cookie)
	srv.Handler().ServeHTTP(revoke, revokeRequest)
	if revoke.Code != http.StatusConflict || !strings.Contains(revoke.Body.String(), "confirmation") {
		t.Fatalf("unconfirmed revoke status/body=%d %s", revoke.Code, revoke.Body.String())
	}

	confirmed := httptest.NewRecorder()
	confirmedRequest := httptest.NewRequest(http.MethodPost, httpserver.EnrollmentTokensPath+"/"+created.ID+"/revoke?confirm=replace-and-revoke", nil)
	confirmedRequest.AddCookie(cookie)
	srv.Handler().ServeHTTP(confirmed, confirmedRequest)
	if confirmed.Code != http.StatusNoContent {
		t.Fatalf("confirmed revoke status/body=%d %s", confirmed.Code, confirmed.Body.String())
	}
}

func TestEnrollmentTokenDeleteRoute(t *testing.T) {
	authStore := newTokenHTTPAuthStore()
	authService := auth.NewService(authStore, auth.ServiceOptions{SessionTTL: time.Hour})
	tokenStore := newTokenHTTPEnrollmentStore()
	authority := httpserver.NewEnrollmentTokenAuthority(tokenStore, nil, httpserver.EnrollmentTokenAuthorityOptions{})
	srv := httpserver.New(config.Config{UIDist: t.TempDir(), SessionCookieName: "proxycore_session", SessionTTL: time.Hour}, nil,
		httpserver.WithAuthService(authService), httpserver.WithEnrollmentTokenAuthority(authority))

	del := func(id string, cookie *http.Cookie) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodDelete, httpserver.EnrollmentTokensPath+"/"+id, nil)
		if cookie != nil {
			req.AddCookie(cookie)
		}
		srv.Handler().ServeHTTP(rec, req)
		return rec
	}
	if rec := del(uuid.NewString(), nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d", rec.Code)
	}
	owner, err := authService.Bootstrap(context.Background(), "owner", "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	session, err := authService.Login(context.Background(), owner.Username, "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: "proxycore_session", Value: session.Token}

	now := time.Now().UTC()
	past := now.Add(-time.Hour)
	future := now.Add(time.Hour)
	seed := func(expires time.Time, consumed, revoked bool, granted bool, ownerID string) string {
		id := uuid.NewString()
		record := tokenHTTPEnrollmentRecord{id: id, selector: id, owner: ownerID, createdAt: past, expiresAt: expires}
		if consumed {
			record.consumedAt = &past
		}
		if revoked {
			record.revokedAt = &past
		}
		tokenStore.records[id] = record
		tokenStore.granted[id] = granted
		return id
	}
	revokedID := seed(future, false, true, false, owner.ID)
	expiredID := seed(past, false, false, false, owner.ID)
	activeID := seed(future, false, false, false, owner.ID)
	grantedID := seed(past, true, true, true, owner.ID)
	foreignID := seed(past, false, true, false, "someone-else")

	for _, tc := range []struct {
		name, id string
		status   int
		code     string
	}{
		{"revoked", revokedID, http.StatusNoContent, ""},
		{"expired", expiredID, http.StatusNoContent, ""},
		{"active", activeID, http.StatusConflict, "TOKEN_ACTIVE"},
		{"granted", grantedID, http.StatusConflict, "TOKEN_IN_USE"},
		{"foreign owner", foreignID, http.StatusNotFound, ""},
		{"unknown", uuid.NewString(), http.StatusNotFound, ""},
		{"second delete", revokedID, http.StatusNotFound, ""},
		{"invalid id", "not-a-uuid", http.StatusNotFound, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := del(tc.id, cookie)
			if rec.Code != tc.status || rec.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("status=%d cache=%q body=%s", rec.Code, rec.Header().Get("Cache-Control"), rec.Body.String())
			}
			if tc.code != "" && topologyReadOnlyCode(t, rec.Body.Bytes()) != tc.code {
				t.Fatalf("code body=%s", rec.Body.String())
			}
			if tc.status == http.StatusNoContent && rec.Body.Len() != 0 {
				t.Fatalf("204 carried a body: %s", rec.Body.String())
			}
		})
	}
	if _, ok := tokenStore.records[activeID]; !ok {
		t.Fatal("active token must survive")
	}
	if _, ok := tokenStore.records[grantedID]; !ok {
		t.Fatal("granted token must survive")
	}
}

var _ auth.Store = (*tokenHTTPAuthStore)(nil)
var _ enrollment.Store = (*tokenHTTPEnrollmentStore)(nil)
