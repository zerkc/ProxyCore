package httpserver

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/enrollment"
	replicationsnapshot "github.com/zerkc/ProxyCore/apps/api/internal/snapshot"
)

const (
	EnrollmentDraftPath            = "/api/topology/enrollment/draft"
	EnrollmentPreviewPath          = "/api/topology/enrollment/preview"
	EnrollmentConfirmPath          = "/api/topology/enrollment/confirm"
	EnrollmentRecoverPath          = "/api/topology/enrollment/recover"
	maxEnrollmentWorkflowBodyBytes = maxIdentityProofBodyBytes
)

var (
	ErrEnrollmentWorkflowUnavailable = errors.New("enrollment workflow unavailable")
	ErrEnrollmentWorkflowDenied      = errors.New("enrollment workflow denied")
)

// EnrollmentSnapshotFetcher is the transport boundary for the authenticated
// PRIMARY exchange. Implementations receive the token only in memory and must
// never place it in a URL, log line, or response.
type EnrollmentSnapshotFetcher func(context.Context, string, string) (*replicationsnapshot.Envelope, error)

type EnrollmentSnapshotValidator interface {
	Validate(context.Context, replicationsnapshot.Envelope) replicationsnapshot.ValidationResult
}

type enrollmentWorkflowHandler struct {
	cache            *enrollment.DraftCache
	verifyToken      func(context.Context, string) error
	fetchSnapshot    EnrollmentSnapshotFetcher
	validator        EnrollmentSnapshotValidator
	identity         func(context.Context) (EnrollmentLocalIdentity, error)
	localIngress     func(context.Context) (domain.Ingress, error)
	converter        *enrollment.NodeConverter
	archiveTTL       time.Duration
	applyWaitTimeout time.Duration
	certificate      IdentityProofCertificateProvider
}

func newEnrollmentWorkflowHandler(opts EnrollmentWorkflowHandlerOptions, certificate IdentityProofCertificateProvider) *enrollmentWorkflowHandler {
	archiveTTL := opts.ArchiveTTL
	if archiveTTL <= 0 {
		archiveTTL = replicationsnapshot.ArchiveTTLDefault
	}
	applyWaitTimeout := opts.ApplyWaitTimeout
	if applyWaitTimeout <= 0 {
		applyWaitTimeout = 5 * time.Minute
	}
	return &enrollmentWorkflowHandler{
		cache: opts.Cache, verifyToken: opts.VerifyToken, fetchSnapshot: opts.FetchSnapshot,
		validator: opts.Validator, identity: opts.Identity, localIngress: opts.LocalIngress,
		converter: opts.Converter, archiveTTL: archiveTTL, applyWaitTimeout: applyWaitTimeout,
		certificate: certificate,
	}
}

func (h *enrollmentWorkflowHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL == nil || r.URL.RawPath != "" || r.URL.RawQuery != "" || r.URL.ForceQuery {
		writeIdentityProofError(w, http.StatusNotFound, "not found")
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeIdentityProofError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if r.TLS == nil || !r.TLS.HandshakeComplete || r.TLS.Version < tls.VersionTLS13 {
		writeIdentityProofError(w, http.StatusBadRequest, "secure transport required")
		return
	}
	switch r.URL.Path {
	case EnrollmentDraftPath:
		h.handleDraft(w, r)
	case EnrollmentPreviewPath:
		h.handlePreview(w, r)
	case EnrollmentConfirmPath:
		h.handleConfirm(w, r)
	case EnrollmentRecoverPath:
		h.handleRecover(w, r)
	default:
		writeIdentityProofError(w, http.StatusNotFound, "not found")
	}
}

func (h *enrollmentWorkflowHandler) handleDraft(w http.ResponseWriter, r *http.Request) {
	request, ok := decodeEnrollmentWorkflowBody[enrollmentDraftRequest](w, r)
	if !ok {
		return
	}
	canonicalURL, err := enrollment.CanonicalizePrimaryURL(request.PrimaryURL)
	if err != nil || canonicalURL != request.PrimaryURL {
		writeIdentityProofError(w, http.StatusBadRequest, "invalid enrollment draft request")
		return
	}
	if !h.requireTLSAuthority(w, r, canonicalURL) {
		return
	}
	selector, secret, valid := parseHTTPEnrollmentToken(request.Token)
	zeroBytes(secret)
	if !valid || h.verifyToken == nil || h.verifyToken(r.Context(), request.Token) != nil {
		writeIdentityProofError(w, http.StatusForbidden, "Permission denied")
		return
	}
	if h.cache == nil || h.fetchSnapshot == nil || h.validator == nil || h.identity == nil {
		writeIdentityProofError(w, http.StatusServiceUnavailable, "enrollment workflow unavailable")
		return
	}
	envelope, err := h.fetchSnapshot(r.Context(), request.Token, canonicalURL)
	if err != nil || envelope == nil {
		writeIdentityProofError(w, http.StatusConflict, "primary snapshot unavailable")
		return
	}
	if !h.validateEnvelope(r, envelope) {
		writeIdentityProofError(w, http.StatusConflict, "primary snapshot denied")
		return
	}
	overlay, err := h.localOverlay(r.Context())
	if err != nil {
		writeIdentityProofError(w, http.StatusConflict, "local identity unavailable")
		return
	}
	draft, err := h.cache.PutWithPrimaryURL(selector, canonicalURL, envelope, overlay)
	if err != nil {
		writeIdentityProofError(w, http.StatusConflict, "enrollment draft unavailable")
		return
	}
	h.writePreview(w, http.StatusOK, draft, overlay)
}

func (h *enrollmentWorkflowHandler) handlePreview(w http.ResponseWriter, r *http.Request) {
	request, ok := decodeEnrollmentWorkflowBody[enrollmentPreviewRequest](w, r)
	if !ok || request.DraftID == "" {
		writeIdentityProofError(w, http.StatusBadRequest, "invalid enrollment preview request")
		return
	}
	if h.cache == nil {
		writeIdentityProofError(w, http.StatusGone, "enrollment draft expired; re-enter the token")
		return
	}
	draft, ok := h.cache.GetByID(request.DraftID)
	if !ok {
		writeIdentityProofError(w, http.StatusGone, "enrollment draft expired; re-enter the token")
		return
	}
	if !h.requireTLSAuthority(w, r, draft.PrimaryURL) {
		return
	}
	if h.validator == nil || h.identity == nil {
		writeIdentityProofError(w, http.StatusServiceUnavailable, "enrollment workflow unavailable")
		return
	}
	overlay, err := h.localOverlay(r.Context())
	if err != nil || !requestedOverlayMatches(request.RequestedNodeID, request.RequestedIngress, overlay) {
		writeIdentityProofError(w, http.StatusConflict, "requested node values are not authoritative")
		return
	}
	if !h.validateEnvelope(r, draft.Envelope) {
		writeIdentityProofError(w, http.StatusConflict, "primary snapshot denied")
		return
	}
	h.writePreview(w, http.StatusOK, draft, overlay)
}

func (h *enrollmentWorkflowHandler) handleConfirm(w http.ResponseWriter, r *http.Request) {
	request, ok := decodeEnrollmentWorkflowBody[enrollmentConfirmRequest](w, r)
	if !ok || request.DraftID == "" {
		writeIdentityProofError(w, http.StatusBadRequest, "invalid enrollment confirm request")
		return
	}
	if h.cache == nil {
		writeIdentityProofError(w, http.StatusGone, "enrollment draft expired; re-enter the token")
		return
	}
	draft, ok := h.cache.GetByID(request.DraftID)
	if !ok {
		writeIdentityProofError(w, http.StatusGone, "enrollment draft expired; re-enter the token")
		return
	}
	if !h.requireTLSAuthority(w, r, draft.PrimaryURL) {
		return
	}
	if h.converter == nil || h.validator == nil || h.identity == nil {
		writeIdentityProofError(w, http.StatusConflict, "enrollment conversion denied")
		return
	}
	if !h.validateEnvelope(r, draft.Envelope) {
		writeIdentityProofError(w, http.StatusConflict, "primary snapshot denied")
		return
	}
	local, err := h.identity(r.Context())
	if err != nil || !local.NodeID.IsValid() || !local.InstallationID.IsValid() {
		writeIdentityProofError(w, http.StatusConflict, "identity transition denied")
		return
	}
	ingress, err := h.currentIngress(r.Context())
	if err != nil {
		writeIdentityProofError(w, http.StatusConflict, "identity transition denied")
		return
	}
	result, err := h.converter.Convert(r.Context(), enrollment.NodeConversionInput{
		Envelope: draft.Envelope, LocalNodeID: local.NodeID, LocalIngress: ingress,
		LocalInstallId: local.InstallationID, ArchiveTTL: h.archiveTTL, ApplyWaitTimeout: h.applyWaitTimeout,
	})
	if err != nil {
		writeIdentityProofError(w, http.StatusConflict, genericConversionReason(err))
		return
	}
	h.cache.DeleteByID(request.DraftID)
	response := enrollmentConfirmResponse{
		Role: result.Identity.Role, Generation: uint64(result.Identity.LeadershipGeneration),
		NodeID: result.Identity.NodeID.String(), ArchiveID: result.ArchiveID.String(), ApplyJobID: result.ApplyJobID.String(),
	}
	if result.Identity.ClusterKeyID != nil {
		response.ClusterKeyID = result.Identity.ClusterKeyID.String()
	}
	writeWorkflowJSON(w, http.StatusOK, response)
}

func (h *enrollmentWorkflowHandler) handleRecover(w http.ResponseWriter, r *http.Request) {
	request, ok := decodeEnrollmentWorkflowBody[enrollmentRecoverRequest](w, r)
	if !ok {
		return
	}
	selector, secret, valid := parseHTTPEnrollmentToken(request.Token)
	zeroBytes(secret)
	if !valid || h.verifyToken == nil || h.verifyToken(r.Context(), request.Token) != nil || h.cache == nil {
		writeIdentityProofError(w, http.StatusGone, "enrollment draft expired; re-enter the token")
		return
	}
	draft, ok := h.cache.GetBySelector(selector)
	if !ok {
		writeIdentityProofError(w, http.StatusGone, "enrollment draft expired; re-enter the token")
		return
	}
	if !h.requireTLSAuthority(w, r, draft.PrimaryURL) {
		return
	}
	if h.validator == nil || h.identity == nil || !h.validateEnvelope(r, draft.Envelope) {
		writeIdentityProofError(w, http.StatusConflict, "enrollment preview unavailable")
		return
	}
	overlay, err := h.localOverlay(r.Context())
	if err != nil {
		writeIdentityProofError(w, http.StatusConflict, "enrollment preview unavailable")
		return
	}
	h.writePreview(w, http.StatusOK, draft, overlay)
}

func (h *enrollmentWorkflowHandler) validateEnvelope(r *http.Request, envelope *replicationsnapshot.Envelope) bool {
	if envelope == nil || h.validator == nil {
		return false
	}
	result := h.validator.Validate(r.Context(), *envelope)
	return result.OK && len(result.Issues) == 0
}

func (h *enrollmentWorkflowHandler) localOverlay(ctx context.Context) (enrollment.NodeLocalOverlay, error) {
	if h.identity == nil || ctx == nil {
		return enrollment.NodeLocalOverlay{}, ErrEnrollmentWorkflowUnavailable
	}
	local, err := h.identity(ctx)
	if err != nil || !local.NodeID.IsValid() {
		return enrollment.NodeLocalOverlay{}, ErrEnrollmentWorkflowUnavailable
	}
	ingress, err := h.currentIngress(ctx)
	if err != nil {
		return enrollment.NodeLocalOverlay{}, err
	}
	return enrollment.NodeLocalOverlay{NodeID: local.NodeID, Ingress: ingress, Role: domain.TopologyRoleNode}, nil
}

func (h *enrollmentWorkflowHandler) currentIngress(ctx context.Context) (domain.Ingress, error) {
	if h.localIngress == nil {
		return domain.Ingress{}, nil
	}
	return h.localIngress(ctx)
}

func requestedOverlayMatches(requestedNodeID string, requestedIngress *domain.Ingress, overlay enrollment.NodeLocalOverlay) bool {
	if requestedNodeID != "" && requestedNodeID != overlay.NodeID.String() {
		return false
	}
	if requestedIngress != nil && *requestedIngress != overlay.Ingress {
		return false
	}
	return true
}

func (h *enrollmentWorkflowHandler) writePreview(w http.ResponseWriter, status int, draft enrollment.Draft, overlay enrollment.NodeLocalOverlay) {
	writeWorkflowJSON(w, status, enrollmentPreviewResponse{
		DraftID: draft.DraftID, EnvelopePreview: newEnvelopePreview(draft.Envelope, overlay),
		NodeLocalOverlay: overlay, ExpiresAt: draft.ExpiresAt,
	})
}

func newEnvelopePreview(envelope *replicationsnapshot.Envelope, overlay enrollment.NodeLocalOverlay) enrollmentEnvelopePreview {
	preview := enrollmentEnvelopePreview{Ingress: overlay.Ingress, NodeID: overlay.NodeID.String(), Role: overlay.Role}
	if envelope == nil {
		return preview
	}
	preview.SourcePrimaryID = envelope.Transient.SourcePrimaryID.String()
	if envelope.NodeLocal.Role.IsValid() {
		preview.Role = envelope.NodeLocal.Role
	}
	preview.SnapshotVersion = uint32(envelope.Transient.SnapshotVersion)
	preview.ReplicationVersion = uint32(envelope.Transient.ReplicationVersion)
	preview.Generation = uint64(envelope.Transient.LeadershipGeneration)
	preview.ContentHash = envelope.ContentHash
	if envelope.NodeLocal.ClusterKeyRef != nil {
		preview.ClusterKeyID = envelope.NodeLocal.ClusterKeyRef.String()
	}
	return preview
}

func (h *enrollmentWorkflowHandler) requireTLSAuthority(w http.ResponseWriter, r *http.Request, primaryURL string) bool {
	if r.TLS == nil || !r.TLS.HandshakeComplete || r.TLS.Version < tls.VersionTLS13 {
		writeIdentityProofError(w, http.StatusBadRequest, "secure transport required")
		return false
	}
	parsed, err := url.Parse(primaryURL)
	if err != nil || parsed.Host == "" || r.Host != parsed.Host || !identityProofSNIMatches(parsed.Hostname(), r.TLS.ServerName) {
		writeIdentityProofError(w, http.StatusMisdirectedRequest, "invalid enrollment authority")
		return false
	}
	if h.certificate == nil {
		writeIdentityProofError(w, http.StatusServiceUnavailable, "enrollment workflow unavailable")
		return false
	}
	certificate, err := h.certificate(r.Context())
	if err != nil || certificate == nil || certificate.VerifyHostname(parsed.Hostname()) != nil {
		writeIdentityProofError(w, http.StatusMisdirectedRequest, "invalid enrollment authority")
		return false
	}
	return true
}

func genericConversionReason(err error) string {
	switch {
	case errors.Is(err, enrollment.ErrNodeConversionAborted):
		return "initial apply failed"
	case errors.Is(err, enrollment.ErrNodeConversionDenied):
		return "identity transition denied"
	default:
		return "enrollment conversion denied"
	}
}

func writeWorkflowJSON(w http.ResponseWriter, status int, body any) {
	setNoStore(w)
	writeJSON(w, status, body)
}

func decodeEnrollmentWorkflowBody[T any](w http.ResponseWriter, r *http.Request) (T, bool) {
	var value T
	if r.ContentLength > maxEnrollmentWorkflowBodyBytes {
		writeIdentityProofError(w, http.StatusBadRequest, "invalid enrollment workflow request")
		return value, false
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxEnrollmentWorkflowBodyBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		writeIdentityProofError(w, http.StatusBadRequest, "invalid enrollment workflow request")
		return value, false
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		writeIdentityProofError(w, http.StatusBadRequest, "invalid enrollment workflow request")
		return value, false
	}
	return value, true
}

var _ EnrollmentSnapshotValidator = (*replicationsnapshot.Validator)(nil)
