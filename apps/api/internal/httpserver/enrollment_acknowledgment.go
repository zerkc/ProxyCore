package httpserver

import (
	"context"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/enrollment"
	syncpublication "github.com/zerkc/ProxyCore/apps/api/internal/sync"
)

const (
	EnrollmentSnapshotAcknowledgementPath = "/api/topology/sync/acknowledge"
	maxSnapshotAcknowledgementBodyBytes   = 8 << 10
)

var (
	ErrSnapshotAcknowledgementInvalidRequest  = errors.New("snapshot acknowledgement invalid request")
	ErrSnapshotAcknowledgementUnauthenticated = errors.New("snapshot acknowledgement unauthenticated")
	ErrSnapshotAcknowledgementUnavailable     = errors.New("snapshot acknowledgement unavailable")
	ErrSnapshotAcknowledgementGone            = errors.New("snapshot acknowledgement gone")
	ErrSnapshotAcknowledgementDenied          = errors.New("snapshot acknowledgement denied")

	// American aliases keep callers from having to know the spelling used by
	// the existing SnapshotAcknowledgement persistence type.
	ErrSnapshotAcknowledgmentInvalidRequest  = ErrSnapshotAcknowledgementInvalidRequest
	ErrSnapshotAcknowledgmentUnauthenticated = ErrSnapshotAcknowledgementUnauthenticated
	ErrSnapshotAcknowledgmentUnavailable     = ErrSnapshotAcknowledgementUnavailable
	ErrSnapshotAcknowledgmentGone            = ErrSnapshotAcknowledgementGone
	ErrSnapshotAcknowledgmentDenied          = ErrSnapshotAcknowledgementDenied
)

// EnrollmentSnapshotAcknowledgmentPath is an American-spelling alias for the
// public route constant.
const EnrollmentSnapshotAcknowledgmentPath = EnrollmentSnapshotAcknowledgementPath

type EnrollmentSnapshotAcknowledgementHandlerOptions struct {
	Store       configuration.SnapshotAcknowledgementStore
	Authorizer  configuration.SnapshotAcknowledgementAuthorizer
	Publication configuration.SnapshotPublicationStore
	Identity    syncpublication.SnapshotIdentityReader
	Certificate IdentityProofCertificateProvider
	Now         func() time.Time
}

// EnrollmentSnapshotAcknowledgementHandler records the exact latest applied
// snapshot tuple over the dedicated TLS listener. It never accepts a redirect
// or an ordinary HTTP request and never returns secret-bearing data.
type EnrollmentSnapshotAcknowledgementHandler struct {
	store       configuration.SnapshotAcknowledgementStore
	authorizer  configuration.SnapshotAcknowledgementAuthorizer
	publication configuration.SnapshotPublicationStore
	identity    syncpublication.SnapshotIdentityReader
	certificate IdentityProofCertificateProvider
	now         func() time.Time
}

// American-spelling aliases are intentionally additive; the persisted
// contract and existing Go types use "Acknowledgement".
type EnrollmentSnapshotAcknowledgmentHandler = EnrollmentSnapshotAcknowledgementHandler
type EnrollmentSnapshotAcknowledgmentHandlerOptions = EnrollmentSnapshotAcknowledgementHandlerOptions

func NewEnrollmentSnapshotAcknowledgementHandler(opts EnrollmentSnapshotAcknowledgementHandlerOptions) *EnrollmentSnapshotAcknowledgementHandler {
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	authorizer := opts.Authorizer
	if authorizer == nil {
		if candidate, ok := opts.Store.(configuration.SnapshotAcknowledgementAuthorizer); ok {
			authorizer = candidate
		}
	}
	publication := opts.Publication
	if publication == nil {
		if candidate, ok := opts.Store.(configuration.SnapshotPublicationStore); ok {
			publication = candidate
		}
	}
	return &EnrollmentSnapshotAcknowledgementHandler{
		store: opts.Store, authorizer: authorizer, publication: publication,
		identity: opts.Identity, certificate: opts.Certificate, now: now,
	}
}

func NewEnrollmentSnapshotAcknowledgmentHandler(opts EnrollmentSnapshotAcknowledgmentHandlerOptions) *EnrollmentSnapshotAcknowledgmentHandler {
	return NewEnrollmentSnapshotAcknowledgementHandler(opts)
}

// NewEnrollmentSnapshotAcknowledgementMux places the acknowledgement route in
// front of the proof/workflow handler without exposing ordinary HTTP routes.
func NewEnrollmentSnapshotAcknowledgementMux(acknowledgement *EnrollmentSnapshotAcknowledgementHandler, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r != nil && r.URL != nil && r.URL.Path == EnrollmentSnapshotAcknowledgementPath {
			if acknowledgement == nil {
				writeSnapshotAcknowledgementError(w, http.StatusServiceUnavailable)
				return
			}
			acknowledgement.ServeHTTP(w, r)
			return
		}
		if next == nil {
			writeSnapshotAcknowledgementError(w, http.StatusNotFound)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func NewEnrollmentSnapshotAcknowledgmentMux(acknowledgement *EnrollmentSnapshotAcknowledgmentHandler, next http.Handler) http.Handler {
	return NewEnrollmentSnapshotAcknowledgementMux(acknowledgement, next)
}

func (h *EnrollmentSnapshotAcknowledgementHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setSnapshotAcknowledgementHeaders(w)
	if r == nil || r.URL == nil || r.URL.Path != EnrollmentSnapshotAcknowledgementPath ||
		r.URL.RawPath != "" || r.URL.RawQuery != "" || r.URL.ForceQuery {
		writeSnapshotAcknowledgementError(w, http.StatusNotFound)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeSnapshotAcknowledgementError(w, http.StatusMethodNotAllowed)
		return
	}
	if !h.validTLSAuthority(r) {
		writeSnapshotAcknowledgementError(w, http.StatusBadRequest)
		return
	}

	request, ok := decodeSnapshotAcknowledgementRequest(w, r)
	if !ok {
		writeSnapshotAcknowledgementError(w, http.StatusBadRequest)
		return
	}
	if _, err := enrollment.CanonicalizePrimaryURL(request.PrimaryURL); err != nil {
		writeSnapshotAcknowledgementError(w, http.StatusBadRequest)
		return
	}
	appliedAt, err := time.Parse(time.RFC3339Nano, request.AppliedAt)
	if err != nil || appliedAt.IsZero() {
		writeSnapshotAcknowledgementError(w, http.StatusBadRequest)
		return
	}
	if !validAcknowledgementUUID(request.NodeID) || !validAcknowledgementHash(request.ContentHash) {
		writeSnapshotAcknowledgementError(w, http.StatusBadRequest)
		return
	}

	presented, ok := parseSnapshotAuthorization(r.Header.Get("Authorization"))
	if !ok {
		writeSnapshotAcknowledgementError(w, http.StatusUnauthorized)
		return
	}
	credential, err := syncpublication.ParseNodeCredential(presented)
	if err != nil {
		writeSnapshotAcknowledgementError(w, http.StatusForbidden)
		return
	}
	credentialID := credential.ID()
	canonicalBearer := credential.BearerCopy()
	credential.Destroy()
	if credentialID == "" || canonicalBearer == "" {
		writeSnapshotAcknowledgementError(w, http.StatusForbidden)
		return
	}
	if h.identity == nil {
		writeSnapshotAcknowledgementError(w, http.StatusServiceUnavailable)
		return
	}
	current, err := h.identity.SnapshotPublicationIdentity()
	if err != nil {
		writeSnapshotAcknowledgementError(w, http.StatusServiceUnavailable)
		return
	}
	if current.Role != domain.TopologyRolePrimary && current.Role != domain.TopologyRolePrimaryWithNodes {
		writeSnapshotAcknowledgementError(w, http.StatusForbidden)
		return
	}
	if current.IsStalePrimary() || !current.InstallationID.IsValid() || !current.NodeID.IsValid() ||
		current.LeadershipGeneration <= 0 || current.LatestKnownGeneration != current.LeadershipGeneration {
		writeSnapshotAcknowledgementError(w, http.StatusForbidden)
		return
	}
	if current.ClusterKeyID == nil {
		writeSnapshotAcknowledgementError(w, http.StatusForbidden)
		return
	}

	state := &acknowledgementAuthState{}
	authenticate := acknowledgementAuthenticator(state)
	if h.authorizer != nil {
		err = h.authorizer.RecordSnapshotAcknowledgementForCredential(r.Context(), configuration.SnapshotAcknowledgementRequest{
			NodeID:              request.NodeID,
			ContentHash:         request.ContentHash,
			AppliedAt:           appliedAt,
			ReceivedAt:          h.now().UTC(),
			CredentialID:        credentialID,
			PresentedCredential: canonicalBearer,
			Authenticate:        authenticate,
		})
	} else {
		err = h.recordThroughPublication(r.Context(), request.NodeID, request.ContentHash, appliedAt, h.now().UTC(), credentialID, canonicalBearer, authenticate)
	}
	if err != nil {
		writeSnapshotAcknowledgementStoreError(w, err, state)
		return
	}
	if state.principalNodeID != request.NodeID || !state.authenticated {
		writeSnapshotAcknowledgementError(w, http.StatusForbidden)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *EnrollmentSnapshotAcknowledgementHandler) recordThroughPublication(
	ctx context.Context,
	nodeID, contentHash string,
	appliedAt, receivedAt time.Time,
	credentialID, presented string,
	authenticate configuration.SnapshotPublicationCredentialAuthenticator,
) error {
	if h == nil || h.publication == nil || h.store == nil {
		return configuration.ErrSnapshotAcknowledgementUnavailable
	}
	publication, err := h.publication.ReadSnapshotPublication(ctx, configuration.SnapshotPublicationRequest{
		CredentialID:        credentialID,
		PresentedCredential: presented,
		MaxBytes:            configuration.SnapshotPublicationMaxBytes,
		Authenticate:        authenticate,
	})
	if err != nil {
		return configuration.ErrSnapshotAcknowledgementDenied
	}
	if publication.Principal.NodeID != nodeID || publication.Snapshot.ContentHash != contentHash ||
		!publication.Snapshot.AppliedAt.Equal(appliedAt) {
		return configuration.ErrSnapshotAcknowledgementDenied
	}
	return h.store.RecordSnapshotAcknowledgement(ctx, configuration.SnapshotAcknowledgement{
		NodeID:               nodeID,
		ContentHash:          contentHash,
		SnapshotVersion:      publication.Snapshot.SnapshotVersion,
		ReplicationVersion:   publication.Snapshot.ReplicationVersion,
		RevisionID:           publication.Snapshot.RevisionID,
		LeadershipGeneration: publication.Snapshot.LeadershipGeneration,
		AppliedAt:            appliedAt,
		ReceivedAt:           receivedAt,
	})
}

type snapshotAcknowledgementWire struct {
	NodeID      string `json:"nodeId"`
	PrimaryURL  string `json:"primaryUrl"`
	ContentHash string `json:"contentHash"`
	AppliedAt   string `json:"appliedAt"`
}

func decodeSnapshotAcknowledgementRequest(w http.ResponseWriter, r *http.Request) (snapshotAcknowledgementWire, bool) {
	if r == nil || r.Body == nil || r.ContentLength > maxSnapshotAcknowledgementBodyBytes {
		return snapshotAcknowledgementWire{}, false
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxSnapshotAcknowledgementBodyBytes))
	decoder.DisallowUnknownFields()
	var request snapshotAcknowledgementWire
	if err := decoder.Decode(&request); err != nil || request.NodeID == "" || request.PrimaryURL == "" ||
		request.ContentHash == "" || request.AppliedAt == "" {
		return snapshotAcknowledgementWire{}, false
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return snapshotAcknowledgementWire{}, false
	}
	return request, true
}

func acknowledgementAuthenticator(state *acknowledgementAuthState) configuration.SnapshotPublicationCredentialAuthenticator {
	return func(presented string, record configuration.SnapshotPublicationCredentialRecord) (configuration.SnapshotPublicationPrincipal, error) {
		if state != nil {
			state.checked = true
			state.revoked = record.CredentialRevokedAt != nil || record.NodeRevokedAt != nil
		}
		principal, err := syncpublication.AuthenticateNodeCredential(presented, &syncpublication.NodeCredentialRecord{
			ID: record.ID, NodeID: record.NodeID, Hash: record.CredentialHash,
			HashVersion: record.HashVersion, RevokedAt: func() *time.Time {
				if record.CredentialRevokedAt != nil {
					return record.CredentialRevokedAt
				}
				return record.NodeRevokedAt
			}(),
		})
		if err != nil {
			return configuration.SnapshotPublicationPrincipal{}, ErrSnapshotAcknowledgementDenied
		}
		if state != nil {
			state.authenticated = true
			state.principalNodeID = principal.NodeID
		}
		return configuration.SnapshotPublicationPrincipal{NodeID: principal.NodeID, CredentialID: principal.CredentialID}, nil
	}
}

type acknowledgementAuthState struct {
	checked         bool
	authenticated   bool
	revoked         bool
	principalNodeID string
}

func writeSnapshotAcknowledgementStoreError(w http.ResponseWriter, err error, state *acknowledgementAuthState) {
	switch {
	case errors.Is(err, configuration.ErrSnapshotAcknowledgementRevoked), state != nil && state.revoked:
		writeSnapshotAcknowledgementError(w, http.StatusGone)
	case errors.Is(err, configuration.ErrSnapshotAcknowledgementUnavailable):
		writeSnapshotAcknowledgementError(w, http.StatusServiceUnavailable)
	case errors.Is(err, configuration.ErrSnapshotAcknowledgementDenied):
		writeSnapshotAcknowledgementError(w, http.StatusForbidden)
	default:
		writeSnapshotAcknowledgementError(w, http.StatusServiceUnavailable)
	}
}

func (h *EnrollmentSnapshotAcknowledgementHandler) validTLSAuthority(r *http.Request) bool {
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

func validAcknowledgementUUID(value string) bool {
	parsed, err := uuid.Parse(value)
	return err == nil && parsed != uuid.Nil && parsed.String() == value
}

func validAcknowledgementHash(value string) bool {
	if len(value) != 64 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func setSnapshotAcknowledgementHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Del("Location")
	w.Header().Del("Set-Cookie")
}

func writeSnapshotAcknowledgementError(w http.ResponseWriter, status int) {
	setSnapshotAcknowledgementHeaders(w)
	if status == http.StatusNoContent {
		w.WriteHeader(status)
		return
	}
	message := "Permission denied"
	switch status {
	case http.StatusBadRequest:
		message = "Invalid request"
	case http.StatusUnauthorized:
		message = "Authentication required"
	case http.StatusNotFound:
		message = "Not found"
	case http.StatusServiceUnavailable:
		message = "Service unavailable"
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"error":"` + message + `"}`))
}

var _ configuration.SnapshotAcknowledgementStore = (*configuration.PgPhase2Store)(nil)
