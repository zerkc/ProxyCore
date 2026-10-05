package auth

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// Pause at the capability boundary, after expensive service verification.
type sqliteCredentialBarrier struct {
	*SQLiteStore
	ready, release chan struct{}
}

func (s sqliteCredentialBarrier) CreateSessionForCredential(ctx context.Context, session Session, hash string) (Session, User, error) {
	close(s.ready)
	select {
	case <-s.release:
		return s.SQLiteStore.CreateSessionForCredential(ctx, session, hash)
	case <-ctx.Done():
		return Session{}, User{}, ctx.Err()
	}
}

func TestSQLiteLoginCredentialBoundary(t *testing.T) {
	for _, mode := range []string{"reset", "change", "disable", "delete", "attributes"} {
		t.Run(mode, func(t *testing.T) {
			s, db, _ := sqliteFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			opts := ServiceOptions{Now: func() time.Time { return sqliteTestAt }, SessionTTL: time.Hour}
			svc := NewService(s, opts)
			hash, err := HashPassword("Original-password-123!")
			if err != nil {
				t.Fatal(err)
			}
			u, err := s.CreateUser(ctx, User{Username: "operator", PasswordHash: hash, Role: RoleOperator, Active: true})
			if err != nil {
				t.Fatal(err)
			}
			var current AuthSession
			if mode == "change" {
				current, err = svc.Login(ctx, u.Username, "Original-password-123!")
				if err != nil {
					t.Fatal(err)
				}
			}
			barrier := sqliteCredentialBarrier{s, make(chan struct{}), make(chan struct{})}
			type result struct {
				login AuthSession
				err   error
			}
			done := make(chan result, 1)
			go func() {
				login, err := NewService(barrier, opts).Login(ctx, " OPERATOR ", "Original-password-123!")
				done <- result{login, err}
			}()
			select {
			case <-barrier.ready:
			case <-ctx.Done():
				t.Fatal("verification boundary deadline")
			}
			switch mode {
			case "reset":
				_, err = svc.ResetPassword(ctx, u.Username)
			case "change":
				_, err = svc.ChangePassword(ctx, current.Token, "Replacement-password-123!")
			case "disable":
				active := false
				_, err = s.UpdateUser(ctx, u.ID, UserPatch{Active: &active})
			case "delete":
				err = s.DeleteUser(ctx, u.ID)
			case "attributes":
				role, required := RoleOwner, true
				_, err = s.UpdateUser(ctx, u.ID, UserPatch{Role: &role, PasswordChangeRequired: &required})
			}
			if err != nil {
				t.Fatal(err)
			}
			close(barrier.release)
			var got result
			select {
			case got = <-done:
			case <-ctx.Done():
				t.Fatal("login completion deadline")
			}
			wantSessions, wantFailures := 0, 1
			if mode == "change" {
				wantSessions = 1 // Only the pre-existing password-change session.
			}
			if mode == "attributes" {
				wantSessions, wantFailures = 1, 0
				if got.err != nil || got.login.User.Role != RoleOwner || !got.login.User.PasswordChangeRequired || !got.login.ExpiresAt.Equal(sqliteTestAt.Add(time.Hour)) {
					t.Fatal("fresh attributes/expiry not returned", got.err)
				}
				if _, err := svc.ResetPassword(ctx, u.Username); err != nil {
					t.Fatal(err)
				}
				if _, err := svc.Authenticate(ctx, got.login.Token); !errors.Is(err, ErrInvalidSession) {
					t.Fatal("login won but reset failed to revoke", err)
				}
			} else if !errors.Is(got.err, ErrInvalidCredential) || got.login.Token != "" {
				t.Fatal("stale credential accepted", got.err)
			}
			var sessions, failures int
			if err := db.QueryRow("SELECT count(*) FROM sessions").Scan(&sessions); err != nil || sessions != wantSessions {
				t.Fatalf("sessions=%d want=%d err=%v", sessions, wantSessions, err)
			}
			if err := db.QueryRow(`SELECT count(*) FROM audit_events WHERE action='login' AND result='failure' AND actor_user_id IS NULL AND resource_id=? AND after_value='{"username":"operator"}'`, u.ID).Scan(&failures); err != nil || failures != wantFailures {
				t.Fatalf("credential failure audits=%d want=%d err=%v", failures, wantFailures, err)
			}
		})
	}
}

func TestSQLiteCredentialMismatchAndFallback(t *testing.T) {
	s, db, _ := sqliteFixture(t)
	ctx := context.Background()
	u := sqliteUser(t, s, "operator", RoleOperator)
	if _, _, err := s.CreateSessionForCredential(ctx, Session{UserID: u.ID, ExpiresAt: sqliteTestAt.Add(time.Hour)}, "wrong"); !errors.Is(err, ErrInvalidCredential) {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRow("SELECT count(*) FROM sessions").Scan(&count); err != nil || count != 0 {
		t.Fatal("mismatch inserted session", err)
	}
	hash, err := HashPassword("Original-password-123!")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateUser(ctx, u.ID, UserPatch{PasswordHash: &hash}); err != nil {
		t.Fatal(err)
	}
	for _, createErr := range []error{nil, ErrInvalidCredential, errors.New("session unavailable")} {
		fallback := &sqliteLoginFallback{sqliteFallback: sqliteFallback{Store: s, auditErr: errors.New("audit unavailable")}, sessionErr: createErr}
		login, err := NewService(fallback, ServiceOptions{}).Login(ctx, u.Username, "Original-password-123!")
		want := "[session]"
		if createErr == nil {
			want = "[session success]"
			if login.Token == "" {
				t.Fatal("fallback returned no token")
			}
		}
		if !errors.Is(err, createErr) || fmt.Sprint(fallback.calls) != want {
			t.Fatal("fallback behavior changed", err, fallback.calls)
		}
	}
}

type sqliteLoginFallback struct {
	sqliteFallback
	sessionErr error
}

func (s *sqliteLoginFallback) CreateSession(ctx context.Context, session Session) (Session, error) {
	s.calls = append(s.calls, "session")
	if s.sessionErr != nil {
		return Session{}, s.sessionErr
	}
	return s.Store.CreateSession(ctx, session)
}
