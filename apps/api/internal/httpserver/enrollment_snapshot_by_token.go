package httpserver

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/identity"
)

const (
	EnrollmentSnapshotByTokenPath = "/api/topology/sync/snapshot-by-token"
	maxSnapshotByTokenBodyBytes   = 8 << 10
)

var (
	ErrSnapshotByTokenInvalidRequest  = errors.New("snapshot by token invalid request")
	ErrSnapshotByTokenUnauthenticated = errors.New("snapshot by token unauthenticated")
	ErrSnapshotByTokenUnavailable     = errors.New("snapshot by token unavailable")
	ErrSnapshotByTokenNotReady        = errors.New("snapshot by token not ready")
	ErrSnapshotByTokenGone            = errors.New("snapshot by token gone")
	ErrSnapshotByTokenDenied          = errors.New("snapshot by token denied")

	// ErrNoPublishableSnapshot is the store-to-handler readiness sentinel. It
	// is deliberately separate from the HTTP response sentinel so a future
	// PostgreSQL store can report readiness without knowing HTTP status codes.
	ErrNoPublishableSnapshot = errors.New("snapshot by token has no publishable snapshot")
)

// SnapshotByTokenStore is the production-shaped seam for the token-authorized
// publication read. PNE-6.7 supplies the PostgreSQL implementation. The
// selector is canonical and the identity record is the loaded PRIMARY view;
// neither contains token or key bytes.
type SnapshotByTokenStore interface {
	ReadSnapshotPublicationByToken(context.Context, string, configuration.SnapshotPublicationIdentityRecord) ([]byte, error)
}

// SnapshotByTokenIdentitySource returns the cached identity and whether it was
// loaded. Implementations must fail closed rather than panic before startup.
type SnapshotByTokenIdentitySource interface {
	Current() (identity.Identity, bool)
}

// SnapshotByTokenKeyMaterial is a short-lived decryptability gate. The
// handler never stores or returns the copied bytes; the production store will
// own its transaction-bound key read when PNE-6.7 lands.
type SnapshotByTokenKeyMaterial interface {
	CopyBytes() []byte
	Destroy()
}

type EnrollmentSnapshotByTokenHandlerOptions struct {
	Store            SnapshotByTokenStore
	Identity         SnapshotByTokenIdentitySource
	Certificate      IdentityProofCertificateProvider
	ParseToken       func(string) (selector, secret string, ok bool)
	VerifyToken      func(context.Context, string) error
	ClusterKeyLoader func(context.Context) (SnapshotByTokenKeyMaterial, error)
	Now              func() time.Time
}

// EnrollmentSnapshotByTokenHandler serves the one-shot NODE enrollment read
// over the dedicated TLS listener. It does not mint or persist credentials.
type EnrollmentSnapshotByTokenHandler struct {
	store            SnapshotByTokenStore
	identity         SnapshotByTokenIdentitySource
	certificate      IdentityProofCertificateProvider
	parseToken       func(string) (selector, secret string, ok bool)
	verifyToken      func(context.Context, string) error
	clusterKeyLoader func(context.Context) (SnapshotByTokenKeyMaterial, error)
	now              func() time.Time
}

func NewEnrollmentSnapshotByTokenHandler(opts EnrollmentSnapshotByTokenHandlerOptions) *EnrollmentSnapshotByTokenHandler {
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &EnrollmentSnapshotByTokenHandler{
		store: opts.Store, identity: opts.Identity, certificate: opts.Certificate,
		parseToken: opts.ParseToken, verifyToken: opts.VerifyToken,
		clusterKeyLoader: opts.ClusterKeyLoader, now: now,
	}
}

// NewEnrollmentSnapshotByTokenMux places the token route in front of the
// proof/workflow handler without exposing ordinary HTTP routes.
func NewEnrollmentSnapshotByTokenMux(handler *EnrollmentSnapshotByTokenHandler, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r != nil && r.URL != nil && r.URL.Path == EnrollmentSnapshotByTokenPath {
			if handler == nil {
				writeSnapshotByTokenError(w, ErrSnapshotByTokenUnavailable)
				return
			}
			handler.ServeHTTP(w, r)
			return
		}
		if next == nil {
			setSnapshotByTokenHeaders(w)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (h *EnrollmentSnapshotByTokenHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setSnapshotByTokenHeaders(w)
	if r == nil || r.URL == nil || r.URL.Path != EnrollmentSnapshotByTokenPath ||
		r.URL.RawPath != "" || r.URL.RawQuery != "" || r.URL.ForceQuery {
		writeSnapshotByTokenStatus(w, http.StatusNotFound, "Not found")
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeSnapshotByTokenStatus(w, http.StatusMethodNotAllowed, "Invalid request")
		return
	}
	if !h.validTLSAuthority(r) {
		writeSnapshotByTokenError(w, ErrSnapshotByTokenInvalidRequest)
		return
	}

	token, ok := decodeSnapshotByTokenRequest(w, r)
	if !ok {
		writeSnapshotByTokenError(w, ErrSnapshotByTokenInvalidRequest)
		return
	}
	body, err := h.readSnapshot(r.Context(), token)
	if err != nil {
		writeSnapshotByTokenError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func (h *EnrollmentSnapshotByTokenHandler) readSnapshot(ctx context.Context, token string) ([]byte, error) {
	if h == nil || ctx == nil || h.store == nil || h.identity == nil || h.parseToken == nil ||
		h.verifyToken == nil || h.clusterKeyLoader == nil {
		return nil, ErrSnapshotByTokenUnavailable
	}
	selector, _, ok := h.parseToken(token)
	if !ok || selector == "" {
		return nil, ErrSnapshotByTokenInvalidRequest
	}
	if err := h.verifyToken(ctx, token); err != nil {
		return nil, ErrSnapshotByTokenUnauthenticated
	}
	current, loaded := snapshotByTokenCurrent(h.identity)
	if !loaded {
		return nil, ErrSnapshotByTokenUnavailable
	}
	if !readySnapshotByTokenIdentity(current) {
		return nil, ErrSnapshotByTokenDenied
	}

	material, err := h.clusterKeyLoader(ctx)
	if err != nil || material == nil {
		return nil, ErrSnapshotByTokenUnavailable
	}
	defer material.Destroy()
	keyBytes := material.CopyBytes()
	if len(keyBytes) == 0 {
		return nil, ErrSnapshotByTokenUnavailable
	}
	zeroBytes(keyBytes)

	identityRecord := configuration.SnapshotPublicationIdentityRecord{
		InstallationID:        string(current.InstallationID),
		NodeID:                string(current.NodeID),
		Role:                  current.Role,
		LeadershipGeneration:  int64(current.LeadershipGeneration),
		LatestKnownGeneration: int64(current.LatestKnownGeneration),
		ClusterKeyID:          current.ClusterKeyID,
		ClusterKeyUsable:      true,
	}
	body, err := h.store.ReadSnapshotPublicationByToken(ctx, selector, identityRecord)
	if err != nil {
		switch {
		case errors.Is(err, ErrNoPublishableSnapshot):
			return nil, ErrSnapshotByTokenNotReady
		case errors.Is(err, ErrSnapshotByTokenInvalidRequest):
			return nil, ErrSnapshotByTokenInvalidRequest
		case errors.Is(err, ErrSnapshotByTokenUnauthenticated):
			return nil, ErrSnapshotByTokenUnauthenticated
		case errors.Is(err, ErrSnapshotByTokenGone):
			return nil, ErrSnapshotByTokenGone
		case errors.Is(err, ErrSnapshotByTokenDenied):
			return nil, ErrSnapshotByTokenDenied
		default:
			return nil, ErrSnapshotByTokenUnavailable
		}
	}
	if body == nil {
		return nil, ErrSnapshotByTokenNotReady
	}
	if len(body) == 0 {
		return nil, ErrSnapshotByTokenDenied
	}
	return append([]byte(nil), body...), nil
}

func snapshotByTokenCurrent(source SnapshotByTokenIdentitySource) (current identity.Identity, loaded bool) {
	defer func() {
		if recover() != nil {
			current, loaded = identity.Identity{}, false
		}
	}()
	return source.Current()
}

func readySnapshotByTokenIdentity(current identity.Identity) bool {
	if !current.InstallationID.IsValid() || !current.NodeID.IsValid() || current.IsStalePrimary() ||
		current.LeadershipGeneration <= 0 || current.LatestKnownGeneration != current.LeadershipGeneration {
		return false
	}
	return current.Role == domain.TopologyRolePrimary || current.Role == domain.TopologyRolePrimaryWithNodes
}

func (h *EnrollmentSnapshotByTokenHandler) validTLSAuthority(r *http.Request) bool {
	if r == nil || r.TLS == nil || !r.TLS.HandshakeComplete || r.TLS.Version < tls.VersionTLS13 {
		return false
	}
	host, port, err := net.SplitHostPort(r.Host)
	if err != nil || host == "" || port == "" || !validSnapshotPublicationPort(port) {
		return false
	}
	if !identityProofSNIMatches(host, r.TLS.ServerName) || h.certificate == nil {
		return false
	}
	certificate, err := h.certificate(r.Context())
	return err == nil && certificate != nil && certificate.VerifyHostname(host) == nil
}

func decodeSnapshotByTokenRequest(w http.ResponseWriter, r *http.Request) (string, bool) {
	if r == nil || r.Body == nil || r.ContentLength > maxSnapshotByTokenBodyBytes {
		return "", false
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxSnapshotByTokenBodyBytes))
	decoder.DisallowUnknownFields()
	var request struct {
		Token string `json:"token"`
	}
	if err := decoder.Decode(&request); err != nil || request.Token == "" {
		return "", false
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return "", false
	}
	return request.Token, true
}

func setSnapshotByTokenHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
}

func writeSnapshotByTokenError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrSnapshotByTokenNotReady):
		setSnapshotByTokenHeaders(w)
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, ErrSnapshotByTokenGone):
		writeSnapshotByTokenStatus(w, http.StatusGone, "Gone")
	case errors.Is(err, ErrSnapshotByTokenInvalidRequest):
		writeSnapshotByTokenStatus(w, http.StatusBadRequest, "Invalid request")
	case errors.Is(err, ErrSnapshotByTokenUnauthenticated), errors.Is(err, ErrSnapshotByTokenDenied):
		writeSnapshotByTokenStatus(w, http.StatusForbidden, "Permission denied")
	default:
		writeSnapshotByTokenStatus(w, http.StatusServiceUnavailable, "Service unavailable")
	}
}

func writeSnapshotByTokenStatus(w http.ResponseWriter, status int, message string) {
	setSnapshotByTokenHeaders(w)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"error":"` + message + `"}`))
}

// ParseHTTPEnrollmentToken exposes the workflow's canonical parser to the
// cmd/server transport selector without duplicating token parsing logic.
func ParseHTTPEnrollmentToken(value string) (string, string, bool) {
	selector, secret, ok := parseHTTPEnrollmentToken(value)
	if !ok {
		return "", "", false
	}
	defer zeroBytes(secret)
	return selector, string(secret), true
}
