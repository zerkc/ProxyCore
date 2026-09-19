package httpserver

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	syncpublication "github.com/zerkc/ProxyCore/apps/api/internal/sync"
)

const EnrollmentSnapshotPublicationPath = "/api/topology/sync/snapshot"

type EnrollmentSnapshotPublicationHandlerOptions struct {
	Store       configuration.SnapshotPublicationStore
	Identity    syncpublication.SnapshotIdentityReader
	Certificate IdentityProofCertificateProvider
	MaxBytes    int
}

// EnrollmentSnapshotPublicationHandler serves the immutable latest applied
// snapshot over the dedicated enrollment TLS listener.
type EnrollmentSnapshotPublicationHandler struct {
	store       configuration.SnapshotPublicationStore
	identity    syncpublication.SnapshotIdentityReader
	certificate IdentityProofCertificateProvider
	maxBytes    int
}

func NewEnrollmentSnapshotPublicationHandler(opts EnrollmentSnapshotPublicationHandlerOptions) *EnrollmentSnapshotPublicationHandler {
	maxBytes := opts.MaxBytes
	if maxBytes <= 0 {
		maxBytes = configuration.SnapshotPublicationMaxBytes
	}
	return &EnrollmentSnapshotPublicationHandler{
		store: opts.Store, identity: opts.Identity, certificate: opts.Certificate, maxBytes: maxBytes,
	}
}

// NewEnrollmentSnapshotPublicationMux places the publication route in front
// of the existing proof/workflow handler without exposing ordinary HTTP routes.
func NewEnrollmentSnapshotPublicationMux(publication *EnrollmentSnapshotPublicationHandler, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if publication != nil && r != nil && r.URL != nil && r.URL.Path == EnrollmentSnapshotPublicationPath {
			publication.ServeHTTP(w, r)
			return
		}
		if next == nil {
			writeSnapshotPublicationError(w, http.StatusNotFound)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (h *EnrollmentSnapshotPublicationHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setSnapshotPublicationHeaders(w)
	if r == nil || r.URL == nil || r.URL.Path != EnrollmentSnapshotPublicationPath ||
		r.URL.RawPath != "" || r.URL.RawQuery != "" || r.URL.ForceQuery {
		writeSnapshotPublicationError(w, http.StatusNotFound)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeSnapshotPublicationError(w, http.StatusMethodNotAllowed)
		return
	}
	if !h.validTLSAuthority(r) {
		writeSnapshotPublicationError(w, http.StatusBadRequest)
		return
	}
	if h.identity == nil {
		writeSnapshotPublicationError(w, http.StatusForbidden)
		return
	}
	current, err := h.identity.SnapshotPublicationIdentity()
	if err != nil {
		writeSnapshotPublicationError(w, http.StatusNotFound)
		return
	}
	if current.Role != domain.TopologyRolePrimary && current.Role != domain.TopologyRolePrimaryWithNodes {
		writeSnapshotPublicationError(w, http.StatusForbidden)
		return
	}
	if current.Role == domain.TopologyRoleStalePrimary {
		writeSnapshotPublicationError(w, http.StatusForbidden)
		return
	}
	if current.IsStalePrimary() || !current.InstallationID.IsValid() || !current.NodeID.IsValid() ||
		current.LeadershipGeneration <= 0 || current.LatestKnownGeneration != current.LeadershipGeneration {
		writeSnapshotPublicationError(w, http.StatusNotFound)
		return
	}
	if current.ClusterKeyID == nil || h.store == nil {
		writeSnapshotPublicationError(w, http.StatusForbidden)
		return
	}

	presented, ok := parseSnapshotAuthorization(r.Header.Get("Authorization"))
	if !ok {
		writeSnapshotPublicationError(w, http.StatusForbidden)
		return
	}
	parsed, err := syncpublication.ParseNodeCredential(presented)
	if err != nil {
		writeSnapshotPublicationError(w, http.StatusForbidden)
		return
	}
	canonicalBearer := parsed.BearerCopy()
	parsed.Destroy()
	if canonicalBearer == "" {
		writeSnapshotPublicationError(w, http.StatusForbidden)
		return
	}

	state := &snapshotPublicationAuthState{}
	store := &observingSnapshotPublicationStore{store: h.store, state: state}
	publisher := syncpublication.NewSnapshotPublisher(store, h.identity, syncpublication.SnapshotPublisherOptions{
		MaxBytes: h.maxBytes,
	})
	result, err := publisher.Publish(r.Context(), syncpublication.SnapshotRequest{Credential: canonicalBearer})
	if err != nil {
		switch {
		case state.revoked:
			writeSnapshotPublicationError(w, http.StatusGone)
		case state.authenticated && !state.resultReturned:
			writeSnapshotPublicationError(w, http.StatusNoContent)
		default:
			writeSnapshotPublicationError(w, http.StatusForbidden)
		}
		return
	}
	if len(result.Bytes) == 0 {
		writeSnapshotPublicationError(w, http.StatusNoContent)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(result.Bytes)
}

func (h *EnrollmentSnapshotPublicationHandler) validTLSAuthority(r *http.Request) bool {
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

func validSnapshotPublicationPort(value string) bool {
	port, err := strconv.ParseUint(value, 10, 16)
	return err == nil && port > 0
}

func parseSnapshotAuthorization(value string) (string, bool) {
	const prefix = "Bearer "
	if !strings.HasPrefix(value, prefix) || len(value) == len(prefix) ||
		strings.TrimSpace(value[len(prefix):]) != value[len(prefix):] {
		return "", false
	}
	return value[len(prefix):], true
}

type snapshotPublicationAuthState struct {
	revoked        bool
	authenticated  bool
	resultReturned bool
}

type observingSnapshotPublicationStore struct {
	store configuration.SnapshotPublicationStore
	state *snapshotPublicationAuthState
}

func (s *observingSnapshotPublicationStore) ReadSnapshotPublication(ctx context.Context, request configuration.SnapshotPublicationRequest) (configuration.SnapshotPublicationResult, error) {
	if s == nil || s.store == nil || s.state == nil || request.Authenticate == nil {
		return configuration.SnapshotPublicationResult{}, configuration.ErrSnapshotPublicationStore
	}
	original := request.Authenticate
	request.Authenticate = func(presented string, record configuration.SnapshotPublicationCredentialRecord) (configuration.SnapshotPublicationPrincipal, error) {
		s.state.revoked = record.CredentialRevokedAt != nil || record.NodeRevokedAt != nil
		principal, err := original(presented, record)
		if err == nil {
			s.state.authenticated = true
		}
		return principal, err
	}
	result, err := s.store.ReadSnapshotPublication(ctx, request)
	s.state.resultReturned = err == nil
	return result, err
}

func setSnapshotPublicationHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
}

func writeSnapshotPublicationError(w http.ResponseWriter, status int) {
	setSnapshotPublicationHeaders(w)
	if status == http.StatusNoContent {
		w.WriteHeader(status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"error":"Permission denied"}`))
}

var _ configuration.SnapshotPublicationStore = (*observingSnapshotPublicationStore)(nil)
