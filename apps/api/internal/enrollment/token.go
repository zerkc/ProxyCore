package enrollment

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/auth"
	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
)

const (
	TokenHashVersion = configuration.EnrollmentTokenHashVersion
	DefaultTokenTTL  = 15 * time.Minute
	MinTokenTTL      = 5 * time.Minute
	MaxTokenTTL      = 60 * time.Minute
	tokenPrefix      = "pcenr1"
	tokenSelectorLen = 16
	tokenSecretLen   = 32
	tokenHashDomain  = "proxycore/enrollment-token/"
)

var (
	ErrOwnerRequired    = errors.New("active Owner authorization required")
	ErrInvalidTokenTTL  = errors.New("enrollment token expiry is outside the allowed range")
	ErrEnrollmentDenied = errors.New("enrollment token denied")
)

type CreatedToken struct {
	ID        string    `json:"id"`
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expiresAt"`
}

type Store interface {
	CreateEnrollmentToken(ctx context.Context, id, selector, hash, hashVersion, ownerID string, createdAt, expiresAt time.Time) (domain.TopologyRole, error)
	CheckEnrollmentToken(ctx context.Context, selector, hash string, now time.Time, consume bool) error
	RevokeEnrollmentToken(ctx context.Context, id, ownerID string, now time.Time) error
}

type Options struct {
	TTL      time.Duration
	Now      func() time.Time
	Identity IdentityActivation
}

type IdentityActivation interface {
	ActivateEnrollment(context.Context, func(context.Context) (domain.TopologyRole, error)) (domain.TopologyRole, error)
}

type Service struct {
	store    Store
	identity IdentityActivation
	ttl      time.Duration
	now      func() time.Time
}

func NewService(store Store, opts Options) *Service {
	if opts.TTL == 0 {
		opts.TTL = DefaultTokenTTL
	}
	if opts.Now == nil {
		opts.Now = func() time.Time { return time.Now().UTC() }
	}
	return &Service{store: store, identity: opts.Identity, ttl: opts.TTL, now: opts.Now}
}

func (s *Service) CreateToken(ctx context.Context, owner auth.User) (CreatedToken, error) {
	if owner.ID == "" || owner.Role != auth.RoleOwner || !owner.Active {
		return CreatedToken{}, ErrOwnerRequired
	}
	if s.ttl < MinTokenTTL || s.ttl > MaxTokenTTL {
		return CreatedToken{}, ErrInvalidTokenTTL
	}
	material := make([]byte, tokenSelectorLen+tokenSecretLen)
	if _, err := rand.Read(material); err != nil {
		return CreatedToken{}, fmt.Errorf("generate enrollment token: %w", err)
	}
	selector := hex.EncodeToString(material[:tokenSelectorLen])
	secret := material[tokenSelectorLen:]
	plaintext := tokenPrefix + "_" + selector + "_" + base64.RawURLEncoding.EncodeToString(secret)
	now := s.now().UTC()
	expiresAt := now.Add(s.ttl)
	id := uuid.NewString()
	create := func(ctx context.Context) (domain.TopologyRole, error) {
		return s.store.CreateEnrollmentToken(ctx, id, selector, hashSecret(secret), TokenHashVersion, owner.ID, now, expiresAt)
	}
	var err error
	if s.identity != nil {
		_, err = s.identity.ActivateEnrollment(ctx, create)
	} else {
		_, err = create(ctx)
	}
	if err != nil {
		return CreatedToken{}, err
	}
	return CreatedToken{ID: id, Token: plaintext, ExpiresAt: expiresAt}, nil
}

func (s *Service) VerifyToken(ctx context.Context, plaintext string) error {
	return s.checkToken(ctx, plaintext, false)
}

func (s *Service) ConsumeToken(ctx context.Context, plaintext string) error {
	return s.checkToken(ctx, plaintext, true)
}

func (s *Service) checkToken(ctx context.Context, plaintext string, consume bool) error {
	selector, secret, ok := parseToken(plaintext)
	if !ok {
		return ErrEnrollmentDenied
	}
	if err := s.store.CheckEnrollmentToken(ctx, selector, hashSecret(secret), s.now().UTC(), consume); err != nil {
		return ErrEnrollmentDenied
	}
	return nil
}

func (s *Service) RevokeToken(ctx context.Context, owner auth.User, id string) error {
	if owner.ID == "" || owner.Role != auth.RoleOwner || !owner.Active {
		return ErrOwnerRequired
	}
	if id == "" {
		return ErrEnrollmentDenied
	}
	return s.store.RevokeEnrollmentToken(ctx, id, owner.ID, s.now().UTC())
}

func parseToken(value string) (string, []byte, bool) {
	parts := strings.SplitN(value, "_", 3)
	if len(parts) != 3 || parts[0] != tokenPrefix {
		return "", nil, false
	}
	selector, err := hex.DecodeString(parts[1])
	if err != nil || len(selector) != tokenSelectorLen {
		return "", nil, false
	}
	secret, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(secret) != tokenSecretLen {
		return "", nil, false
	}
	return parts[1], secret, true
}

func hashSecret(secret []byte) string {
	hasher := sha256.New()
	_, _ = hasher.Write([]byte(tokenHashDomain))
	_, _ = hasher.Write(secret)
	return hex.EncodeToString(hasher.Sum(nil))
}
