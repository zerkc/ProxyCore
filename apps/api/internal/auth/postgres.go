package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type PostgresStore struct {
	pool *pgxpool.Pool
}

func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{pool: pool}
}

func (s *PostgresStore) EnsureSchema(ctx context.Context) error {
	statements := []string{
		`do $$ begin create type proxycore_role as enum ('owner', 'operator'); exception when duplicate_object then null; end $$;`,
		`create table if not exists users (
			id uuid primary key,
			username text not null,
			password_hash text not null,
			role proxycore_role not null,
			active boolean not null default true,
			created_at timestamptz not null default now(),
			updated_at timestamptz not null default now()
		);`,
		`create unique index if not exists users_username_idx on users (username);`,
		`create table if not exists sessions (
			id uuid primary key,
			user_id uuid not null references users(id) on delete cascade,
			token_hash text not null,
			expires_at timestamptz not null,
			revoked_at timestamptz,
			last_seen_at timestamptz,
			created_at timestamptz not null default now()
		);`,
		`create unique index if not exists sessions_token_hash_idx on sessions (token_hash);`,
		`create table if not exists audit_events (
			id uuid primary key,
			actor_user_id uuid references users(id),
			action text not null,
			resource_type text not null,
			resource_id text,
			before_value jsonb,
			after_value jsonb,
			correlation_id text not null,
			result text not null,
			created_at timestamptz not null default now()
		);`,
	}
	for _, statement := range statements {
		if _, err := s.pool.Exec(ctx, statement); err != nil {
			return fmt.Errorf("ensure auth schema: %w", err)
		}
	}
	return nil
}

func (s *PostgresStore) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.pool.Query(ctx, `
		select id::text, username, password_hash, role::text, active, created_at, updated_at
		from users
		order by username
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	users := []User{}
	for rows.Next() {
		user, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		users = append(users, user)
	}
	return users, rows.Err()
}

func (s *PostgresStore) FindUserByUsername(ctx context.Context, username string) (*User, error) {
	row := s.pool.QueryRow(ctx, `
		select id::text, username, password_hash, role::text, active, created_at, updated_at
		from users
		where username = $1
	`, username)
	return scanUserPtr(row)
}

func (s *PostgresStore) FindUserByID(ctx context.Context, id string) (*User, error) {
	row := s.pool.QueryRow(ctx, `
		select id::text, username, password_hash, role::text, active, created_at, updated_at
		from users
		where id = $1
	`, id)
	return scanUserPtr(row)
}

func (s *PostgresStore) CreateUser(ctx context.Context, user User) (User, error) {
	row := s.pool.QueryRow(ctx, `
		insert into users (id, username, password_hash, role, active, created_at, updated_at)
		values ($1, $2, $3, $4::proxycore_role, $5, $6, $7)
		returning id::text, username, password_hash, role::text, active, created_at, updated_at
	`, user.ID, user.Username, user.PasswordHash, string(user.Role), user.Active, user.CreatedAt, user.UpdatedAt)
	return scanUser(row)
}

func (s *PostgresStore) UpdateUser(ctx context.Context, id string, patch UserPatch) (User, error) {
	setClauses := []string{"updated_at = now()"}
	args := []any{id}
	next := 2
	if patch.Role != nil {
		setClauses = append(setClauses, fmt.Sprintf("role = $%d::proxycore_role", next))
		args = append(args, string(*patch.Role))
		next++
	}
	if patch.Active != nil {
		setClauses = append(setClauses, fmt.Sprintf("active = $%d", next))
		args = append(args, *patch.Active)
		next++
	}
	if patch.PasswordHash != nil {
		setClauses = append(setClauses, fmt.Sprintf("password_hash = $%d", next))
		args = append(args, *patch.PasswordHash)
		next++
	}
	query := "update users set " + strings.Join(setClauses, ", ") +
		" where id = $1 returning id::text, username, password_hash, role::text, active, created_at, updated_at"
	row := s.pool.QueryRow(ctx, query, args...)
	user, err := scanUser(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrUserNotFound
	}
	if err != nil {
		return User{}, err
	}
	return user, nil
}

func (s *PostgresStore) DeleteUser(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `delete from users where id = $1`, id)
	return err
}

func (s *PostgresStore) CreateSession(ctx context.Context, session Session) (Session, error) {
	row := s.pool.QueryRow(ctx, `
		insert into sessions (id, user_id, token_hash, expires_at, revoked_at, last_seen_at, created_at)
		values ($1, $2, $3, $4, $5, $6, $7)
		returning id::text, user_id::text, token_hash, expires_at, revoked_at, last_seen_at, created_at
	`, session.ID, session.UserID, session.TokenHash, session.ExpiresAt, session.RevokedAt, session.LastSeenAt, session.CreatedAt)
	return scanSession(row)
}

func (s *PostgresStore) FindSessionByTokenHash(ctx context.Context, tokenHash string) (*Session, error) {
	row := s.pool.QueryRow(ctx, `
		select id::text, user_id::text, token_hash, expires_at, revoked_at, last_seen_at, created_at
		from sessions
		where token_hash = $1 and revoked_at is null
	`, tokenHash)
	return scanSessionPtr(row)
}

func (s *PostgresStore) RevokeSession(ctx context.Context, id string, revokedAt time.Time) error {
	_, err := s.pool.Exec(ctx, `update sessions set revoked_at = $2 where id = $1`, id, revokedAt)
	return err
}

func (s *PostgresStore) TouchSession(ctx context.Context, id string, lastSeenAt time.Time) error {
	_, err := s.pool.Exec(ctx, `update sessions set last_seen_at = $2 where id = $1`, id, lastSeenAt)
	return err
}

func (s *PostgresStore) AddAudit(ctx context.Context, event AuditEvent) error {
	var afterValue *string
	if event.AfterValue != nil {
		serialized, err := json.Marshal(event.AfterValue)
		if err != nil {
			return fmt.Errorf("marshal audit after_value: %w", err)
		}
		value := string(serialized)
		afterValue = &value
	}
	_, err := s.pool.Exec(ctx, `
		insert into audit_events (
			id, actor_user_id, action, resource_type, resource_id, before_value,
			after_value, correlation_id, result, created_at
		)
		values ($1, $2, $3, $4, $5, null, $6::jsonb, $7, $8, $9)
	`, event.ID, event.ActorUserID, event.Action, event.ResourceType, event.ResourceID, afterValue, event.Correlation, event.Result, event.CreatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "42P01" {
			return nil
		}
		return err
	}
	return nil
}

type scanner interface {
	Scan(dest ...any) error
}

func scanUser(row scanner) (User, error) {
	var user User
	var role string
	if err := row.Scan(&user.ID, &user.Username, &user.PasswordHash, &role, &user.Active, &user.CreatedAt, &user.UpdatedAt); err != nil {
		return User{}, err
	}
	user.Role = Role(role)
	return user, nil
}

func scanUserPtr(row scanner) (*User, error) {
	user, err := scanUser(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &user, nil
}

func scanSession(row scanner) (Session, error) {
	var session Session
	if err := row.Scan(
		&session.ID,
		&session.UserID,
		&session.TokenHash,
		&session.ExpiresAt,
		&session.RevokedAt,
		&session.LastSeenAt,
		&session.CreatedAt,
	); err != nil {
		return Session{}, err
	}
	return session, nil
}

func scanSessionPtr(row scanner) (*Session, error) {
	session, err := scanSession(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &session, nil
}
