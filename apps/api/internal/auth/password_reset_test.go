package auth

import (
	"context"
	"strings"
	"testing"
	"time"
)

type resetTestStore struct {
	users    map[string]User
	sessions map[string]Session
	audits   []AuditEvent
}

func (s *resetTestStore) ListUsers(context.Context) ([]User, error) { return nil, nil }
func (s *resetTestStore) FindUserByUsername(_ context.Context, username string) (*User, error) {
	for _, user := range s.users { if user.Username == username { u := user; return &u, nil } }
	return nil, nil
}
func (s *resetTestStore) FindUserByID(_ context.Context, id string) (*User, error) { u, ok := s.users[id]; if !ok { return nil, nil }; return &u, nil }
func (s *resetTestStore) CreateUser(_ context.Context, user User) (User, error) { s.users[user.ID] = user; return user, nil }
func (s *resetTestStore) UpdateUser(_ context.Context, id string, patch UserPatch) (User, error) { return User{}, nil }
func (s *resetTestStore) DeleteUser(context.Context, string) error { return nil }
func (s *resetTestStore) CreateSession(_ context.Context, session Session) (Session, error) { s.sessions[session.ID] = session; return session, nil }
func (s *resetTestStore) FindSessionByTokenHash(_ context.Context, hash string) (*Session, error) { for _, v := range s.sessions { if v.TokenHash == hash && v.RevokedAt == nil { x := v; return &x, nil } }; return nil, nil }
func (s *resetTestStore) RevokeSession(_ context.Context, id string, at time.Time) error { v := s.sessions[id]; v.RevokedAt = &at; s.sessions[id] = v; return nil }
func (s *resetTestStore) TouchSession(context.Context, string, time.Time) error { return nil }
func (s *resetTestStore) AddAudit(_ context.Context, event AuditEvent) error { s.audits = append(s.audits, event); return nil }
func (s *resetTestStore) ResetPassword(_ context.Context, username, hash string, at time.Time, event AuditEvent) (User, error) {
	for id, user := range s.users {
		if user.Username != username { continue }
		if !user.Active { return User{}, ErrUnavailableUser }
		user.PasswordHash, user.PasswordChangeRequired, user.UpdatedAt = hash, true, at
		s.users[id] = user
		for sid, session := range s.sessions { if session.UserID == id && session.RevokedAt == nil { session.RevokedAt = &at; s.sessions[sid] = session } }
		s.audits = append(s.audits, event)
		return user, nil
	}
	return User{}, ErrUserNotFound
}
func (s *resetTestStore) CompletePasswordChange(_ context.Context, userID, currentSessionID, hash string, at time.Time, event AuditEvent) (User, error) {
	user := s.users[userID]; user.PasswordHash, user.PasswordChangeRequired, user.UpdatedAt = hash, false, at; s.users[userID] = user
	for sid, session := range s.sessions { if session.UserID == userID && sid != currentSessionID && session.RevokedAt == nil { session.RevokedAt = &at; s.sessions[sid] = session } }
	s.audits = append(s.audits, event)
	return user, nil
}

func TestGenerateTemporaryPasswordIsStrongAndRandom(t *testing.T) {
	first, err := GenerateTemporaryPassword()
	if err != nil { t.Fatal(err) }
	second, err := GenerateTemporaryPassword()
	if err != nil { t.Fatal(err) }
	if len(first) < 20 || first == second || strings.ContainsAny(first, "\r\n") { t.Fatalf("unsafe temporary passwords %q %q", first, second) }
	if _, err := HashPassword(first); err != nil { t.Fatalf("temporary password invalid: %v", err) }
}

func TestResetPasswordNormalizesRevokesAndRequiresChange(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	oldHash, _ := HashPassword("old-password")
	store := &resetTestStore{users: map[string]User{"u1": {ID: "u1", Username: "owner", PasswordHash: oldHash, Active: true}}, sessions: map[string]Session{"s1": {ID: "s1", UserID: "u1"}, "s2": {ID: "s2", UserID: "u1"}}}
	svc := NewService(store, ServiceOptions{Now: func() time.Time { return now }})
	temporary, err := svc.ResetPassword(context.Background(), " OWNER ")
	if err != nil { t.Fatal(err) }
	user := store.users["u1"]
	if !user.PasswordChangeRequired || !VerifyPassword(temporary, user.PasswordHash) { t.Fatal("reset hash/flag not persisted") }
	for id, session := range store.sessions { if session.RevokedAt == nil { t.Fatalf("session %s not revoked", id) } }
	if len(store.audits) != 1 || store.audits[0].Action != "password.reset" || store.audits[0].AfterValue != nil { t.Fatalf("unsafe audit: %+v", store.audits) }
}

func TestResetPasswordRejectsMissingAndInactiveUsers(t *testing.T) {
	store := &resetTestStore{users: map[string]User{"disabled": {ID: "disabled", Username: "disabled", Active: false}}, sessions: map[string]Session{}}
	svc := NewService(store, ServiceOptions{})
	if password, err := svc.ResetPassword(context.Background(), "missing"); err != ErrUserNotFound || password != "" { t.Fatalf("missing password=%q err=%v", password, err) }
	if password, err := svc.ResetPassword(context.Background(), "disabled"); err != ErrUnavailableUser || password != "" { t.Fatalf("inactive password=%q err=%v", password, err) }
	if len(store.audits) != 0 { t.Fatalf("failed reset wrote audit: %+v", store.audits) }
}

func TestChangePasswordKeepsCurrentSessionAndRevokesOthers(t *testing.T) {
	now := time.Now().UTC()
	oldHash, _ := HashPassword("temporary-password")
	token := "current-token"
	store := &resetTestStore{users: map[string]User{"u1": {ID: "u1", Username: "owner", PasswordHash: oldHash, Active: true, PasswordChangeRequired: true}}, sessions: map[string]Session{
		"current": {ID: "current", UserID: "u1", TokenHash: HashOpaqueToken(token), ExpiresAt: now.Add(time.Hour)},
		"other": {ID: "other", UserID: "u1", ExpiresAt: now.Add(time.Hour)},
	}}
	svc := NewService(store, ServiceOptions{Now: func() time.Time { return now }})
	user, err := svc.ChangePassword(context.Background(), token, "new-secure-password")
	if err != nil { t.Fatal(err) }
	if user.PasswordChangeRequired || !VerifyPassword("new-secure-password", store.users["u1"].PasswordHash) { t.Fatal("password change not completed") }
	if store.sessions["current"].RevokedAt != nil || store.sessions["other"].RevokedAt == nil { t.Fatal("session policy incorrect") }
}
