package configuration

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

const tokenOwner = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
const tokenID = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
const tokenOther = "cccccccc-cccc-cccc-cccc-cccccccccccc"

var tokenCreated = time.Date(1999, 12, 31, 23, 59, 59, 123456789, time.UTC)
var tokenExpires = tokenCreated.Add(2 * time.Second)

func tokenKey(t *testing.T, at time.Time) string {
	t.Helper()
	key, err := sqlitedb.EncodeTimestamp(sqlitedb.FiniteTimestamp{Seconds: at.Unix(), Nanos: int32(at.Nanosecond())})
	if err != nil {
		t.Fatal(err)
	}
	return *key
}

func tokenFixture(t *testing.T) (*sql.DB, *SQLiteEnrollmentTokenStore, string) {
	t.Helper()
	db, _, path := sqliteSettingsFixture(t)
	settingsExec(t, db, `insert into users(id,username,password_hash,role,created_at,updated_at) values(?, 'synthetic-owner','dummy','owner','0','0')`, tokenOwner)
	settingsExec(t, db, `insert into installation_identity(id,installation_id,node_id,role,updated_at) values(?,?,?,'standalone-primary','0')`, installationID, tokenID, tokenOther)
	return db, NewSQLiteEnrollmentTokenStore(settingsWriter(db)), path
}

func createToken(t *testing.T, s *SQLiteEnrollmentTokenStore) {
	t.Helper()
	role, err := s.CreateEnrollmentToken(context.Background(), tokenID, "selector", "synthetic-hash", EnrollmentTokenHashVersion, tokenOwner, tokenCreated, tokenExpires)
	if err != nil || string(role) != "primary" {
		t.Fatalf("create role=%q err=%v", role, err)
	}
}

func TestSQLiteEnrollmentTokenCreate(t *testing.T) {
	db, s, _ := tokenFixture(t)
	createToken(t, s)
	var role, updated, created, expires string
	if err := db.QueryRow(`select role,updated_at,created_at,expires_at from installation_identity cross join enrollment_tokens`).Scan(&role, &updated, &created, &expires); err != nil {
		t.Fatal(err)
	}
	if role != "primary" || updated != tokenKey(t, tokenCreated) || created != updated || expires != tokenKey(t, tokenExpires) {
		t.Fatalf("unexpected persisted clocks/role: %q %q %q %q", role, updated, created, expires)
	}
}

func TestSQLiteEnrollmentTokenMissingDenied(t *testing.T) {
	_, s, _ := tokenFixture(t)
	if err := s.CheckEnrollmentToken(context.Background(), "missing", "synthetic-hash", tokenCreated, false); !errors.Is(err, errEnrollmentTokenDenied) {
		t.Fatalf("missing: %v", err)
	}
}

func TestSQLiteEnrollmentTokenEligibility(t *testing.T) {
	for _, tc := range []struct {
		name, role         string
		leadership, latest int64
		version            string
		missing, allowed   bool
	}{
		{"standalone", "standalone-primary", 1, 1, EnrollmentTokenHashVersion, false, true},
		{"primary", "primary", 4, 2, EnrollmentTokenHashVersion, false, true},
		{"with nodes", "primary-with-nodes", 2, 2, EnrollmentTokenHashVersion, false, true},
		{"node", "node", 1, 1, EnrollmentTokenHashVersion, false, false},
		{"stale role", "stale-primary", 3, 1, EnrollmentTokenHashVersion, false, false},
		{"stale generation", "primary", 1, 2, EnrollmentTokenHashVersion, false, false},
		{"missing", "primary", 1, 1, EnrollmentTokenHashVersion, true, false},
		{"unsupported version", "standalone-primary", 1, 1, "unsupported", false, false},
		{"signed generations", "primary", -1, -2, EnrollmentTokenHashVersion, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, s, _ := tokenFixture(t)
			settingsExec(t, db, `update installation_identity set role=?,leadership_generation=?,latest_known_generation=?`, tc.role, tc.leadership, tc.latest)
			if tc.missing {
				settingsExec(t, db, `delete from installation_identity`)
			}
			role, err := s.CreateEnrollmentToken(context.Background(), tokenID, "", "", tc.version, tokenOwner, tokenCreated, tokenExpires)
			if !tc.allowed {
				if !errors.Is(err, errEnrollmentTokenDenied) || role != "" {
					t.Fatalf("role=%q err=%v", role, err)
				}
				var n int
				if err := db.QueryRow(`select count(*) from enrollment_tokens`).Scan(&n); err != nil || n != 0 {
					t.Fatalf("tokens=%d err=%v", n, err)
				}
				if !tc.missing {
					var gotRole, stamp string
					if err := db.QueryRow(`select role,updated_at from installation_identity`).Scan(&gotRole, &stamp); err != nil {
						t.Fatal(err)
					}
					if gotRole != tc.role || stamp != "0" {
						t.Fatalf("denial mutated identity: %q %q", gotRole, stamp)
					}
				}
				return
			}
			want := tc.role
			if want == "standalone-primary" {
				want = "primary"
			}
			if err != nil || string(role) != want {
				t.Fatalf("role=%q err=%v", role, err)
			}
			if err := s.CheckEnrollmentToken(context.Background(), "", "", tokenCreated, false); err != nil {
				t.Fatalf("empty selector/hash are reference-compatible: %v", err)
			}
		})
	}
}

func TestSQLiteEnrollmentTokenCreateRollback(t *testing.T) {
	for _, failure := range []string{"duplicate id", "duplicate selector", "owner FK", "expiry equality", "expiry earlier", "selector constraint", "hash constraint", "identity DB failure", "insert DB failure"} {
		t.Run(failure, func(t *testing.T) {
			db, s, _ := tokenFixture(t)
			createToken(t, s)
			settingsExec(t, db, `update installation_identity set role='standalone-primary',updated_at='0'`)
			id, selector, hash, owner, expires := tokenOther, "other", "synthetic-hash", tokenOwner, tokenExpires
			switch failure {
			case "duplicate id":
				id = tokenID
			case "duplicate selector":
				selector = "selector"
			case "owner FK":
				owner = tokenOther
			case "expiry equality":
				expires = tokenCreated
			case "expiry earlier":
				expires = tokenCreated.Add(-time.Nanosecond)
			case "selector constraint":
				selector = "bad\x00selector"
			case "hash constraint":
				hash = "bad\x00hash"
			case "identity DB failure":
				settingsExec(t, db, `create trigger fail_identity before update on installation_identity begin select raise(abort,'synthetic identity failure'); end`)
			case "insert DB failure":
				settingsExec(t, db, `create trigger fail_token before insert on enrollment_tokens begin select raise(abort,'synthetic insert failure'); end`)
			}
			role, err := s.CreateEnrollmentToken(context.Background(), id, selector, hash, EnrollmentTokenHashVersion, owner, tokenCreated, expires)
			if err == nil || errors.Is(err, errEnrollmentTokenDenied) || role != "" {
				t.Fatalf("DB failure role=%q err=%v", role, err)
			}
			var gotRole, stamp string
			if err := db.QueryRow(`select role,updated_at from installation_identity`).Scan(&gotRole, &stamp); err != nil {
				t.Fatal(err)
			}
			if gotRole != "standalone-primary" || stamp != "0" {
				t.Fatalf("promotion escaped rollback: %q %q", gotRole, stamp)
			}
			var n int
			if err := db.QueryRow(`select count(*) from enrollment_tokens`).Scan(&n); err != nil || n != 1 {
				t.Fatalf("tokens=%d err=%v", n, err)
			}
		})
	}
}

// A complete token-row snapshot detects mutation of any lifecycle or payload field.
func tokenRow(t *testing.T, db *sql.DB) []any {
	t.Helper()
	rows, err := db.Query(`select * from enrollment_tokens order by id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var all []any
	for rows.Next() {
		values := make([]any, len(cols))
		dest := make([]any, len(cols))
		for i := range values {
			dest[i] = &values[i]
		}
		if err := rows.Scan(dest...); err != nil {
			t.Fatal(err)
		}
		all = append(all, values)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return all
}

func TestSQLiteEnrollmentTokenCheckMatrix(t *testing.T) {
	for _, tc := range []struct {
		name, mutation, hash string
		now                  time.Time
		denied               bool
	}{
		{"valid", "", "synthetic-hash", tokenCreated, false},
		{"wrong same length hash", "", "synthetic-xxxx", tokenCreated, true},
		{"wrong short hash", "", "x", tokenCreated, true},
		{"empty hash", "", "", tokenCreated, true},
		{"unsupported stored version", `update enrollment_tokens set hash_version='future'`, "synthetic-hash", tokenCreated, true},
		{"expiry equality", "", "synthetic-hash", tokenExpires, true},
		{"one nanosecond before", "", "synthetic-hash", tokenExpires.Add(-time.Nanosecond), false},
		{"one nanosecond after", "", "synthetic-hash", tokenExpires.Add(time.Nanosecond), true},
		{"cross year", "", "synthetic-hash", time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), false},
		{"positive infinity", `update enrollment_tokens set expires_at='2'`, "synthetic-hash", time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC), false},
		{"consumed", `update enrollment_tokens set consumed_at='0'`, "synthetic-hash", tokenCreated, true},
		{"revoked", `update enrollment_tokens set revoked_at='0'`, "synthetic-hash", tokenCreated, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, s, _ := tokenFixture(t)
			createToken(t, s)
			if tc.mutation != "" {
				settingsExec(t, db, tc.mutation)
			}
			before := tokenRow(t, db)
			err := s.CheckEnrollmentToken(context.Background(), "selector", tc.hash, tc.now, false)
			if tc.denied {
				if !errors.Is(err, errEnrollmentTokenDenied) {
					t.Fatalf("expected denied: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if after := tokenRow(t, db); !reflect.DeepEqual(before, after) {
				t.Fatalf("nonconsume mutated token: before=%v after=%v", before, after)
			}
		})
	}
}

func TestSQLiteEnrollmentTokenNegativeInfinity(t *testing.T) {
	db, s, _ := tokenFixture(t)
	db.SetMaxOpenConns(1)
	createToken(t, s)
	// A negative-infinity expiry requires an intentionally corrupt fixture: the
	// migrated expiry CHECK otherwise (correctly) prevents such a row.
	settingsExec(t, db, `pragma ignore_check_constraints=on`)
	settingsExec(t, db, `update enrollment_tokens set expires_at='0'`)
	settingsExec(t, db, `pragma ignore_check_constraints=off`)
	if err := s.CheckEnrollmentToken(context.Background(), "selector", "synthetic-hash", tokenCreated, false); !errors.Is(err, errEnrollmentTokenDenied) {
		t.Fatalf("negative infinity: %v", err)
	}
	settingsExec(t, db, `pragma ignore_check_constraints=on`)
	settingsExec(t, db, `update enrollment_tokens set expires_at='bad-key'`)
	settingsExec(t, db, `pragma ignore_check_constraints=off`)
	if err := s.CheckEnrollmentToken(context.Background(), "selector", "synthetic-hash", tokenCreated, false); err == nil || errors.Is(err, errEnrollmentTokenDenied) {
		t.Fatalf("invalid key must propagate: %v", err)
	}
}

func TestSQLiteEnrollmentTokenLifecycle(t *testing.T) {
	db, s, _ := tokenFixture(t)
	createToken(t, s)
	ctx := context.Background()
	now := tokenCreated.Add(time.Nanosecond)
	if err := s.CheckEnrollmentToken(ctx, "selector", "synthetic-hash", now, true); err != nil {
		t.Fatal(err)
	}
	var consumed string
	var attempt, revoked *string
	if err := db.QueryRow(`select consumed_at,consumed_by_attempt_id,revoked_at from enrollment_tokens`).Scan(&consumed, &attempt, &revoked); err != nil {
		t.Fatal(err)
	}
	if consumed != tokenKey(t, now) || attempt != nil || revoked != nil {
		t.Fatalf("consume: %q %v %v", consumed, attempt, revoked)
	}
	for _, consume := range []bool{false, true} {
		if err := s.CheckEnrollmentToken(ctx, "selector", "synthetic-hash", now, consume); !errors.Is(err, errEnrollmentTokenDenied) {
			t.Fatalf("reuse consume=%v: %v", consume, err)
		}
	}
	if err := s.RevokeEnrollmentToken(ctx, tokenID, tokenOwner, now); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeEnrollmentToken(ctx, tokenID, tokenOwner, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`select consumed_at,consumed_by_attempt_id,revoked_at from enrollment_tokens`).Scan(&consumed, &attempt, &revoked); err != nil {
		t.Fatal(err)
	}
	if consumed != tokenKey(t, now) || attempt != nil || revoked == nil || *revoked != tokenKey(t, now) {
		t.Fatalf("first revoke/consumption: %q %v %v", consumed, attempt, revoked)
	}
}

func TestSQLiteEnrollmentTokenRevokeNoops(t *testing.T) {
	for _, tc := range []struct{ name, id, owner string }{
		{"missing", tokenOther, tokenOwner},
		{"wrong owner", tokenID, tokenOther},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, s, _ := tokenFixture(t)
			createToken(t, s)
			before := tokenRow(t, db)
			if err := s.RevokeEnrollmentToken(context.Background(), tc.id, tc.owner, tokenCreated); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, tokenRow(t, db)) {
				t.Fatal("noop changed row")
			}
		})
	}
}

func TestSQLiteEnrollmentTokenUUIDCompatibility(t *testing.T) {
	for _, alias := range []func(string) string{
		strings.ToUpper,
		func(id string) string { return strings.ReplaceAll(id, "-", "") },
		func(id string) string { return "{" + id + "}" },
		func(id string) string {
			return strings.ReplaceAll(id, "-", "")[:4] + "-" + strings.ReplaceAll(id, "-", "")[4:]
		},
	} {
		t.Run(alias(tokenID), func(t *testing.T) {
			db, s, _ := tokenFixture(t)
			if _, err := s.CreateEnrollmentToken(context.Background(), alias(tokenID), "selector", "synthetic-hash", EnrollmentTokenHashVersion, alias(tokenOwner), tokenCreated, tokenExpires); err != nil {
				t.Fatal(err)
			}
			var id, owner string
			if err := db.QueryRow(`select id,created_by_user_id from enrollment_tokens`).Scan(&id, &owner); err != nil {
				t.Fatal(err)
			}
			if id != tokenID || owner != tokenOwner {
				t.Fatalf("noncanonical persisted %q %q", id, owner)
			}
			if err := s.RevokeEnrollmentToken(context.Background(), alias(tokenID), alias(tokenOwner), tokenCreated); err != nil {
				t.Fatal(err)
			}
			if err := s.CheckEnrollmentToken(context.Background(), "selector", "synthetic-hash", tokenCreated, false); !errors.Is(err, errEnrollmentTokenDenied) {
				t.Fatalf("alias revoke missed: %v", err)
			}
		})
	}
	for _, target := range []string{"create id", "create owner", "revoke id", "revoke owner"} {
		t.Run(target, func(t *testing.T) {
			_, s, _ := tokenFixture(t)
			var err error
			switch target {
			case "create id":
				_, err = s.CreateEnrollmentToken(context.Background(), "bad", "s", "h", EnrollmentTokenHashVersion, tokenOwner, tokenCreated, tokenExpires)
			case "create owner":
				_, err = s.CreateEnrollmentToken(context.Background(), tokenID, "s", "h", EnrollmentTokenHashVersion, "bad", tokenCreated, tokenExpires)
			case "revoke id":
				err = s.RevokeEnrollmentToken(context.Background(), "bad", tokenOwner, tokenCreated)
			case "revoke owner":
				err = s.RevokeEnrollmentToken(context.Background(), tokenID, "bad", tokenCreated)
			}
			if err == nil || err.Error() != postgresInvalidUUIDError("bad").Error() {
				t.Fatalf("invalid UUID: %v", err)
			}
		})
	}
}

func TestSQLiteEnrollmentTokenReopen(t *testing.T) {
	db, s, path := tokenFixture(t)
	createToken(t, s)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := sqlitedb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	s = NewSQLiteEnrollmentTokenStore(settingsWriter(reopened))
	if err := s.CheckEnrollmentToken(context.Background(), "selector", "synthetic-hash", tokenCreated, true); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeEnrollmentToken(context.Background(), tokenID, tokenOwner, tokenCreated); err != nil {
		t.Fatal(err)
	}
	var consumed, revoked string
	if err := reopened.QueryRow(`select consumed_at,revoked_at from enrollment_tokens`).Scan(&consumed, &revoked); err != nil {
		t.Fatal(err)
	}
	if consumed != tokenKey(t, tokenCreated) || revoked != consumed {
		t.Fatalf("reopened: %q %q", consumed, revoked)
	}
}

func tokenOperations(s *SQLiteEnrollmentTokenStore) []struct {
	name string
	run  func(context.Context) error
} {
	return []struct {
		name string
		run  func(context.Context) error
	}{
		{"create", func(ctx context.Context) error {
			_, err := s.CreateEnrollmentToken(ctx, tokenOther, "other", "h", EnrollmentTokenHashVersion, tokenOwner, tokenCreated, tokenExpires)
			return err
		}},
		{"check", func(ctx context.Context) error {
			return s.CheckEnrollmentToken(ctx, "selector", "synthetic-hash", tokenCreated, false)
		}},
		{"consume", func(ctx context.Context) error {
			return s.CheckEnrollmentToken(ctx, "selector", "synthetic-hash", tokenCreated, true)
		}},
		{"revoke", func(ctx context.Context) error {
			return s.RevokeEnrollmentToken(ctx, tokenID, tokenOwner, tokenCreated)
		}},
	}
}

func TestSQLiteEnrollmentTokenCancellationAndClosedDB(t *testing.T) {
	for _, failure := range []string{"canceled", "closed"} {
		for index := 0; index < 4; index++ {
			t.Run(failure+"/"+[]string{"create", "check", "consume", "revoke"}[index], func(t *testing.T) {
				db, s, _ := tokenFixture(t)
				createToken(t, s)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				before := tokenRow(t, db)
				if failure == "canceled" {
					cancel()
				} else if err := db.Close(); err != nil {
					t.Fatal(err)
				}
				err := tokenOperations(s)[index].run(ctx)
				if err == nil || errors.Is(err, errEnrollmentTokenDenied) {
					t.Fatalf("DB/context error: %v", err)
				}
				if failure == "canceled" {
					if !errors.Is(err, context.Canceled) {
						t.Fatalf("cancellation: %v", err)
					}
					if !reflect.DeepEqual(before, tokenRow(t, db)) {
						t.Fatal("canceled operation mutated row")
					}
				}
			})
		}
	}
}

func TestSQLiteEnrollmentTokenDatabaseFailures(t *testing.T) {
	for _, failure := range []string{"check query", "consume update", "consume zero rows", "revoke update"} {
		t.Run(failure, func(t *testing.T) {
			db, s, _ := tokenFixture(t)
			createToken(t, s)
			before := tokenRow(t, db)
			var err error
			switch failure {
			case "check query":
				settingsExec(t, db, `alter table enrollment_tokens rename to unavailable_tokens`)
				err = s.CheckEnrollmentToken(context.Background(), "selector", "synthetic-hash", tokenCreated, false)
			case "consume update":
				settingsExec(t, db, `create trigger fail_consume before update on enrollment_tokens begin select raise(abort,'synthetic consume failure'); end`)
				err = s.CheckEnrollmentToken(context.Background(), "selector", "synthetic-hash", tokenCreated, true)
			case "consume zero rows":
				settingsExec(t, db, `create trigger ignore_consume before update on enrollment_tokens begin select raise(ignore); end`)
				err = s.CheckEnrollmentToken(context.Background(), "selector", "synthetic-hash", tokenCreated, true)
			case "revoke update":
				settingsExec(t, db, `create trigger fail_revoke before update on enrollment_tokens begin select raise(abort,'synthetic revoke failure'); end`)
				err = s.RevokeEnrollmentToken(context.Background(), tokenID, tokenOwner, tokenCreated)
			}
			if failure == "consume zero rows" {
				if !errors.Is(err, errEnrollmentTokenDenied) {
					t.Fatalf("zero rows: %v", err)
				}
			} else if err == nil || errors.Is(err, errEnrollmentTokenDenied) {
				t.Fatalf("DB error must propagate: %v", err)
			}
			if failure != "check query" && !reflect.DeepEqual(before, tokenRow(t, db)) {
				t.Fatal("failed update mutated token")
			}
		})
	}
}

// The first handle is paused by its instance clock after BEGIN IMMEDIATE.
// The competing operation runs on a second pool, proving file-level exclusion
// rather than an in-process mutex. Channel ordering fixes which writer wins.
func TestSQLiteEnrollmentTokenTwoHandleSerialization(t *testing.T) {
	for _, order := range []string{"consume/consume", "consume/revoke", "revoke/consume", "check/revoke"} {
		t.Run(order, func(t *testing.T) {
			db, s, path := tokenFixture(t)
			createToken(t, s)
			other, err := sqlitedb.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer other.Close()
			locked := make(chan struct{})
			release := make(chan struct{})
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
			}()
			first := NewSQLiteEnrollmentTokenStore(sqlitedb.NewWriter(db, func() sqlitedb.FiniteTimestamp {
				close(locked)
				<-release
				return sqlitedb.FiniteTimestamp{Seconds: 123}
			}))
			second := NewSQLiteEnrollmentTokenStore(settingsWriter(other))
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			call := func(store *SQLiteEnrollmentTokenStore, op string, now time.Time) error {
				if op == "revoke" {
					return store.RevokeEnrollmentToken(ctx, tokenID, tokenOwner, now)
				}
				return store.CheckEnrollmentToken(ctx, "selector", "synthetic-hash", now, op == "consume")
			}
			ops := strings.Split(order, "/")
			firstDone := make(chan error, 1)
			secondDone := make(chan error, 1)
			go func() { firstDone <- call(first, ops[0], tokenCreated) }()
			select {
			case <-locked:
			case <-ctx.Done():
				t.Fatal("first writer did not lock")
			}
			started := make(chan struct{})
			go func() { close(started); secondDone <- call(second, ops[1], tokenCreated.Add(time.Nanosecond)) }()
			<-started
			select {
			case err := <-secondDone:
				t.Fatalf("competing writer bypassed held transaction: %v", err)
			case <-time.After(25 * time.Millisecond):
			}
			close(release)
			select {
			case err := <-firstDone:
				if err != nil {
					t.Fatalf("first: %v", err)
				}
			case <-ctx.Done():
				t.Fatal("first writer timed out")
			}
			select {
			case err := <-secondDone:
				denied := ops[1] == "consume"
				if denied {
					if !errors.Is(err, errEnrollmentTokenDenied) {
						t.Fatalf("second consume must lose: %v", err)
					}
				} else if err != nil {
					t.Fatalf("second: %v", err)
				}
			case <-ctx.Done():
				t.Fatal("competing writer timed out")
			}
			var consumed, revoked, attempt *string
			if err := db.QueryRow(`select consumed_at,revoked_at,consumed_by_attempt_id from enrollment_tokens`).Scan(&consumed, &revoked, &attempt); err != nil {
				t.Fatal(err)
			}
			if attempt != nil {
				t.Fatal("unexpected enrollment attempt linkage")
			}
			if ops[0] == "consume" {
				if consumed == nil || *consumed != tokenKey(t, tokenCreated) {
					t.Fatalf("winner consume: %v", consumed)
				}
			} else if consumed != nil {
				t.Fatalf("losing consume persisted: %v", consumed)
			}
			if order == "consume/consume" {
				if revoked != nil {
					t.Fatal("unexpected revoke")
				}
			} else {
				want := tokenCreated.Add(time.Nanosecond)
				if ops[0] == "revoke" {
					want = tokenCreated
				}
				if revoked == nil || *revoked != tokenKey(t, want) {
					t.Fatalf("revoke stamp: %v", revoked)
				}
			}
		})
	}
}

func TestSQLiteEnrollmentTokenInvalidDependencies(t *testing.T) {
	for _, s := range []*SQLiteEnrollmentTokenStore{nil, NewSQLiteEnrollmentTokenStore(nil)} {
		for _, op := range tokenOperations(s) {
			if err := op.run(context.Background()); err == nil {
				t.Fatalf("%s accepted absent writer", op.name)
			}
		}
	}
	_, s, _ := tokenFixture(t)
	for _, op := range tokenOperations(s) {
		if err := op.run(nil); err == nil {
			t.Fatalf("%s accepted nil context", op.name)
		}
	}
}

func TestSQLiteEnrollmentTokenOneWrite(t *testing.T) {
	db, s, _ := tokenFixture(t)
	var clocks atomic.Int32
	s = NewSQLiteEnrollmentTokenStore(sqlitedb.NewWriter(db, func() sqlitedb.FiniteTimestamp { clocks.Add(1); return sqlitedb.FiniteTimestamp{Seconds: 123} }))
	createToken(t, s)
	for _, op := range tokenOperations(s)[1:] {
		if err := op.run(context.Background()); err != nil {
			t.Fatalf("%s: %v", op.name, err)
		}
	}
	if got := clocks.Load(); got != 4 {
		t.Fatalf("create/check/consume/revoke must each take one writer: %d", got)
	}
	if err := s.CheckEnrollmentToken(context.Background(), "selector", "synthetic-hash", tokenCreated, false); !errors.Is(err, errEnrollmentTokenDenied) {
		t.Fatalf("denied: %v", err)
	}
	if _, err := s.CreateEnrollmentToken(context.Background(), tokenOther, "s", "h", "unsupported", tokenOwner, tokenCreated, tokenExpires); !errors.Is(err, errEnrollmentTokenDenied) {
		t.Fatalf("version: %v", err)
	}
	if got := clocks.Load(); got != 6 {
		t.Fatalf("denied operations must serialize too: %d", got)
	}
}
