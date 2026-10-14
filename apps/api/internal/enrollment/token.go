package enrollment

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	tokenPrefix      = TokenPrefixV1
	tokenSelectorLen = 16
	tokenSecretLen   = 32
	tokenHashDomain  = "proxycore/enrollment-token/"
)

var (
	ErrOwnerRequired    = errors.New("active Owner authorization required")
	ErrInvalidTokenTTL  = errors.New("enrollment token expiry is outside the allowed range")
	ErrEnrollmentDenied = errors.New("enrollment token denied")

	ErrTokenNotFound = configuration.ErrEnrollmentTokenNotFound
	ErrTokenActive   = configuration.ErrEnrollmentTokenActive
	ErrTokenInUse    = configuration.ErrEnrollmentTokenInUse
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
	// DeleteEnrollmentToken removes an inactive token without a grant. It
	// returns ErrTokenNotFound, ErrTokenActive or ErrTokenInUse.
	DeleteEnrollmentToken(ctx context.Context, id, ownerID string, now time.Time) error
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

func (s *Service) DeleteToken(ctx context.Context, owner auth.User, id string) error {
	if owner.ID == "" || owner.Role != auth.RoleOwner || !owner.Active {
		return ErrOwnerRequired
	}
	if id == "" {
		return ErrTokenNotFound
	}
	return s.store.DeleteEnrollmentToken(ctx, id, owner.ID, s.now().UTC())
}

func parseToken(value string) (string, []byte, bool) {
	claims, ok := ParseTokenClaims(value)
	if !ok {
		return "", nil, false
	}
	return claims.Selector, claims.Secret, true
}

func hashSecret(secret []byte) string {
	hasher := sha256.New()
	_, _ = hasher.Write([]byte(tokenHashDomain))
	_, _ = hasher.Write(secret)
	return hex.EncodeToString(hasher.Sum(nil))
}

const (
	// TokenPrefixV1 is the legacy token: pcenr1_<hex selector>_<b64url secret>.
	TokenPrefixV1 = "pcenr1"
	// TokenPrefixV2 is the self-contained token: pcenr2_<b64url JSON payload>.
	// The payload carries selector+secret (authenticated server-side exactly as
	// v1) plus the primary's canonical URL and enrollment CA certificate, which
	// are routing/trust hints the joining node validates itself.
	TokenPrefixV2 = "pcenr2"
	// MaxTokenV2PayloadBytes bounds the decoded JSON payload.
	MaxTokenV2PayloadBytes = 4 << 10
	// MaxTokenV2Bytes bounds the whole v2 token text (prefix + b64url payload).
	MaxTokenV2Bytes = len(TokenPrefixV2) + 1 + (MaxTokenV2PayloadBytes*4+2)/3 + 1

	tokenV2Version = 2
)

var errTokenV2Material = errors.New("invalid enrollment token material")

// TokenClaims is the parsed content of an enrollment token of either version.
// PrimaryURL and CAPEM are empty for v1 tokens.
type TokenClaims struct {
	Version    int
	Selector   string
	Secret     []byte
	PrimaryURL string
	CAPEM      string
}

// LegacyWire returns the v1 spelling of the same selector+secret, which is all
// the primary needs; the URL and CA stay on the node.
func (c TokenClaims) LegacyWire() string {
	return TokenPrefixV1 + "_" + c.Selector + "_" + base64.RawURLEncoding.EncodeToString(c.Secret)
}

type tokenV2Payload struct {
	Version    int    `json:"v"`
	Selector   string `json:"selector"`
	Secret     string `json:"secret"`
	PrimaryURL string `json:"primaryUrl"`
	CAPEM      string `json:"caPem"`
}

// IsTokenCredential reports whether value is a well-formed enrollment token of
// any supported version. It is the one place callers use to tell a token from
// another credential, so no code path hard-codes a single prefix.
func IsTokenCredential(value string) bool {
	claims, ok := ParseTokenClaims(value)
	zeroBytes(claims.Secret)
	return ok
}

// ParseTokenClaims parses a v1 or v2 token. Every malformed, oversized or
// unknown-version input is rejected without detail.
func ParseTokenClaims(value string) (TokenClaims, bool) {
	prefix, rest, found := strings.Cut(value, "_")
	if !found || rest == "" {
		return TokenClaims{}, false
	}
	switch prefix {
	case TokenPrefixV1:
		selector, secret, ok := parseSelectorAndSecret(rest)
		if !ok {
			return TokenClaims{}, false
		}
		return TokenClaims{Version: 1, Selector: selector, Secret: secret}, true
	case TokenPrefixV2:
		return parseTokenV2(rest)
	default:
		return TokenClaims{}, false
	}
}

func parseSelectorAndSecret(rest string) (string, []byte, bool) {
	selectorText, secretText, found := strings.Cut(rest, "_")
	if !found {
		return "", nil, false
	}
	secret, err := base64.RawURLEncoding.DecodeString(secretText)
	if err != nil || len(secret) != tokenSecretLen || !validTokenSelector(selectorText) {
		zeroBytes(secret)
		return "", nil, false
	}
	return selectorText, secret, true
}

func validTokenSelector(selector string) bool {
	decoded, err := hex.DecodeString(selector)
	return err == nil && len(decoded) == tokenSelectorLen && hex.EncodeToString(decoded) == selector
}

func parseTokenV2(rest string) (TokenClaims, bool) {
	if len(rest) > MaxTokenV2Bytes {
		return TokenClaims{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(rest)
	if err != nil || len(raw) == 0 || len(raw) > MaxTokenV2PayloadBytes {
		return TokenClaims{}, false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var payload tokenV2Payload
	if err := decoder.Decode(&payload); err != nil {
		return TokenClaims{}, false
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return TokenClaims{}, false
	}
	if payload.Version != tokenV2Version || !validTokenSelector(payload.Selector) {
		return TokenClaims{}, false
	}
	secret, err := base64.RawURLEncoding.DecodeString(payload.Secret)
	if err != nil || len(secret) != tokenSecretLen {
		zeroBytes(secret)
		return TokenClaims{}, false
	}
	canonicalURL, err := CanonicalizePrimaryURL(payload.PrimaryURL)
	if err != nil || canonicalURL != payload.PrimaryURL {
		zeroBytes(secret)
		return TokenClaims{}, false
	}
	// The CA must already be in its normalized single-certificate form, so the
	// node and the primary agree on the exact bytes that become the anchor.
	normalized, _, err := configuration.NormalizePrimaryTrustCA(payload.CAPEM)
	if err != nil || normalized != payload.CAPEM {
		zeroBytes(secret)
		return TokenClaims{}, false
	}
	return TokenClaims{
		Version: 2, Selector: payload.Selector, Secret: secret,
		PrimaryURL: payload.PrimaryURL, CAPEM: payload.CAPEM,
	}, true
}

// EncodeTokenV2 builds a v2 token. primaryURL must already be canonical and
// caPEM is normalized here; the whole payload is bounded.
func EncodeTokenV2(selector string, secret []byte, primaryURL, caPEM string) (string, error) {
	if !validTokenSelector(selector) || len(secret) != tokenSecretLen {
		return "", errTokenV2Material
	}
	canonicalURL, err := CanonicalizePrimaryURL(primaryURL)
	if err != nil || canonicalURL != primaryURL {
		return "", errTokenV2Material
	}
	normalized, _, err := configuration.NormalizePrimaryTrustCA(caPEM)
	if err != nil {
		return "", errTokenV2Material
	}
	raw, err := json.Marshal(tokenV2Payload{
		Version: tokenV2Version, Selector: selector, Secret: base64.RawURLEncoding.EncodeToString(secret),
		PrimaryURL: primaryURL, CAPEM: normalized,
	})
	if err != nil || len(raw) > MaxTokenV2PayloadBytes {
		return "", errTokenV2Material
	}
	return TokenPrefixV2 + "_" + base64.RawURLEncoding.EncodeToString(raw), nil
}
