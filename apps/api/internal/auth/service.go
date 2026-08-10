package auth

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

type Role string

const (
	RoleOwner    Role = "owner"
	RoleOperator Role = "operator"
)

var (
	ErrBootstrapComplete = errors.New("bootstrap is already complete")
	ErrInvalidCredential = errors.New("invalid username or password")
	ErrInvalidSession    = errors.New("session is invalid or expired")
	ErrUnavailableUser   = errors.New("session user is unavailable")
	ErrUsernameInvalid   = errors.New("username must be 3-64 lowercase letters, digits, dot, underscore, or hyphen")
)

type User struct {
	ID           string    `json:"id"`
	Username     string    `json:"username"`
	PasswordHash string    `json:"-"`
	Role         Role      `json:"role"`
	Active       bool      `json:"active"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

type Session struct {
	ID         string     `json:"id"`
	UserID     string     `json:"userId"`
	TokenHash  string     `json:"-"`
	ExpiresAt  time.Time  `json:"expiresAt"`
	RevokedAt  *time.Time `json:"revokedAt,omitempty"`
	LastSeenAt *time.Time `json:"lastSeenAt,omitempty"`
	CreatedAt  time.Time  `json:"createdAt"`
}

type AuditEvent struct {
	ID           string    `json:"id"`
	ActorUserID  *string   `json:"actorUserId,omitempty"`
	Action       string    `json:"action"`
	ResourceType string    `json:"resourceType"`
	ResourceID   *string   `json:"resourceId,omitempty"`
	AfterValue   any       `json:"afterValue,omitempty"`
	Correlation  string    `json:"correlationId"`
	Result       string    `json:"result"`
	CreatedAt    time.Time `json:"createdAt"`
}

type Store interface {
	ListUsers(ctx context.Context) ([]User, error)
	FindUserByUsername(ctx context.Context, username string) (*User, error)
	FindUserByID(ctx context.Context, id string) (*User, error)
	CreateUser(ctx context.Context, user User) (User, error)
	UpdateUser(ctx context.Context, id string, patch UserPatch) (User, error)
	DeleteUser(ctx context.Context, id string) error
	CreateSession(ctx context.Context, session Session) (Session, error)
	FindSessionByTokenHash(ctx context.Context, tokenHash string) (*Session, error)
	RevokeSession(ctx context.Context, id string, revokedAt time.Time) error
	TouchSession(ctx context.Context, id string, lastSeenAt time.Time) error
	AddAudit(ctx context.Context, event AuditEvent) error
}

type AuthSession struct {
	Token     string    `json:"-"`
	UserID    string    `json:"userId"`
	ExpiresAt time.Time `json:"expiresAt"`
	User      User      `json:"user"`
}

type ServiceOptions struct {
	SessionTTL time.Duration
	Now        func() time.Time
}

type Service struct {
	store      Store
	sessionTTL time.Duration
	now        func() time.Time
}

func NewService(store Store, opts ServiceOptions) *Service {
	if opts.SessionTTL <= 0 {
		opts.SessionTTL = 8 * time.Hour
	}
	if opts.Now == nil {
		opts.Now = func() time.Time { return time.Now().UTC() }
	}
	return &Service{store: store, sessionTTL: opts.SessionTTL, now: opts.Now}
}

func (s *Service) Bootstrap(ctx context.Context, username, password string) (User, error) {
	users, err := s.store.ListUsers(ctx)
	if err != nil {
		return User{}, err
	}
	for _, user := range users {
		if user.Active {
			_ = s.audit(ctx, "bootstrap", "installation", nil, nil, "denied", nil)
			return User{}, ErrBootstrapComplete
		}
	}

	user, err := s.buildUser(username, password, RoleOwner)
	if err != nil {
		return User{}, err
	}
	created, err := s.store.CreateUser(ctx, user)
	if err != nil {
		return User{}, err
	}
	_ = s.audit(ctx, "bootstrap", "user", &created.ID, nil, "success", map[string]any{"role": created.Role})
	return created, nil
}

func (s *Service) Login(ctx context.Context, username, password string) (AuthSession, error) {
	normalized, err := NormalizeUsername(username)
	if err != nil {
		_ = s.audit(ctx, "login", "user", nil, nil, "failure", map[string]any{"username": strings.TrimSpace(strings.ToLower(username))})
		return AuthSession{}, ErrInvalidCredential
	}
	user, err := s.store.FindUserByUsername(ctx, normalized)
	if err != nil {
		return AuthSession{}, err
	}
	if user == nil || !user.Active || !VerifyPassword(password, user.PasswordHash) {
		var userID *string
		if user != nil {
			userID = &user.ID
		}
		_ = s.audit(ctx, "login", "user", userID, nil, "failure", map[string]any{"username": normalized})
		return AuthSession{}, ErrInvalidCredential
	}

	token, err := CreateOpaqueToken()
	if err != nil {
		return AuthSession{}, err
	}
	now := s.now().UTC()
	expiresAt := now.Add(s.sessionTTL)
	session, err := s.store.CreateSession(ctx, Session{
		ID:        newUUID(),
		UserID:    user.ID,
		TokenHash: HashOpaqueToken(token),
		ExpiresAt: expiresAt,
		CreatedAt: now,
	})
	if err != nil {
		return AuthSession{}, err
	}
	_ = s.audit(ctx, "login", "user", &user.ID, nil, "success", nil)
	return AuthSession{Token: token, UserID: user.ID, ExpiresAt: session.ExpiresAt, User: *user}, nil
}

func (s *Service) Authenticate(ctx context.Context, token string) (User, error) {
	if strings.TrimSpace(token) == "" {
		return User{}, ErrInvalidSession
	}
	session, err := s.store.FindSessionByTokenHash(ctx, HashOpaqueToken(token))
	if err != nil {
		return User{}, err
	}
	now := s.now().UTC()
	if session == nil || session.RevokedAt != nil || !session.ExpiresAt.After(now) {
		return User{}, ErrInvalidSession
	}
	user, err := s.store.FindUserByID(ctx, session.UserID)
	if err != nil {
		return User{}, err
	}
	if user == nil || !user.Active {
		return User{}, ErrUnavailableUser
	}
	if err := s.store.TouchSession(ctx, session.ID, now); err != nil {
		return User{}, err
	}
	return *user, nil
}

func (s *Service) Logout(ctx context.Context, token string) error {
	return s.Revoke(ctx, token)
}

func (s *Service) Revoke(ctx context.Context, token string) error {
	if strings.TrimSpace(token) == "" {
		return nil
	}
	session, err := s.store.FindSessionByTokenHash(ctx, HashOpaqueToken(token))
	if err != nil {
		return err
	}
	if session == nil || session.RevokedAt != nil {
		return nil
	}
	now := s.now().UTC()
	if err := s.store.RevokeSession(ctx, session.ID, now); err != nil {
		return err
	}
	_ = s.audit(ctx, "logout", "session", &session.ID, &session.UserID, "success", nil)
	return nil
}

func (s *Service) buildUser(username, password string, role Role) (User, error) {
	normalized, err := NormalizeUsername(username)
	if err != nil {
		return User{}, err
	}
	hash, err := HashPassword(password)
	if err != nil {
		return User{}, err
	}
	now := s.now().UTC()
	return User{
		ID:           newUUID(),
		Username:     normalized,
		PasswordHash: hash,
		Role:         role,
		Active:       true,
		CreatedAt:    now,
		UpdatedAt:    now,
	}, nil
}

func (s *Service) audit(ctx context.Context, action, resourceType string, resourceID, actorUserID *string, result string, afterValue any) error {
	return s.store.AddAudit(ctx, AuditEvent{
		ID:           newUUID(),
		ActorUserID:  actorUserID,
		Action:       action,
		ResourceType: resourceType,
		ResourceID:   resourceID,
		AfterValue:   afterValue,
		Correlation:  newUUID(),
		Result:       result,
		CreatedAt:    s.now().UTC(),
	})
}

var usernamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{2,63}$`)

func NormalizeUsername(username string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(username))
	if !usernamePattern.MatchString(normalized) {
		return "", ErrUsernameInvalid
	}
	return normalized, nil
}

func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("generate uuid: %v", err))
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
