package auth

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
	"modernc.org/sqlite"
	sqlitecodes "modernc.org/sqlite/lib"
)

// Operational auth clocks exclude the foundation's NULL/infinity variants.
const sqliteMinSecond int64 = -62135596800
const sqliteMaxSecond int64 = 253402300799

func sqliteTime(t time.Time) (string, error) {
	t = t.UTC()
	if t.Year() < 1 || t.Year() > 9999 {
		return "", fmt.Errorf("auth timestamp outside years 1..9999")
	}
	key, err := sqlitedb.EncodeTimestamp(sqlitedb.FiniteTimestamp{Seconds: t.Unix(), Nanos: int32(t.Nanosecond())})
	if err != nil {
		return "", err
	}
	return *key, nil
}

func sqliteDecodeTime(key sql.NullString) (time.Time, error) {
	if !key.Valid {
		return time.Time{}, fmt.Errorf("required auth timestamp is NULL")
	}
	value, err := sqlitedb.DecodeTimestamp(&key.String)
	if err != nil {
		return time.Time{}, err
	}
	finite, ok := value.(sqlitedb.FiniteTimestamp)
	if !ok || finite.Seconds < sqliteMinSecond || finite.Seconds > sqliteMaxSecond {
		return time.Time{}, fmt.Errorf("auth timestamp must be finite within years 1..9999")
	}
	t := time.Unix(finite.Seconds, int64(finite.Nanos)).UTC()
	encoded, err := sqliteTime(t)
	if err != nil || encoded != key.String {
		return time.Time{}, fmt.Errorf("auth timestamp roundtrip failed")
	}
	return t, nil
}

func sqliteNullableTime(key sql.NullString) (*time.Time, error) {
	if !key.Valid {
		return nil, nil
	}
	t, err := sqliteDecodeTime(key)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func sqliteTimePointer(t *time.Time) (any, error) {
	if t == nil {
		return nil, nil
	}
	return sqliteTime(*t)
}

func sqliteBool(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

func sqliteReadBool(n int64) (bool, error) {
	if n != 0 && n != 1 {
		return false, fmt.Errorf("invalid auth boolean %d", n)
	}
	return n == 1, nil
}

func sqliteScanUser(row scanner) (User, error) {
	var u User
	var active, required int64
	var created, updated sql.NullString
	if err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &active, &required, &created, &updated); err != nil {
		return User{}, err
	}
	var err error
	if u.Active, err = sqliteReadBool(active); err != nil {
		return User{}, err
	}
	if u.PasswordChangeRequired, err = sqliteReadBool(required); err != nil {
		return User{}, err
	}
	if u.CreatedAt, err = sqliteDecodeTime(created); err != nil {
		return User{}, err
	}
	if u.UpdatedAt, err = sqliteDecodeTime(updated); err != nil {
		return User{}, err
	}
	return u, nil
}

func sqliteUserPointer(row scanner) (*User, error) {
	u, err := sqliteScanUser(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

func sqliteScanSession(row scanner) (Session, error) {
	var s Session
	var expires, revoked, seen, created sql.NullString
	if err := row.Scan(&s.ID, &s.UserID, &s.TokenHash, &expires, &revoked, &seen, &created); err != nil {
		return Session{}, err
	}
	var err error
	if s.ExpiresAt, err = sqliteDecodeTime(expires); err != nil {
		return Session{}, err
	}
	if s.CreatedAt, err = sqliteDecodeTime(created); err != nil {
		return Session{}, err
	}
	if s.RevokedAt, err = sqliteNullableTime(revoked); err != nil {
		return Session{}, err
	}
	if s.LastSeenAt, err = sqliteNullableTime(seen); err != nil {
		return Session{}, err
	}
	return s, nil
}

func sqliteDefaults(v sqlitedb.WriteValue, id *string, stamps ...*time.Time) error {
	var err error
	if *id == "" {
		if *id, err = v.NewUUID(); err != nil {
			return err
		}
	}
	now, err := sqliteDecodeTime(sql.NullString{String: v.Timestamp(), Valid: true})
	if err != nil {
		return err
	}
	for _, stamp := range stamps {
		if stamp.IsZero() {
			*stamp = now
		}
	}
	return nil
}

func sqliteAfter(value any) (any, error) {
	if value == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return string(encoded), nil // STRICT TEXT: never bind []byte (BLOB).
}

// Keep extended codes intact; do not turn every constraint into a username collision.
func sqliteConstraintCode(err error) int {
	var driverError *sqlite.Error
	if !errors.As(err, &driverError) {
		return 0
	}
	switch driverError.Code() {
	case sqlitecodes.SQLITE_CONSTRAINT_UNIQUE, sqlitecodes.SQLITE_CONSTRAINT_PRIMARYKEY,
		sqlitecodes.SQLITE_CONSTRAINT_FOREIGNKEY, sqlitecodes.SQLITE_CONSTRAINT_CHECK,
		sqlitecodes.SQLITE_CONSTRAINT_NOTNULL, sqlitecodes.SQLITE_CONSTRAINT_DATATYPE:
		return driverError.Code()
	}
	return 0
}
