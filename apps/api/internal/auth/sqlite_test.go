package auth

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

func sqliteUser(t *testing.T, s *SQLiteStore, name string, role Role) User {
	t.Helper()
	u, err := s.CreateUser(context.Background(), User{Username: name, PasswordHash: "hash", Role: role, Active: true})
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func sqliteSession(t *testing.T, s *SQLiteStore, u User, token string) Session {
	t.Helper()
	v, err := s.CreateSession(context.Background(), Session{UserID: u.ID, TokenHash: token, ExpiresAt: sqliteTestAt.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func sqliteEvent() AuditEvent {
	return AuditEvent{Action: "test", ResourceType: "user", Correlation: "test", Result: "success", CreatedAt: sqliteTestAt.Add(-time.Hour)}
}

// Mask the optional capability to exercise the unchanged PG-compatible path.
type sqliteFallback struct {
	Store
	calls     []string
	auditErr  error
	createErr error
}

func (s *sqliteFallback) ListUsers(ctx context.Context) ([]User, error) {
	s.calls = append(s.calls, "list")
	return s.Store.ListUsers(ctx)
}
func (s *sqliteFallback) CreateUser(ctx context.Context, u User) (User, error) {
	s.calls = append(s.calls, "create")
	if s.createErr != nil {
		return User{}, s.createErr
	}
	if (u.Role != RoleOwner && u.Role != RoleOperator) || !u.Active || !VerifyPassword("Original-password-123!", u.PasswordHash) {
		return User{}, errors.New("credentials not built before create")
	}
	return s.Store.CreateUser(ctx, u)
}
func (s *sqliteFallback) AddAudit(ctx context.Context, e AuditEvent) error {
	s.calls = append(s.calls, e.Result)
	if s.auditErr != nil {
		return s.auditErr
	}
	return s.Store.AddAudit(ctx, e)
}

// Synchronize prechecks to force a service-level capability race loser.
type sqliteBootstrapRace struct {
	*SQLiteStore
	ready *sync.WaitGroup
}

func (s sqliteBootstrapRace) ListUsers(ctx context.Context) ([]User, error) {
	u, err := s.SQLiteStore.ListUsers(ctx)
	s.ready.Done()
	s.ready.Wait()
	return u, err
}

func TestSQLiteBootstrapAndFallback(t *testing.T) {
	ctx := context.Background()
	store, db, _ := sqliteFixture(t)
	var ready sync.WaitGroup
	ready.Add(2)
	raced := sqliteBootstrapRace{store, &ready}
	results := make(chan error, 2)
	for _, name := range []string{"first", "second"} {
		go func(name string) {
			_, err := NewService(raced, ServiceOptions{}).Bootstrap(ctx, name, "Original-password-123!")
			results <- err
		}(name)
	}
	wins, losses := 0, 0
	for range 2 {
		err := <-results
		if err == nil {
			wins++
		} else if errors.Is(err, ErrBootstrapComplete) {
			losses++
		} else {
			t.Fatal(err)
		}
	}
	if wins != 1 || losses != 1 {
		t.Fatalf("winners=%d losers=%d", wins, losses)
	}
	var denied int
	if err := db.QueryRow("SELECT count(*) FROM audit_events WHERE action='bootstrap' AND result='denied'").Scan(&denied); err != nil || denied != 1 {
		t.Fatalf("denied=%d %v", denied, err)
	}
	fallbackStore, _, _ := sqliteFixture(t)
	fallback := &sqliteFallback{Store: fallbackStore}
	svc := NewService(fallback, ServiceOptions{})
	if _, err := svc.Bootstrap(ctx, "owner", "bad"); err == nil {
		t.Fatal("weak password accepted")
	}
	if fmt.Sprint(fallback.calls) != "[list]" {
		t.Fatal(fallback.calls)
	}
	fallback.calls = nil
	fallback.auditErr = errors.New("audit unavailable")
	owner, err := svc.Bootstrap(ctx, "owner", "Original-password-123!")
	if err != nil || owner.Role != RoleOwner {
		t.Fatalf("fallback bootstrap: %+v %v", owner, err)
	}
	if fmt.Sprint(fallback.calls) != "[list create success]" {
		t.Fatal(fallback.calls)
	}
	if _, err := svc.Bootstrap(ctx, "other", "short"); !errors.Is(err, ErrBootstrapComplete) {
		t.Fatal(err)
	}
	if _, err := svc.CreateUser(ctx, owner, "operator", "Original-password-123!", RoleOperator); err != nil {
		t.Fatal("ordinary user audit should be best effort", err)
	}
	login, err := svc.Login(ctx, owner.Username, "Original-password-123!")
	if err != nil {
		t.Fatal("login audit should be best effort", err)
	}
	if err := svc.Logout(ctx, login.Token); err != nil {
		t.Fatal("logout audit should be best effort", err)
	}
	if _, err := svc.Authenticate(ctx, login.Token); !errors.Is(err, ErrInvalidSession) {
		t.Fatal(err)
	}
	errorStore, _, _ := sqliteFixture(t)
	errorFallback := &sqliteFallback{Store: errorStore, createErr: ErrBootstrapComplete}
	if _, err := NewService(errorFallback, ServiceOptions{}).Bootstrap(ctx, "owner", "Original-password-123!"); !errors.Is(err, ErrBootstrapComplete) {
		t.Fatal(err)
	}
	if fmt.Sprint(errorFallback.calls) != "[list create]" {
		t.Fatal("non-capability creation errors must not acquire new audit behavior", errorFallback.calls)
	}
	activeStore, _, _ := sqliteFixture(t)
	sqliteUser(t, activeStore, "operator", RoleOperator)
	if _, err := activeStore.BootstrapUser(ctx, User{Username: "owner"}); !errors.Is(err, ErrBootstrapComplete) {
		t.Fatal(err)
	}
}

func TestSQLiteOwnerMutationRaces(t *testing.T) {
	for _, mode := range []string{"demote", "delete", "deactivate", "mixed"} {
		t.Run(mode, func(t *testing.T) {
			s, _, _ := sqliteFixture(t)
			owners := []User{sqliteUser(t, s, "one", RoleOwner), sqliteUser(t, s, "two", RoleOwner)}
			results := make(chan error, 2)
			for i, u := range owners {
				go func(i int, u User) {
					if mode == "delete" || (mode == "mixed" && i == 0) {
						results <- s.DeleteUser(context.Background(), u.ID)
						return
					}
					role, active := RoleOperator, false
					patch := UserPatch{Role: &role}
					if mode == "deactivate" {
						patch = UserPatch{Active: &active}
					}
					_, err := s.UpdateUser(context.Background(), u.ID, patch)
					results <- err
				}(i, u)
			}
			wins := 0
			for range 2 {
				err := <-results
				if err == nil {
					wins++
				} else if err.Error() != "Cannot delete the last Owner" && err.Error() != "Cannot remove or demote the last Owner" {
					t.Fatal(err)
				}
			}
			if wins != 1 {
				t.Fatalf("wins=%d", wins)
			}
			users, err := s.ListUsers(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			count := 0
			for _, u := range users {
				if u.Active && u.Role == RoleOwner {
					count++
				}
			}
			if count != 1 {
				t.Fatalf("owners=%d", count)
			}
		})
	}
}

func TestSQLitePasswordAtomicityAndSessionValidation(t *testing.T) {
	s, db, _ := sqliteFixture(t)
	ctx := context.Background()
	u := sqliteUser(t, s, "owner", RoleOwner)
	other := sqliteUser(t, s, "other", RoleOperator)
	current := sqliteSession(t, s, u, "current")
	sibling := sqliteSession(t, s, u, "sibling")
	badAudit := sqliteEvent()
	badAudit.Action = "invalid\x00action" // Real CHECK failure, no authority/schema alteration.
	for _, reset := range []bool{true, false} {
		var err error
		if reset {
			_, err = s.ResetPassword(ctx, u.Username, "new", sqliteTestAt, badAudit)
		} else {
			required := true
			if _, err := s.UpdateUser(ctx, u.ID, UserPatch{PasswordChangeRequired: &required}); err != nil {
				t.Fatal(err)
			}
			_, err = s.CompletePasswordChange(ctx, u.ID, current.ID, "new", sqliteTestAt, badAudit)
		}
		if sqliteConstraintCode(err) != 275 {
			t.Fatalf("audit constraint: %v", err)
		}
		persisted, err := s.FindUserByID(ctx, u.ID)
		if err != nil || persisted.PasswordHash != "hash" || persisted.PasswordChangeRequired != !reset {
			t.Fatalf("rollback: %+v %v", persisted, err)
		}
		for _, token := range []string{"current", "sibling"} {
			v, err := s.FindSessionByTokenHash(ctx, token)
			if err != nil || v == nil {
				t.Fatalf("session rollback %s %v", token, err)
			}
		}
	}
	for _, tc := range []struct {
		user, session string
		at            time.Time
	}{
		{other.ID, current.ID, sqliteTestAt}, {u.ID, "missing", sqliteTestAt}, {u.ID, current.ID, current.ExpiresAt},
	} {
		if _, err := s.CompletePasswordChange(ctx, tc.user, tc.session, "new", tc.at, sqliteEvent()); !errors.Is(err, ErrInvalidSession) {
			t.Fatal(err)
		}
	}
	if err := s.RevokeSession(ctx, sibling.ID, sqliteTestAt); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeSession(ctx, sibling.ID, sqliteTestAt.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.TouchSession(ctx, sibling.ID, sqliteTestAt.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	var revoked string
	var seen sql.NullString
	if err := db.QueryRow("SELECT revoked_at,last_seen_at FROM sessions WHERE id=?", sibling.ID).Scan(&revoked, &seen); err != nil {
		t.Fatal(err)
	}
	want, _ := sqliteTime(sqliteTestAt)
	if revoked != want || seen.Valid {
		t.Fatal("revoked session changed")
	}
	if _, err := s.CompletePasswordChange(ctx, u.ID, sibling.ID, "new", sqliteTestAt, sqliteEvent()); !errors.Is(err, ErrInvalidSession) {
		t.Fatal(err)
	}
	otherSession := sqliteSession(t, s, other, HashOpaqueToken("inactive-token"))
	inactive := false
	if _, err := s.UpdateUser(ctx, other.ID, UserPatch{Active: &inactive}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CompletePasswordChange(ctx, other.ID, otherSession.ID, "new", sqliteTestAt, sqliteEvent()); !errors.Is(err, ErrUnavailableUser) {
		t.Fatal(err)
	}
	svc := NewService(s, ServiceOptions{Now: func() time.Time { return sqliteTestAt }})
	if _, err := svc.Authenticate(ctx, "inactive-token"); !errors.Is(err, ErrUnavailableUser) {
		t.Fatal(err)
	}
	if _, err := svc.Login(ctx, "missing", "anything"); !errors.Is(err, ErrInvalidCredential) {
		t.Fatal(err)
	}
	if _, err := s.ResetPassword(ctx, other.Username, "new", sqliteTestAt, sqliteEvent()); !errors.Is(err, ErrUnavailableUser) {
		t.Fatal(err)
	}
	if _, err := s.ResetPassword(ctx, "missing", "new", sqliteTestAt, sqliteEvent()); !errors.Is(err, ErrUserNotFound) {
		t.Fatal(err)
	}
	if _, err := NewService(s, ServiceOptions{}).Login(ctx, other.Username, "anything"); !errors.Is(err, ErrInvalidCredential) {
		t.Fatal(err)
	}
}

func TestSQLiteConstraintsCascadeAndAuditValues(t *testing.T) {
	s, db, _ := sqliteFixture(t)
	ctx := context.Background()
	u := sqliteUser(t, s, "operator", RoleOperator)
	session := sqliteSession(t, s, u, "token")
	duplicate := u
	duplicate.ID = newUUID()
	if _, err := s.CreateUser(ctx, duplicate); sqliteConstraintCode(err) != 2067 {
		t.Fatal("adapter username constraint", err)
	}
	if _, err := s.CreateSession(ctx, Session{UserID: "missing", TokenHash: "foreign", ExpiresAt: sqliteTestAt}); sqliteConstraintCode(err) != 787 {
		t.Fatal("adapter foreign key constraint", err)
	}
	for _, tc := range []struct {
		name, query string
		args        []any
		code        int
	}{
		{"unique", "UPDATE users SET username=? WHERE id=?", []any{u.Username, sqliteUser(t, s, "other", RoleOperator).ID}, 2067},
		{"primary", "UPDATE users SET id=? WHERE username='other'", []any{u.ID}, 1555},
		{"foreign", "UPDATE sessions SET user_id='missing' WHERE id=?", []any{session.ID}, 787},
		{"check", "UPDATE users SET active=2 WHERE id=?", []any{u.ID}, 275},
		{"null", "UPDATE users SET username=NULL WHERE id=?", []any{u.ID}, 1299},
		{"datatype", "UPDATE users SET username=? WHERE id=?", []any{[]byte("blob"), u.ID}, 3091},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := db.Exec(tc.query, tc.args...)
			if sqliteConstraintCode(fmt.Errorf("wrapped: %w", err)) != tc.code {
				t.Fatalf("code=%d err=%v", sqliteConstraintCode(err), err)
			}
		})
	}
	for _, after := range []any{nil, (*User)(nil), u, session} {
		e := sqliteEvent()
		e.AfterValue = after
		if err := s.AddAudit(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	var nulls, jsonNulls, beforeNulls int
	if err := db.QueryRow("SELECT sum(after_value IS NULL),sum(after_value='null'),sum(before_value IS NULL) FROM audit_events").Scan(&nulls, &jsonNulls, &beforeNulls); err != nil {
		t.Fatal(err)
	}
	if nulls != 1 || jsonNulls != 1 || beforeNulls != 4 {
		t.Fatal(nulls, jsonNulls, beforeNulls)
	}
	for _, value := range []any{u, session, AuthSession{Token: "raw-token", User: u}} {
		b, err := json.Marshal(value)
		if err != nil || strings.Contains(string(b), "hash") || strings.Contains(string(b), "raw-token") || strings.Contains(string(b), "tokenHash") {
			t.Fatalf("redaction: %s %v", b, err)
		}
	}
	if err := s.DeleteUser(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	if found, err := s.FindSessionByTokenHash(ctx, "token"); err != nil || found != nil {
		t.Fatal("cascade", found, err)
	}
	actor := sqliteUser(t, s, "actor", RoleOperator)
	e := sqliteEvent()
	e.ActorUserID = &actor.ID
	if err := s.AddAudit(ctx, e); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteUser(ctx, actor.ID); sqliteConstraintCode(err) != 787 {
		t.Fatal("actor history", err)
	}
}

func TestSQLiteOperationalValues(t *testing.T) {
	for _, at := range []time.Time{time.Unix(-1, 123456789), time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC)} {
		key, err := sqliteTime(at)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := sqliteDecodeTime(sql.NullString{String: key, Valid: true})
		if err != nil || !decoded.Equal(at) {
			t.Fatalf("roundtrip %v %v", decoded, err)
		}
	}
	for _, seconds := range []int64{math.MinInt64, math.MaxInt64, sqliteMinSecond - 1, sqliteMaxSecond + 1} {
		key, _ := sqlitedb.EncodeTimestamp(sqlitedb.FiniteTimestamp{Seconds: seconds})
		if _, err := sqliteDecodeTime(sql.NullString{String: *key, Valid: true}); err == nil {
			t.Fatal("extreme accepted")
		}
	}
	for _, key := range []sql.NullString{{}, {String: "0", Valid: true}, {String: "2", Valid: true}} {
		if _, err := sqliteDecodeTime(key); err == nil {
			t.Fatal("nonfinite required value accepted")
		}
	}
	if v, err := sqliteNullableTime(sql.NullString{}); err != nil || v != nil {
		t.Fatal(v, err)
	}
	for _, year := range []int{0, 10000} {
		if _, err := sqliteTime(time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC)); err == nil {
			t.Fatal("year accepted")
		}
	}
	for _, n := range []int64{-1, 2} {
		if _, err := sqliteReadBool(n); err == nil {
			t.Fatal("boolean accepted")
		}
	}
	s, db, _ := sqliteFixture(t)
	u := sqliteUser(t, s, "owner", RoleOwner)
	if !u.CreatedAt.Equal(sqliteTestAt) || !u.UpdatedAt.Equal(sqliteTestAt) || u.ID == "" {
		t.Fatal("writer defaults")
	}
	if _, err := s.CreateSession(context.Background(), Session{UserID: u.ID}); err == nil {
		t.Fatal("missing expiry")
	}
	if _, err := db.Exec("UPDATE users SET created_at='2' WHERE id=?", u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FindUserByID(context.Background(), u.ID); err == nil {
		t.Fatal("stored infinity accepted")
	}
}

func TestSQLiteCRUDExplicitValues(t *testing.T) {
	s, db, _ := sqliteFixture(t)
	ctx := context.Background()
	at := time.Unix(-1, 987654321).UTC()
	u := User{ID: newUUID(), Username: "explicit", PasswordHash: "original", Role: RoleOperator,
		Active: true, PasswordChangeRequired: true, CreatedAt: at, UpdatedAt: at.Add(time.Second)}
	created, err := s.CreateUser(ctx, u)
	if err != nil || created.ID != u.ID {
		t.Fatalf("create: %+v %v", created, err)
	}
	persisted, err := s.FindUserByUsername(ctx, u.Username)
	if err != nil || persisted == nil || *persisted != u {
		t.Fatalf("explicit values: %+v %v", persisted, err)
	}
	role, active, required, hash := RoleOwner, false, false, "replacement"
	updated, err := s.UpdateUser(ctx, u.ID, UserPatch{Role: &role, Active: &active, PasswordHash: &hash, PasswordChangeRequired: &required})
	if err != nil || updated.Role != role || updated.Active || updated.PasswordChangeRequired || updated.PasswordHash != hash || !updated.UpdatedAt.Equal(sqliteTestAt) || !updated.CreatedAt.Equal(at) {
		t.Fatalf("full patch: %+v %v", updated, err)
	}
	persisted, err = s.FindUserByID(ctx, u.ID)
	if err != nil || persisted == nil || *persisted != updated {
		t.Fatalf("patch persistence: %+v %v", persisted, err)
	}
	if _, err := s.UpdateUser(ctx, "missing", UserPatch{}); !errors.Is(err, ErrUserNotFound) {
		t.Fatal(err)
	}
	if found, err := s.FindUserByID(ctx, "missing"); err != nil || found != nil {
		t.Fatal(found, err)
	}
	if err := s.DeleteUser(ctx, "missing"); err != nil {
		t.Fatal(err)
	}
	session := Session{ID: newUUID(), UserID: u.ID, TokenHash: "explicit-token", ExpiresAt: at.Add(time.Hour), CreatedAt: at, LastSeenAt: &at}
	if _, err := s.CreateSession(ctx, session); err != nil {
		t.Fatal(err)
	}
	found, err := s.FindSessionByTokenHash(ctx, session.TokenHash)
	if err != nil || found == nil || found.ID != session.ID || !found.CreatedAt.Equal(at) || !found.ExpiresAt.Equal(session.ExpiresAt) || found.LastSeenAt == nil || !found.LastSeenAt.Equal(at) || found.RevokedAt != nil {
		t.Fatalf("explicit session: %+v %v", found, err)
	}
	if err := s.TouchSession(ctx, session.ID, at.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	found, err = s.FindSessionByTokenHash(ctx, session.TokenHash)
	if err != nil || found == nil || found.LastSeenAt == nil || !found.LastSeenAt.Equal(at.Add(time.Second)) {
		t.Fatalf("explicit touch: %+v %v", found, err)
	}
	e := sqliteEvent()
	e.ID = newUUID()
	if err := s.AddAudit(ctx, e); err != nil {
		t.Fatal(err)
	}
	var stamp string
	if err := db.QueryRow("SELECT created_at FROM audit_events WHERE id=?", e.ID).Scan(&stamp); err != nil {
		t.Fatal(err)
	}
	want, _ := sqliteTime(e.CreatedAt)
	if stamp != want {
		t.Fatal("audit clock was overwritten")
	}
	if _, err := s.ResetPassword(ctx, u.Username, "new", at, sqliteEvent()); !errors.Is(err, ErrUnavailableUser) {
		t.Fatal(err)
	}
}

func TestSQLitePasswordExplicitClockAndAudit(t *testing.T) {
	s, db, _ := sqliteFixture(t)
	ctx := context.Background()
	u := sqliteUser(t, s, "owner", RoleOwner)
	session := sqliteSession(t, s, u, "first")
	at := sqliteTestAt.Add(time.Minute)
	reset, err := s.ResetPassword(ctx, u.Username, "replacement", at, sqliteEvent())
	if err != nil || !reset.PasswordChangeRequired || !reset.UpdatedAt.Equal(at) {
		t.Fatalf("reset: %+v %v", reset, err)
	}
	var resource, revoked string
	if err := db.QueryRow("SELECT resource_id FROM audit_events").Scan(&resource); err != nil || resource != u.ID {
		t.Fatalf("mandatory resource: %s %v", resource, err)
	}
	if err := db.QueryRow("SELECT revoked_at FROM sessions WHERE id=?", session.ID).Scan(&revoked); err != nil {
		t.Fatal(err)
	}
	want, _ := sqliteTime(at)
	if revoked != want {
		t.Fatal("reset did not use explicit clock")
	}
	current := sqliteSession(t, s, u, "current")
	at = at.Add(time.Minute)
	changed, err := s.CompletePasswordChange(ctx, u.ID, current.ID, "final", at, sqliteEvent())
	if err != nil || changed.PasswordChangeRequired || !changed.UpdatedAt.Equal(at) {
		t.Fatalf("change: %+v %v", changed, err)
	}
	if found, err := s.FindSessionByTokenHash(ctx, "current"); err != nil || found == nil {
		t.Fatal("current session lost", err)
	}
}

func TestSQLiteConstructorAndSQLBoundaries(t *testing.T) {
	ctx := context.Background()
	db, err := sqlitedb.Open(filepath.Join(t.TempDir(), "unmigrated.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := NewSQLiteStore(db, sqlitedb.NewWriter(db, nil))
	var tables int
	if err := db.QueryRow("SELECT count(*) FROM sqlite_schema WHERE type='table'").Scan(&tables); err != nil || tables != 0 {
		t.Fatalf("constructor schema: %d %v", tables, err)
	}
	// Deliberately unmigrated caller-owned database: no authority tables are dropped.
	if err := s.AddAudit(ctx, sqliteEvent()); err == nil {
		t.Fatal("missing audit table suppressed")
	}
	key, _ := sqliteTime(sqliteTestAt)
	for _, tc := range []struct {
		active  int64
		created any
	}{{0, key}, {1, key}, {2, key}, {1, nil}} {
		row := db.QueryRow("SELECT 'id','name','hash','operator',?,0,?,?", tc.active, tc.created, key)
		u, err := sqliteScanUser(row)
		valid := tc.active != 2 && tc.created != nil
		if valid && (err != nil || u.Active != (tc.active == 1)) {
			t.Fatalf("boolean scan: %+v %v", u, err)
		}
		if !valid && err == nil {
			t.Fatal("invalid SQL user values accepted")
		}
	}
	if _, err := sqliteScanSession(db.QueryRow("SELECT 'id','user','token',NULL,NULL,NULL,?", key)); err == nil {
		t.Fatal("SQL NULL expiry accepted")
	}
	if _, err := sqliteScanSession(db.QueryRow("SELECT 'id','user','token',?,'2',NULL,?", key, key)); err == nil {
		t.Fatal("nullable infinity accepted")
	}
	if _, err := sqliteAfter(make(chan int)); err == nil {
		t.Fatal("unmarshalable audit accepted")
	}
}

// The wrapper controls scheduling only; both mutations execute on real SQLite.
type sqliteLoginInterleave struct {
	Store
	beforeInsert func() error
}

func (s sqliteLoginInterleave) CreateSession(ctx context.Context, session Session) (Session, error) {
	if err := s.beforeInsert(); err != nil {
		return Session{}, err
	}
	return s.Store.CreateSession(ctx, session)
}

func (s sqliteLoginInterleave) CreateSessionForCredential(ctx context.Context, session Session, hash string) (Session, User, error) {
	if err := s.beforeInsert(); err != nil {
		return Session{}, User{}, err
	}
	return s.Store.(credentialSessionStore).CreateSessionForCredential(ctx, session, hash)
}

func TestSQLiteLoginResetActivationGate(t *testing.T) {
	// Reset wins after password verification: no stale session may be inserted.
	s, _, _ := sqliteFixture(t)
	ctx := context.Background()
	opts := ServiceOptions{Now: func() time.Time { return sqliteTestAt }}
	resetter := NewService(s, opts)
	u, err := resetter.Bootstrap(ctx, "owner", "Original-password-123!")
	if err != nil {
		t.Fatal(err)
	}
	interleaved := sqliteLoginInterleave{Store: s, beforeInsert: func() error {
		_, err := resetter.ResetPassword(ctx, u.Username)
		return err
	}}
	_, err = NewService(interleaved, opts).Login(ctx, u.Username, "Original-password-123!")
	if !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("stale login accepted: %v", err)
	}
	users, err := s.ListUsers(ctx)
	if err != nil || len(users) != 1 || !users[0].PasswordChangeRequired {
		t.Fatal("reset did not persist", err)
	}
}
