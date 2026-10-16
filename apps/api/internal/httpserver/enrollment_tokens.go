package httpserver

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
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
	enrollmentTokenSelectorLen = 16
	enrollmentTokenSecretLen   = 32
	enrollmentTokenHashDomain  = "proxycore/enrollment-token/"
	maxEnrollmentTokenBody     = 8 << 10
)

var (
	ErrEnrollmentTokenAuthorityUnavailable = errors.New("enrollment token authority unavailable")
	ErrEnrollmentTokenRequest              = errors.New("invalid enrollment token request")
	ErrEnrollmentTokenRoute                = errors.New("enrollment token route cannot be embedded")
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

// EnrollmentTokenRoute is the public routing/trust hint embedded in a v2
// token. PrimaryURL must be canonical and CAPEM a normalized CA certificate.
type EnrollmentTokenRoute struct {
	PrimaryURL string
	CAPEM      string
}

type EnrollmentTokenLister func(context.Context, string) ([]EnrollmentTokenRecord, error)

type EnrollmentTokenAuthority interface {
	// Create mints a token. A non-nil route makes it a self-contained v2 token
	// carrying the primary URL and enrollment CA; nil mints a legacy v1 token.
	Create(context.Context, auth.User, *EnrollmentTokenRoute) (EnrollmentTokenCreation, error)
	List(context.Context, string) ([]EnrollmentTokenRecord, error)
	Revoke(context.Context, auth.User, string) error
	// Delete removes an inactive token that minted no node. It returns
	// enrollment.ErrTokenNotFound, ErrTokenActive or ErrTokenInUse.
	Delete(context.Context, auth.User, string) error
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

func (a *enrollmentTokenAuthority) Create(ctx context.Context, owner auth.User, route *EnrollmentTokenRoute) (EnrollmentTokenCreation, error) {
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
	plaintext := enrollment.TokenPrefixV1 + "_" + selector + "_" + base64.RawURLEncoding.EncodeToString(secret)
	if route != nil {
		encoded, err := enrollment.EncodeTokenV2(selector, secret, route.PrimaryURL, route.CAPEM)
		if err != nil {
			zeroBytes(secret)
			return EnrollmentTokenCreation{}, ErrEnrollmentTokenRoute
		}
		plaintext = encoded
	}
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

func (a *enrollmentTokenAuthority) Delete(ctx context.Context, owner auth.User, id string) error {
	if a == nil || a.store == nil || ctx == nil || owner.ID == "" || owner.Role != auth.RoleOwner || !owner.Active {
		return enrollment.ErrOwnerRequired
	}
	if _, err := uuid.Parse(id); err != nil {
		return enrollment.ErrTokenNotFound
	}
	return a.store.DeleteEnrollmentToken(ctx, id, owner.ID, a.now().UTC())
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

// parseHTTPEnrollmentToken accepts v1 and v2 tokens and returns only the
// selector and secret; the embedded URL/CA never take part in authentication.
func parseHTTPEnrollmentToken(value string) (string, []byte, bool) {
	claims, ok := enrollment.ParseTokenClaims(value)
	if !ok {
		return "", nil, false
	}
	return claims.Selector, claims.Secret, true
}

func (s *Server) handleCreateEnrollmentToken(w http.ResponseWriter, r *http.Request) {
	setNoStore(w)
	user, ok := s.requireUserReadOnly(w, r, auth.RoleOwner)
	if !ok {
		return
	}
	requestedURL, ok := decodeEnrollmentTokenCreateRequest(w, r)
	if !ok {
		return
	}
	if s.enrollmentTokens == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "enrollment tokens unavailable"})
		return
	}
	var route *EnrollmentTokenRoute
	if s.selfContainedTokens {
		resolved, failure := s.resolveEnrollmentTokenRoute(r.Context(), requestedURL)
		if failure != nil {
			writeConfigError(w, failure)
			return
		}
		route = &resolved
	}
	created, err := s.enrollmentTokens.Create(r.Context(), user, route)
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "enrollment token could not be created"})
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

const defaultEnrollmentTLSPort = 3443

// enrollmentTokenDefaults is the UI prefill for the token creation form. Empty
// string / false mean "unknown"; "not configured" is never an error.
type enrollmentTokenDefaults struct {
	PrimaryURL  string `json:"primaryUrl"`
	GatewayIPv4 string `json:"gatewayIPv4"`
	Port        int    `json:"port"`
	TrustReady  bool   `json:"trustReady"`
	HostCovered bool   `json:"hostCovered"`
}

func (s *Server) handleEnrollmentTokenDefaults(w http.ResponseWriter, r *http.Request) {
	setNoStore(w)
	if _, ok := s.requireUserReadOnly(w, r, auth.RoleOwner); !ok {
		return
	}
	defaults := enrollmentTokenDefaults{
		PrimaryURL:  s.defaultEnrollmentPrimaryURL(r.Context()),
		GatewayIPv4: s.gatewayIngress(r.Context()).IPv4,
		Port:        s.enrollmentTLSPort(),
	}
	if s.config != nil {
		if trust, err := s.config.LoadEnrollmentTLSTrust(r.Context()); err == nil && trust.Ready {
			defaults.TrustReady = true
			defaults.HostCovered = defaults.PrimaryURL != "" &&
				enrollmentLeafCoversURL(trust.Material.CertificatePEM, defaults.PrimaryURL)
		}
	}
	writeJSON(w, http.StatusOK, defaults)
}

// gatewayIngress prefers the advertised ingress persisted from the Ingress
// screen, then the startup default, then the environment. A settings read
// failure degrades to the latter two instead of failing the caller.
func (s *Server) gatewayIngress(ctx context.Context) domain.Ingress {
	var persisted domain.Ingress
	if s.config != nil {
		if settings, err := s.config.GetSettings(ctx); err == nil {
			persisted = settings.Ingress
		}
	}
	return domain.Ingress{
		IPv4: firstNonEmpty(persisted.IPv4, s.defaultIngress.IPv4, s.cfg.ProxyIngressIPv4),
		IPv6: firstNonEmpty(persisted.IPv6, s.defaultIngress.IPv6, s.cfg.ProxyIngressIPv6),
	}
}

// enrollmentTLSPort is the enrollment listener's port, falling back to the
// documented default when the address carries none.
func (s *Server) enrollmentTLSPort() int {
	_, portText, err := net.SplitHostPort(strings.TrimSpace(s.cfg.EnrollmentTLSAddr))
	if err != nil {
		return defaultEnrollmentTLSPort
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port <= 0 || port > 65535 {
		return defaultEnrollmentTLSPort
	}
	return port
}

// defaultEnrollmentPrimaryURL is https://<gateway IPv4>:<port>/ (IPv6 only
// when there is no IPv4), or "" when no gateway address is configured.
func (s *Server) defaultEnrollmentPrimaryURL(ctx context.Context) string {
	ingress := s.gatewayIngress(ctx)
	host := ingress.IPv4
	if host == "" {
		host = ingress.IPv6
	}
	if host == "" {
		return ""
	}
	canonical, err := enrollment.CanonicalizePrimaryURL("https://" + net.JoinHostPort(host, strconv.Itoa(s.enrollmentTLSPort())) + "/")
	if err != nil {
		return ""
	}
	return canonical
}

// resolveEnrollmentTokenRoute decides what a new v2 token embeds. Nothing is
// minted unless the enrollment CA is ready and the URL's host is covered by
// the enrollment leaf, because a join against it would fail TLS otherwise.
func (s *Server) resolveEnrollmentTokenRoute(ctx context.Context, requestedURL string) (EnrollmentTokenRoute, *httpError) {
	if s.config == nil {
		return EnrollmentTokenRoute{}, &httpError{status: http.StatusServiceUnavailable, message: "configuration store is not configured"}
	}
	trust, err := s.config.LoadEnrollmentTLSTrust(ctx)
	if err != nil {
		return EnrollmentTokenRoute{}, &httpError{status: http.StatusServiceUnavailable, message: enrollmentTrustUnavailableMessage, code: "ENROLLMENT_TRUST_UNAVAILABLE"}
	}
	if !trust.Ready {
		return EnrollmentTokenRoute{}, &httpError{status: http.StatusConflict, message: enrollmentTrustNotReadyMessage, code: "ENROLLMENT_TRUST_NOT_READY"}
	}
	rawURL := strings.TrimSpace(requestedURL)
	if rawURL == "" {
		rawURL = s.defaultEnrollmentPrimaryURL(ctx)
	}
	if rawURL == "" {
		return EnrollmentTokenRoute{}, &httpError{status: http.StatusUnprocessableEntity, message: "a primary URL is required", code: "PRIMARY_URL_REQUIRED"}
	}
	canonical, err := enrollment.CanonicalizePrimaryURL(rawURL)
	if err != nil {
		return EnrollmentTokenRoute{}, &httpError{status: http.StatusUnprocessableEntity, message: "primary URL must be an https origin with host and port", code: "PRIMARY_URL_INVALID"}
	}
	if !enrollmentLeafCoversURL(trust.Material.CertificatePEM, canonical) {
		return EnrollmentTokenRoute{}, &httpError{status: http.StatusUnprocessableEntity,
			message: "primary URL host is not in the enrollment certificate; add it to the enrollment hostnames first",
			code:    "PRIMARY_URL_NOT_IN_CERT"}
	}
	return EnrollmentTokenRoute{PrimaryURL: canonical, CAPEM: trust.Material.CACertificatePEM}, nil
}

// enrollmentLeafCoversURL reports whether the leaf certificate is valid for the
// URL's host (DNS names and IP SANs alike).
func enrollmentLeafCoversURL(leafPEM, primaryURL string) bool {
	parsed, err := url.Parse(primaryURL)
	if err != nil || parsed.Hostname() == "" {
		return false
	}
	block, _ := pem.Decode([]byte(leafPEM))
	if block == nil || block.Type != "CERTIFICATE" {
		return false
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return false
	}
	return leaf.VerifyHostname(parsed.Hostname()) == nil
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

// handleDeleteEnrollmentToken removes an inactive token that minted no node.
// Active tokens must be revoked first; tokens with a grant stay because the
// grant still authenticates the node they enrolled.
func (s *Server) handleDeleteEnrollmentToken(w http.ResponseWriter, r *http.Request) {
	setNoStore(w)
	user, ok := s.requireUserReadOnly(w, r, auth.RoleOwner)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if _, err := uuid.Parse(id); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "enrollment token not found"})
		return
	}
	if s.enrollmentTokens == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "enrollment tokens unavailable"})
		return
	}
	switch err := s.enrollmentTokens.Delete(r.Context(), user, id); {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, enrollment.ErrTokenNotFound):
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "enrollment token not found"})
	case errors.Is(err, enrollment.ErrTokenActive):
		writeJSON(w, http.StatusConflict, map[string]any{"error": "enrollment token is active; revoke it first", "code": "TOKEN_ACTIVE"})
	case errors.Is(err, enrollment.ErrTokenInUse):
		writeJSON(w, http.StatusConflict, map[string]any{"error": "enrollment token minted a node; manage it from the Nodes view", "code": "TOKEN_IN_USE"})
	default:
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "enrollment tokens unavailable"})
	}
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

// decodeEnrollmentTokenCreateRequest accepts exactly a JSON object that is
// either empty or carries only an optional string "primaryUrl".
func decodeEnrollmentTokenCreateRequest(w http.ResponseWriter, r *http.Request) (string, bool) {
	reject := func() (string, bool) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid enrollment token request"})
		return "", false
	}
	if r.ContentLength > maxEnrollmentTokenBody {
		return reject()
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxEnrollmentTokenBody))
	var body map[string]json.RawMessage
	if err := decoder.Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		return reject()
	}
	if body == nil {
		return reject()
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return reject()
	}
	requested := ""
	for key, value := range body {
		if key != "primaryUrl" {
			return reject()
		}
		if err := json.Unmarshal(value, &requested); err != nil {
			return reject()
		}
	}
	return requested, true
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
