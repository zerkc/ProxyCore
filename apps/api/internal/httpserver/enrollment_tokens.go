package httpserver

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/auth"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/enrollment"
	replicationsnapshot "github.com/zerkc/ProxyCore/apps/api/internal/snapshot"
)

const (
	EnrollmentTokensPath       = "/api/enrollment/tokens"
	EnrollmentTokenConfirmText = "replace-and-revoke"
	enrollmentTokenPrefix      = "pcenr1"
	enrollmentTokenSelectorLen = 16
	enrollmentTokenSecretLen   = 32
	enrollmentTokenHashDomain  = "proxycore/enrollment-token/"
	maxEnrollmentTokenBody     = 8 << 10
)

var (
	ErrEnrollmentTokenAuthorityUnavailable = errors.New("enrollment token authority unavailable")
	ErrEnrollmentTokenRequest              = errors.New("invalid enrollment token request")
)

// EnrollmentTokenRecord is the complete non-secret projection returned by the
// Owner list route. It intentionally has no token hash or plaintext field.
type EnrollmentTokenRecord struct {
	ID            string
	Selector      string
	CreatedAt     time.Time
	ExpiresAt     time.Time
	ConsumedAt    *time.Time
	RevokedAt     *time.Time
	AttemptCount  int
	LastAttemptAt *time.Time
}

// EnrollmentTokenCreation is the one-time creation projection. Token is only
// serialized by POST /api/enrollment/tokens and never by list/revoke paths.
type EnrollmentTokenCreation struct {
	ID        string
	Selector  string
	Token     string
	ExpiresAt time.Time
}

type EnrollmentTokenLister func(context.Context, string) ([]EnrollmentTokenRecord, error)

type EnrollmentTokenAuthority interface {
	Create(context.Context, auth.User) (EnrollmentTokenCreation, error)
	List(context.Context, string) ([]EnrollmentTokenRecord, error)
	Revoke(context.Context, auth.User, string) error
	Verify(context.Context, string) error
}

type EnrollmentTokenAuthorityOptions struct {
	List EnrollmentTokenLister
	TTL  time.Duration
	Now  func() time.Time
}

// NewEnrollmentTokenAuthority binds the existing enrollment token store to
// the HTTP projections. Persistence remains in configuration; this adapter
// only owns bounded wire generation, selector parsing, and redaction.
func NewEnrollmentTokenAuthority(store enrollment.Store, identity enrollment.IdentityActivation, opts EnrollmentTokenAuthorityOptions) EnrollmentTokenAuthority {
	if opts.TTL <= 0 {
		opts.TTL = replicationsnapshot.ArchiveTTLDefault
	}
	if opts.Now == nil {
		opts.Now = func() time.Time { return time.Now().UTC() }
	}
	return &enrollmentTokenAuthority{store: store, identity: identity, list: opts.List, ttl: opts.TTL, now: opts.Now}
}

type enrollmentTokenAuthority struct {
	store    enrollment.Store
	identity enrollment.IdentityActivation
	list     EnrollmentTokenLister
	ttl      time.Duration
	now      func() time.Time
}

func (a *enrollmentTokenAuthority) Create(ctx context.Context, owner auth.User) (EnrollmentTokenCreation, error) {
	if a == nil || a.store == nil || ctx == nil || owner.ID == "" || owner.Role != auth.RoleOwner || !owner.Active {
		return EnrollmentTokenCreation{}, enrollment.ErrOwnerRequired
	}
	if a.ttl <= 0 {
		return EnrollmentTokenCreation{}, ErrEnrollmentTokenAuthorityUnavailable
	}
	var material [enrollmentTokenSelectorLen + enrollmentTokenSecretLen]byte
	if _, err := rand.Read(material[:]); err != nil {
		return EnrollmentTokenCreation{}, ErrEnrollmentTokenAuthorityUnavailable
	}
	selector := hex.EncodeToString(material[:enrollmentTokenSelectorLen])
	secret := material[enrollmentTokenSelectorLen:]
	hash := hashEnrollmentTokenSecret(secret)
	plaintext := enrollmentTokenPrefix + "_" + selector + "_" + base64.RawURLEncoding.EncodeToString(secret)
	zeroBytes(secret)

	now := a.now().UTC()
	expiresAt := now.Add(a.ttl)
	id := uuid.NewString()
	create := func(runCtx context.Context) (domain.TopologyRole, error) {
		return a.store.CreateEnrollmentToken(runCtx, id, selector, hash, enrollment.TokenHashVersion, owner.ID, now, expiresAt)
	}
	var err error
	if a.identity != nil {
		_, err = a.identity.ActivateEnrollment(ctx, create)
	} else {
		_, err = create(ctx)
	}
	if err != nil {
		return EnrollmentTokenCreation{}, err
	}
	return EnrollmentTokenCreation{ID: id, Selector: selector, Token: plaintext, ExpiresAt: expiresAt}, nil
}

func (a *enrollmentTokenAuthority) List(ctx context.Context, ownerID string) ([]EnrollmentTokenRecord, error) {
	if a == nil || a.list == nil || ctx == nil || strings.TrimSpace(ownerID) == "" {
		return nil, ErrEnrollmentTokenAuthorityUnavailable
	}
	return a.list(ctx, ownerID)
}

func (a *enrollmentTokenAuthority) Revoke(ctx context.Context, owner auth.User, id string) error {
	if a == nil || a.store == nil || ctx == nil || owner.ID == "" || owner.Role != auth.RoleOwner || !owner.Active {
		return enrollment.ErrOwnerRequired
	}
	if _, err := uuid.Parse(id); err != nil {
		return enrollment.ErrEnrollmentDenied
	}
	return a.store.RevokeEnrollmentToken(ctx, id, owner.ID, a.now().UTC())
}

func (a *enrollmentTokenAuthority) Verify(ctx context.Context, plaintext string) error {
	if a == nil || a.store == nil || ctx == nil {
		return enrollment.ErrEnrollmentDenied
	}
	selector, secret, ok := parseHTTPEnrollmentToken(plaintext)
	if !ok {
		return enrollment.ErrEnrollmentDenied
	}
	defer zeroBytes(secret)
	if err := a.store.CheckEnrollmentToken(ctx, selector, hashEnrollmentTokenSecret(secret), a.now().UTC(), false); err != nil {
		return enrollment.ErrEnrollmentDenied
	}
	return nil
}

func hashEnrollmentTokenSecret(secret []byte) string {
	hasher := sha256.New()
	_, _ = hasher.Write([]byte(enrollmentTokenHashDomain))
	_, _ = hasher.Write(secret)
	return hex.EncodeToString(hasher.Sum(nil))
}

func parseHTTPEnrollmentToken(value string) (string, []byte, bool) {
	parts := strings.SplitN(value, "_", 3)
	if len(parts) != 3 || parts[0] != enrollmentTokenPrefix || parts[1] == "" {
		return "", nil, false
	}
	selectorBytes, err := hex.DecodeString(parts[1])
	if err != nil || len(selectorBytes) != enrollmentTokenSelectorLen || hex.EncodeToString(selectorBytes) != parts[1] {
		return "", nil, false
	}
	secret, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(secret) != enrollmentTokenSecretLen {
		return "", nil, false
	}
	return parts[1], secret, true
}

func (s *Server) handleCreateEnrollmentToken(w http.ResponseWriter, r *http.Request) {
	setNoStore(w)
	user, ok := s.requireUserReadOnly(w, r, auth.RoleOwner)
	if !ok {
		return
	}
	if !decodeEmptyEnrollmentObject(w, r) {
		return
	}
	if s.enrollmentTokens == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "enrollment tokens unavailable"})
		return
	}
	created, err := s.enrollmentTokens.Create(r.Context(), user)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "enrollment token could not be created"})
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) handleListEnrollmentTokens(w http.ResponseWriter, r *http.Request) {
	setNoStore(w)
	user, ok := s.requireUserReadOnly(w, r, auth.RoleOwner)
	if !ok {
		return
	}
	if s.enrollmentTokens == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "enrollment tokens unavailable"})
		return
	}
	tokens, err := s.enrollmentTokens.List(r.Context(), user.ID)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "enrollment tokens unavailable"})
		return
	}
	response := make([]enrollmentTokenListResponse, 0, len(tokens))
	for _, token := range tokens {
		response = append(response, enrollmentTokenListResponse{
			ID: token.ID, Selector: token.Selector, CreatedAt: token.CreatedAt, ExpiresAt: token.ExpiresAt,
			ConsumedAt: token.ConsumedAt, RevokedAt: token.RevokedAt,
			AttemptSummary: enrollmentTokenAttemptSummary{Count: token.AttemptCount, LastAttemptAt: token.LastAttemptAt},
		})
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleRevokeEnrollmentToken(w http.ResponseWriter, r *http.Request) {
	setNoStore(w)
	user, ok := s.requireUserReadOnly(w, r, auth.RoleOwner)
	if !ok {
		return
	}
	if r.URL.Query().Get("confirm") != EnrollmentTokenConfirmText {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "explicit confirmation required"})
		return
	}
	if s.enrollmentTokens == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "enrollment tokens unavailable"})
		return
	}
	if err := s.enrollmentTokens.Revoke(r.Context(), user, r.PathValue("id")); err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "enrollment token could not be revoked"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type enrollmentTokenListResponse struct {
	ID             string                        `json:"id"`
	Selector       string                        `json:"selector"`
	CreatedAt      time.Time                     `json:"createdAt"`
	ExpiresAt      time.Time                     `json:"expiresAt"`
	ConsumedAt     *time.Time                    `json:"consumedAt,omitempty"`
	RevokedAt      *time.Time                    `json:"revokedAt,omitempty"`
	AttemptSummary enrollmentTokenAttemptSummary `json:"attemptSummary"`
}

type enrollmentTokenAttemptSummary struct {
	Count         int        `json:"count"`
	LastAttemptAt *time.Time `json:"lastAttemptAt,omitempty"`
}

func decodeEmptyEnrollmentObject(w http.ResponseWriter, r *http.Request) bool {
	if r.ContentLength > maxEnrollmentTokenBody {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid enrollment token request"})
		return false
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxEnrollmentTokenBody))
	var body map[string]json.RawMessage
	if err := decoder.Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid enrollment token request"})
		return false
	}
	if body == nil || len(body) != 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid enrollment token request"})
		return false
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid enrollment token request"})
		return false
	}
	return true
}

func setNoStore(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
}

func zeroBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}

var _ EnrollmentTokenAuthority = (*enrollmentTokenAuthority)(nil)
