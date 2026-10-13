package auth

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

var sqliteTestAt = time.Date(2026, 1, 2, 3, 4, 5, 123456789, time.UTC)

func sqliteFixture(t *testing.T) (*SQLiteStore, *sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "auth.sqlite")
	db, err := sqlitedb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := sqlitedb.Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	writer := sqlitedb.NewWriter(db, func() sqlitedb.FiniteTimestamp {
		return sqlitedb.FiniteTimestamp{Seconds: sqliteTestAt.Unix(), Nanos: int32(sqliteTestAt.Nanosecond())}
	})
	return NewSQLiteStore(db, writer), db, path
}

func TestSQLiteServiceLifecycle(t *testing.T) {
	store, db, path := sqliteFixture(t)
	ctx := context.Background()
	now := sqliteTestAt
	svc := NewService(store, ServiceOptions{Now: func() time.Time { return now }, SessionTTL: time.Hour})
	owner, err := svc.Bootstrap(ctx, "owner", "Original-password-123!")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.Login(ctx, "owner", "wrong"); !errors.Is(err, ErrInvalidCredential) {
		t.Fatal(err)
	}
	login, err := svc.Login(ctx, "owner", "Original-password-123!")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.Authenticate(ctx, login.Token); err != nil {
		t.Fatal(err)
	}
	touched, err := store.FindSessionByTokenHash(ctx, HashOpaqueToken(login.Token))
	if err != nil || touched == nil || touched.LastSeenAt == nil || !touched.LastSeenAt.Equal(now) {
		t.Fatalf("touch was not persisted: %+v %v", touched, err)
	}
	temporary, err := svc.ResetPassword(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.Authenticate(ctx, login.Token); !errors.Is(err, ErrInvalidSession) {
		t.Fatal(err)
	}
	restricted, err := svc.Login(ctx, "owner", temporary)
	if err != nil || !restricted.User.PasswordChangeRequired {
		t.Fatalf("restricted: %+v %v", restricted, err)
	}
	other, err := svc.Login(ctx, "owner", temporary)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := svc.ChangePassword(ctx, restricted.Token, "Replacement-password-123!")
	if err != nil || changed.PasswordChangeRequired {
		t.Fatalf("change: %+v %v", changed, err)
	}
	if _, err = svc.Authenticate(ctx, other.Token); !errors.Is(err, ErrInvalidSession) {
		t.Fatal(err)
	}
	if _, err = svc.Authenticate(ctx, restricted.Token); err != nil {
		t.Fatal(err)
	}
	if err = svc.Logout(ctx, restricted.Token); err != nil {
		t.Fatal(err)
	}
	if _, err = svc.Authenticate(ctx, restricted.Token); !errors.Is(err, ErrInvalidSession) {
		t.Fatal(err)
	}
	if _, err = svc.Authenticate(ctx, "missing"); !errors.Is(err, ErrInvalidSession) {
		t.Fatal(err)
	}
	edge, err := svc.Login(ctx, "owner", "Replacement-password-123!")
	if err != nil {
		t.Fatal(err)
	}
	now = edge.ExpiresAt
	if _, err = svc.Authenticate(ctx, edge.Token); !errors.Is(err, ErrInvalidSession) {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := sqlitedb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	persisted, err := NewSQLiteStore(reopened, sqlitedb.NewWriter(reopened, nil)).FindUserByID(ctx, owner.ID)
	if err != nil || persisted == nil || persisted.PasswordHash != changed.PasswordHash {
		t.Fatalf("reopen: %v %v", persisted, err)
	}
}
