package auth

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

// SQLiteStore is additive and inactive until explicitly selected by a caller.
// Service.Login uses the optional credential capability to serialize hash
// revalidation and insertion against SQLite password mutations. CreateSession
// remains generic; PG fallback is unchanged. Runtime wiring and full migration
// readiness are outside this adapter's contract.
// The caller owns Open, Migrate, Close, and the matching database/writer pair.
type SQLiteStore struct {
	db     *sql.DB
	writer *sqlitedb.Writer
}

var _ Store = (*SQLiteStore)(nil)

func NewSQLiteStore(db *sql.DB, writer *sqlitedb.Writer) *SQLiteStore {
	return &SQLiteStore{db: db, writer: writer}
}

const sqliteUserColumns = "id,username,password_hash,role,active,password_change_required,created_at,updated_at"
const sqliteSessionColumns = "id,user_id,token_hash,expires_at,revoked_at,last_seen_at,created_at"

func (s *SQLiteStore) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+sqliteUserColumns+" FROM users ORDER BY username")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	users := []User{}
	for rows.Next() {
		u, err := sqliteScanUser(rows)
		if err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

func (s *SQLiteStore) FindUserByUsername(ctx context.Context, username string) (*User, error) {
	return sqliteUserPointer(s.db.QueryRowContext(ctx, "SELECT "+sqliteUserColumns+" FROM users WHERE username=?", username))
}

func (s *SQLiteStore) FindUserByID(ctx context.Context, id string) (*User, error) {
	return sqliteUserPointer(s.db.QueryRowContext(ctx, "SELECT "+sqliteUserColumns+" FROM users WHERE id=?", id))
}

func sqliteInsertUser(v sqlitedb.WriteValue, u *User) error {
	if err := sqliteDefaults(v, &u.ID, &u.CreatedAt, &u.UpdatedAt); err != nil {
		return err
	}
	created, err := sqliteTime(u.CreatedAt)
	if err != nil {
		return err
	}
	updated, err := sqliteTime(u.UpdatedAt)
	if err != nil {
		return err
	}
	_, err = v.Conn().ExecContext(v.Context(), "INSERT INTO users ("+sqliteUserColumns+") VALUES (?,?,?,?,?,?,?,?)",
		u.ID, u.Username, u.PasswordHash, string(u.Role), sqliteBool(u.Active), sqliteBool(u.PasswordChangeRequired), created, updated)
	return err
}

func (s *SQLiteStore) CreateUser(ctx context.Context, u User) (User, error) {
	err := s.writer.Write(ctx, func(v sqlitedb.WriteValue) error { return sqliteInsertUser(v, &u) })
	if err != nil {
		return User{}, err
	}
	return u, nil
}

func (s *SQLiteStore) BootstrapUser(ctx context.Context, u User) (User, error) {
	err := s.writer.Write(ctx, func(v sqlitedb.WriteValue) error {
		var count int
		if err := v.Conn().QueryRowContext(v.Context(), "SELECT count(*) FROM users WHERE active=1").Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			return ErrBootstrapComplete
		}
		u.Role, u.Active = RoleOwner, true
		return sqliteInsertUser(v, &u)
	})
	if err != nil {
		return User{}, err
	}
	return u, nil
}

func sqliteOwnerGuard(v sqlitedb.WriteValue, u User, removing bool, message string) error {
	if !u.Active || u.Role != RoleOwner || !removing {
		return nil
	}
	var count int
	if err := v.Conn().QueryRowContext(v.Context(), "SELECT count(*) FROM users WHERE active=1 AND role='owner'").Scan(&count); err != nil {
		return err
	}
	if count <= 1 {
		return errors.New(message)
	}
	return nil
}

func sqliteWriteUser(v sqlitedb.WriteValue, u User, stamp string) error {
	_, err := v.Conn().ExecContext(v.Context(), "UPDATE users SET role=?,active=?,password_hash=?,password_change_required=?,updated_at=? WHERE id=?",
		string(u.Role), sqliteBool(u.Active), u.PasswordHash, sqliteBool(u.PasswordChangeRequired), stamp, u.ID)
	return err
}

func (s *SQLiteStore) UpdateUser(ctx context.Context, id string, patch UserPatch) (User, error) {
	var result User
	err := s.writer.Write(ctx, func(v sqlitedb.WriteValue) error {
		u, err := sqliteScanUser(v.Conn().QueryRowContext(v.Context(), "SELECT "+sqliteUserColumns+" FROM users WHERE id=?", id))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrUserNotFound
		}
		if err != nil {
			return err
		}
		next := u
		if patch.Role != nil {
			next.Role = *patch.Role
		}
		if patch.Active != nil {
			next.Active = *patch.Active
		}
		if patch.PasswordHash != nil {
			next.PasswordHash = *patch.PasswordHash
		}
		if patch.PasswordChangeRequired != nil {
			next.PasswordChangeRequired = *patch.PasswordChangeRequired
		}
		if err := sqliteOwnerGuard(v, u, !next.Active || next.Role != RoleOwner, "Cannot remove or demote the last Owner"); err != nil {
			return err
		}
		next.UpdatedAt, err = sqliteDecodeTime(sql.NullString{String: v.Timestamp(), Valid: true})
		if err != nil {
			return err
		}
		if err := sqliteWriteUser(v, next, v.Timestamp()); err != nil {
			return err
		}
		result = next
		return nil
	})
	if err != nil {
		return User{}, err
	}
	return result, nil
}

func (s *SQLiteStore) DeleteUser(ctx context.Context, id string) error {
	return s.writer.Write(ctx, func(v sqlitedb.WriteValue) error {
		u, err := sqliteScanUser(v.Conn().QueryRowContext(v.Context(), "SELECT "+sqliteUserColumns+" FROM users WHERE id=?", id))
		if errors.Is(err, sql.ErrNoRows) {
			// Match PG's idempotent deletion.
			return nil
		}
		if err != nil {
			return err
		}
		if err := sqliteOwnerGuard(v, u, true, "Cannot delete the last Owner"); err != nil {
			return err
		}
		_, err = v.Conn().ExecContext(v.Context(), "DELETE FROM users WHERE id=?", id)
		return err
	})
}

// Hash equality is not a credential generation: arbitrary identical-hash
// updates cannot be detected. Service password mutations generate fresh salts.
func (s *SQLiteStore) CreateSessionForCredential(ctx context.Context, session Session, expectedHash string) (Session, User, error) {
	var user User
	err := s.writer.Write(ctx, func(v sqlitedb.WriteValue) error {
		current, err := sqliteScanUser(v.Conn().QueryRowContext(v.Context(), "SELECT "+sqliteUserColumns+" FROM users WHERE id=?", session.UserID))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrInvalidCredential
		}
		if err != nil {
			return err
		}
		if !current.Active || current.PasswordHash != expectedHash {
			return ErrInvalidCredential
		}
		user = current
		return sqliteInsertSession(v, &session)
	})
	if err != nil {
		return Session{}, User{}, err
	}
	return session, user, nil
}

func (s *SQLiteStore) CreateSession(ctx context.Context, session Session) (Session, error) {
	err := s.writer.Write(ctx, func(v sqlitedb.WriteValue) error { return sqliteInsertSession(v, &session) })
	if err != nil {
		return Session{}, err
	}
	return session, nil
}

func sqliteInsertSession(v sqlitedb.WriteValue, session *Session) error {
	if session.ExpiresAt.IsZero() {
		return errors.New("session expiry is required")
	}
	if err := sqliteDefaults(v, &session.ID, &session.CreatedAt); err != nil {
		return err
	}
	expires, err := sqliteTime(session.ExpiresAt)
	if err != nil {
		return err
	}
	created, err := sqliteTime(session.CreatedAt)
	if err != nil {
		return err
	}
	revoked, err := sqliteTimePointer(session.RevokedAt)
	if err != nil {
		return err
	}
	seen, err := sqliteTimePointer(session.LastSeenAt)
	if err != nil {
		return err
	}
	_, err = v.Conn().ExecContext(v.Context(), "INSERT INTO sessions ("+sqliteSessionColumns+") VALUES (?,?,?,?,?,?,?)",
		session.ID, session.UserID, session.TokenHash, expires, revoked, seen, created)
	return err
}

func (s *SQLiteStore) FindSessionByTokenHash(ctx context.Context, hash string) (*Session, error) {
	session, err := sqliteScanSession(s.db.QueryRowContext(ctx, "SELECT "+sqliteSessionColumns+" FROM sessions WHERE token_hash=? AND revoked_at IS NULL", hash))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &session, nil
}

func (s *SQLiteStore) RevokeSession(ctx context.Context, id string, at time.Time) error {
	return s.sessionStamp(ctx, id, at, "revoked_at")
}

func (s *SQLiteStore) TouchSession(ctx context.Context, id string, at time.Time) error {
	return s.sessionStamp(ctx, id, at, "last_seen_at")
}

func (s *SQLiteStore) sessionStamp(ctx context.Context, id string, at time.Time, column string) error {
	return s.writer.Write(ctx, func(v sqlitedb.WriteValue) error {
		stamp, err := sqliteTime(at)
		if err != nil {
			return err
		}
		_, err = v.Conn().ExecContext(v.Context(), "UPDATE sessions SET "+column+"=? WHERE id=? AND revoked_at IS NULL", stamp, id)
		return err
	})
}

func sqliteInsertAudit(v sqlitedb.WriteValue, event AuditEvent) error {
	if err := sqliteDefaults(v, &event.ID, &event.CreatedAt); err != nil {
		return err
	}
	stamp, err := sqliteTime(event.CreatedAt)
	if err != nil {
		return err
	}
	after, err := sqliteAfter(event.AfterValue)
	if err != nil {
		return err
	}
	_, err = v.Conn().ExecContext(v.Context(), `INSERT INTO audit_events
		(id,actor_user_id,action,resource_type,resource_id,before_value,after_value,correlation_id,result,created_at)
		VALUES (?,?,?,?,?,NULL,?,?,?,?)`, event.ID, event.ActorUserID, event.Action, event.ResourceType, event.ResourceID, after, event.Correlation, event.Result, stamp)
	return err
}

func (s *SQLiteStore) AddAudit(ctx context.Context, event AuditEvent) error {
	return s.writer.Write(ctx, func(v sqlitedb.WriteValue) error { return sqliteInsertAudit(v, event) })
}

func (s *SQLiteStore) ResetPassword(ctx context.Context, username, hash string, at time.Time, event AuditEvent) (User, error) {
	return s.passwordMutation(ctx, username, "", hash, at, event, true)
}

func (s *SQLiteStore) CompletePasswordChange(ctx context.Context, userID, sessionID, hash string, at time.Time, event AuditEvent) (User, error) {
	return s.passwordMutation(ctx, userID, sessionID, hash, at, event, false)
}

func (s *SQLiteStore) passwordMutation(ctx context.Context, target, sessionID, hash string, at time.Time, event AuditEvent, reset bool) (User, error) {
	var result User
	err := s.writer.Write(ctx, func(v sqlitedb.WriteValue) error {
		stamp, err := sqliteTime(at)
		if err != nil {
			return err
		}
		column := "id"
		if reset {
			column = "username"
		} else {
			session, err := sqliteScanSession(v.Conn().QueryRowContext(v.Context(), "SELECT "+sqliteSessionColumns+" FROM sessions WHERE id=?", sessionID))
			if errors.Is(err, sql.ErrNoRows) {
				return ErrInvalidSession
			}
			if err != nil {
				return err
			}
			if session.UserID != target || session.RevokedAt != nil || !session.ExpiresAt.After(at) {
				return ErrInvalidSession
			}
		}
		u, err := sqliteScanUser(v.Conn().QueryRowContext(v.Context(), "SELECT "+sqliteUserColumns+" FROM users WHERE "+column+"=?", target))
		if errors.Is(err, sql.ErrNoRows) {
			if reset {
				return ErrUserNotFound
			}
			return ErrUnavailableUser
		}
		if err != nil {
			return err
		}
		if !u.Active {
			return ErrUnavailableUser
		}
		u.PasswordHash, u.PasswordChangeRequired, u.UpdatedAt = hash, reset, at.UTC()
		if err := sqliteWriteUser(v, u, stamp); err != nil {
			return err
		}
		query := "UPDATE sessions SET revoked_at=? WHERE user_id=? AND revoked_at IS NULL"
		args := []any{stamp, u.ID}
		if !reset {
			query += " AND id<>?"
			args = append(args, sessionID)
		}
		if _, err := v.Conn().ExecContext(v.Context(), query, args...); err != nil {
			return err
		}
		event.ResourceID = &u.ID
		if err := sqliteInsertAudit(v, event); err != nil {
			return err
		}
		result = u
		return nil
	})
	if err != nil {
		return User{}, err
	}
	return result, nil
}
