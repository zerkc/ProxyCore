package httpserver

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	gosync "sync"
	"time"

	"github.com/google/uuid"
	"github.com/zerkc/ProxyCore/apps/api/internal/cluster"
	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/enrollment"
	"github.com/zerkc/ProxyCore/apps/api/internal/identity"
	replicationsnapshot "github.com/zerkc/ProxyCore/apps/api/internal/snapshot"
	syncpublication "github.com/zerkc/ProxyCore/apps/api/internal/sync"
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

// SnapshotKEKLoader is the request boundary for the locally persisted cluster
// KEK. Implementations return a fresh caller-owned key for every validation.
type SnapshotKEKLoader interface {
	LoadLocalKEK(context.Context) (*cluster.KEK, uuid.UUID, error)
}

// NewLocalKEKSnapshotValidator adapts the local loader to the existing
// workflow validator seam without retaining a process-wide KEK. Each Validate
// call owns and destroys exactly one freshly loaded key.
func NewLocalKEKSnapshotValidator(loader SnapshotKEKLoader) EnrollmentSnapshotValidator {
	return localKEKSnapshotValidator{loader: loader}
}

type localKEKSnapshotValidator struct {
	loader SnapshotKEKLoader
}

func (v localKEKSnapshotValidator) Validate(ctx context.Context, envelope replicationsnapshot.Envelope) replicationsnapshot.ValidationResult {
	result, _ := v.validate(ctx, envelope)
	return result
}

// ValidatePreview validates structure, compatibility and owners for a read-only
// preview. A not-yet-enrolled installation holds no cluster KEK (the primary's
// KEK only arrives with the enrollment grant), so secrets that cannot be
// decrypted yet are tolerated here and ONLY here. Validate, used before any
// conversion, still fails closed on them.
func (v localKEKSnapshotValidator) ValidatePreview(ctx context.Context, envelope replicationsnapshot.Envelope) replicationsnapshot.ValidationResult {
	result, hasKEK := v.validate(ctx, envelope)
	if hasKEK {
		return result
	}
	filtered := replicationsnapshot.ValidationResult{OK: true}
	for _, issue := range result.Issues {
		if issue.Code == "NO_KEY" {
			continue
		}
		filtered.Issues = append(filtered.Issues, issue)
		filtered.OK = false
	}
	return filtered
}

// ValidateWithKEK strictly validates with an explicit KEK (the one a grant just
// delivered) instead of the local loader. The caller keeps ownership of kek.
func (v localKEKSnapshotValidator) ValidateWithKEK(ctx context.Context, envelope replicationsnapshot.Envelope, kek *cluster.KEK) replicationsnapshot.ValidationResult {
	if kek == nil {
		return replicationsnapshot.ValidationResult{OK: false, Issues: []replicationsnapshot.ValidationIssue{{Code: "NO_KEY"}}}
	}
	return replicationsnapshot.NewValidator(kek).Validate(ctx, envelope)
}

func (v localKEKSnapshotValidator) validate(ctx context.Context, envelope replicationsnapshot.Envelope) (replicationsnapshot.ValidationResult, bool) {
	var kek *cluster.KEK
	if v.loader != nil {
		if loaded, _, err := v.loader.LoadLocalKEK(ctx); err == nil && loaded != nil {
			kek = loaded
			defer kek.Destroy()
		}
	}
	return replicationsnapshot.NewValidator(kek).Validate(ctx, envelope), kek != nil
}

// EnrollmentSnapshotFetcher is the transport boundary for the authenticated
// PRIMARY exchange. Implementations receive the token only in memory and must
// never place it in a URL, log line, or response. Arguments are
// (ctx, primaryURL, credential).
type EnrollmentSnapshotFetcher func(context.Context, string, string) (*replicationsnapshot.Envelope, error)

type EnrollmentSnapshotValidator interface {
	Validate(context.Context, replicationsnapshot.Envelope) replicationsnapshot.ValidationResult
}

// EnrollmentNodeConverter is the conversion seam; *enrollment.NodeConverter
// implements it.
type EnrollmentNodeConverter interface {
	Convert(context.Context, enrollment.NodeConversionInput) (enrollment.NodeConversionResult, error)
}

// EnrollmentGrantExchanger POSTs the grant exchange to the primary. The token
// is passed in memory only.
type EnrollmentGrantExchanger func(ctx context.Context, primaryURL, token string, request syncpublication.ExchangeRequest) (syncpublication.ExchangeResult, error)

// EnrollmentGrantAcceptor opens the sealed bootstrap payload and persists the
// cluster KEK and node credential atomically (enrollment.BootstrapAcceptor).
type EnrollmentGrantAcceptor func(ctx context.Context, recipient *enrollment.BootstrapRecipient, binding enrollment.BootstrapBinding, sealed []byte, payloadHash string) (enrollment.AcceptedBootstrap, error)

// EnrollmentClusterKEKLoader reads back the persisted KEK for keyID; the
// caller owns and destroys the result.
type EnrollmentClusterKEKLoader func(ctx context.Context, keyID uuid.UUID) (*cluster.KEK, error)

// kekEnvelopeValidator is implemented by validators that can strictly validate
// with an explicit KEK. Confirm needs it because the installation is not yet a
// node, so the local loader cannot serve a KEK until after conversion.
type kekEnvelopeValidator interface {
	ValidateWithKEK(context.Context, replicationsnapshot.Envelope, *cluster.KEK) replicationsnapshot.ValidationResult
}

// EnrollmentSyncBindingRecorder stores the node's primary binding.
// EnrollmentPrimaryTrustRecorder stores the CA trusted for a primary origin.
type EnrollmentPrimaryTrustRecorder func(ctx context.Context, origin, caPEM string) error

type EnrollmentSyncBindingRecorder func(ctx context.Context, binding configuration.NodeSyncBinding, appliedHash string) error

const previewDigestDomain = "proxycore/enrollment/confirm/preview-digest/v1"

type enrollmentWorkflowHandler struct {
	cache            *enrollment.DraftCache
	verifyToken      func(context.Context, string) error
	fetchSnapshot    EnrollmentSnapshotFetcher
	validator        EnrollmentSnapshotValidator
	identity         func(context.Context) (EnrollmentLocalIdentity, error)
	localIngress     func(context.Context) (domain.Ingress, error)
	converter        EnrollmentNodeConverter
	exchangeGrant    EnrollmentGrantExchanger
	acceptGrant      EnrollmentGrantAcceptor
	loadClusterKEK   EnrollmentClusterKEKLoader
	recordSync       EnrollmentSyncBindingRecorder
	recordTrust      EnrollmentPrimaryTrustRecorder
	confirming       gosync.Map
	archiveTTL       time.Duration
	applyWaitTimeout time.Duration
	certificate      IdentityProofCertificateProvider
	logger           *log.Logger
	// trustedTransport is set only for the main API mount, where an owner
	// session authenticates the caller and TLS terminates at a proxy. The
	// dedicated enrollment listener never sets it.
	trustedTransport bool
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
		converter: opts.Converter, exchangeGrant: opts.ExchangeGrant, acceptGrant: opts.AcceptGrant,
		loadClusterKEK: opts.LoadClusterKEK, recordSync: opts.RecordSyncBinding, recordTrust: opts.RecordPrimaryTrust, archiveTTL: archiveTTL, applyWaitTimeout: applyWaitTimeout,
		certificate: certificate, logger: opts.Logger,
	}
}

// workflowSentinels names the package sentinels the workflow can surface in
// addition to enrollment.KnownCauseSentinels.
var workflowSentinels = []enrollment.NamedError{
	{Name: "ErrEnrollmentWorkflowUnavailable", Err: ErrEnrollmentWorkflowUnavailable},
	{Name: "ErrEnrollmentWorkflowDenied", Err: ErrEnrollmentWorkflowDenied},
	{Name: "ErrExchangeDenied", Err: syncpublication.ErrExchangeDenied},
	{Name: "ErrExchangeInvalid", Err: syncpublication.ErrExchangeInvalid},
	{Name: "ErrExchangeRateLimited", Err: syncpublication.ErrExchangeRateLimited},
	{Name: "ErrExchangeUnavailable", Err: syncpublication.ErrExchangeUnavailable},
	{Name: "ErrSnapshotClientUntrusted", Err: syncpublication.ErrSnapshotClientUntrusted},
	{Name: "ErrSnapshotClientUnauthorized", Err: syncpublication.ErrSnapshotClientUnauthorized},
	{Name: "ErrSnapshotClientUnavailable", Err: syncpublication.ErrSnapshotClientUnavailable},
	{Name: "ErrSnapshotClientInvalid", Err: syncpublication.ErrSnapshotClientInvalid},
	{Name: "ErrNodeCredentialDenied", Err: syncpublication.ErrNodeCredentialDenied},
}

// causeDetail renders only sentinel names and the Go type chain of err, never
// its text, so tokens, keys or payloads inside wrapped messages cannot leak.
func causeDetail(err error) string {
	return "cause=" + enrollment.RedactedCause(err, workflowSentinels...)
}

// reject logs one redacted line (operation, step, stable reason code, HTTP
// status and optional safe detail) and writes the unchanged generic response.
// detail must never carry error text, tokens, keys or payloads.
func (h *enrollmentWorkflowHandler) reject(w http.ResponseWriter, op, step, reason string, status int, message, detail string) {
	if h.logger != nil {
		line := fmt.Sprintf("enrollment %s rejected step=%s reason=%s status=%d", op, step, reason, status)
		if detail != "" {
			line += " " + detail
		}
		h.logger.Print(line)
	}
	writeIdentityProofError(w, status, message)
}

func (h *enrollmentWorkflowHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL == nil || r.URL.RawPath != "" || r.URL.RawQuery != "" || r.URL.ForceQuery {
		h.reject(w, "request", "route", "path_invalid", http.StatusNotFound, "not found", "")
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		h.reject(w, "request", "route", "method_not_allowed", http.StatusMethodNotAllowed, "method not allowed", "")
		return
	}
	if !h.trustedTransport && (r.TLS == nil || !r.TLS.HandshakeComplete || r.TLS.Version < tls.VersionTLS13) {
		h.reject(w, "request", "transport", "secure_transport_required", http.StatusBadRequest, "secure transport required", "")
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
		h.reject(w, "request", "route", "path_unknown", http.StatusNotFound, "not found", "")
	}
}

func (h *enrollmentWorkflowHandler) handleDraft(w http.ResponseWriter, r *http.Request) {
	const op = "draft"
	request, ok := decodeEnrollmentWorkflowBody[enrollmentDraftRequest](h, w, r, op)
	if !ok {
		return
	}
	primaryURL, requestCA, wireToken, embedded, ok := resolveDraftRoute(request)
	if !ok {
		h.reject(w, op, "route", "token_route_mismatch", http.StatusBadRequest, "invalid enrollment draft request", "")
		return
	}
	canonicalURL, err := enrollment.CanonicalizePrimaryURL(primaryURL)
	if err != nil || canonicalURL != primaryURL {
		h.reject(w, op, "route", "primary_url_invalid", http.StatusBadRequest, "invalid enrollment draft request", causeDetail(err))
		return
	}
	if !h.requireTLSAuthority(w, r, canonicalURL, op) {
		return
	}
	selector, valid := h.acceptToken(r.Context(), request.Token)
	if !valid {
		h.reject(w, op, "token", "token_denied", http.StatusForbidden, "Permission denied", "")
		return
	}
	if h.cache == nil || h.fetchSnapshot == nil || h.validator == nil || h.identity == nil {
		h.reject(w, op, "workflow", "workflow_unavailable", http.StatusServiceUnavailable, "enrollment workflow unavailable",
			"missing="+h.missingDraftDependencies())
		return
	}
	ctx := r.Context()
	caPEM := ""
	if strings.TrimSpace(requestCA) != "" {
		normalized, _, err := configuration.NormalizePrimaryTrustCA(requestCA)
		if err != nil {
			h.reject(w, op, "ca", "ca_invalid", http.StatusBadRequest, "invalid CA certificate", causeDetail(err))
			return
		}
		caPEM = normalized
		// A non-empty CA is the only anchor for this origin: the transport
		// builds a pool from it alone, never the system roots.
		ctx = syncpublication.WithTrustOverride(ctx, primaryOrigin(canonicalURL), caPEM)
	}
	envelope, err := h.fetchSnapshot(ctx, canonicalURL, wireToken)
	if errors.Is(err, syncpublication.ErrSnapshotClientUntrusted) {
		h.reject(w, op, "fetch", "primary_untrusted", http.StatusConflict, "primary certificate not trusted; load the primary CA", causeDetail(err))
		return
	}
	if err != nil || envelope == nil {
		h.reject(w, op, "fetch", "snapshot_unavailable", http.StatusConflict, "primary snapshot unavailable", causeDetail(err))
		return
	}
	if ok, detail := h.validatePreviewEnvelope(r, envelope); !ok {
		h.reject(w, op, "validate", "snapshot_denied", http.StatusConflict, "primary snapshot denied", detail)
		return
	}
	overlay, err := h.localOverlay(r.Context())
	if err != nil {
		h.reject(w, op, "overlay", "local_identity_unavailable", http.StatusConflict, "local identity unavailable", overlayDetail(err))
		return
	}
	draft, err := h.cache.PutWithPrimaryURLAndToken(selector, canonicalURL, envelope, overlay, wireToken)
	if err != nil {
		h.reject(w, op, "cache", "draft_unavailable", http.StatusConflict, "enrollment draft unavailable", causeDetail(err))
		return
	}
	if caPEM != "" {
		if h.cache.AttachPrimaryCA(draft.DraftID, caPEM) {
			draft.PrimaryCAPEM = caPEM
		} else if embedded {
			// A token-carried CA is mandatory: never leave a draft that
			// could confirm without it.
			h.cache.DeleteByID(draft.DraftID)
			h.reject(w, op, "cache", "ca_attach_failed", http.StatusConflict, "enrollment draft unavailable", "")
			return
		}
	}
	h.writePreview(w, http.StatusOK, draft, overlay)
}

func (h *enrollmentWorkflowHandler) missingDraftDependencies() string {
	var missing []string
	if h.cache == nil {
		missing = append(missing, "cache")
	}
	if h.fetchSnapshot == nil {
		missing = append(missing, "fetch_snapshot")
	}
	if h.validator == nil {
		missing = append(missing, "validator")
	}
	if h.identity == nil {
		missing = append(missing, "identity")
	}
	return strings.Join(missing, ",")
}

// resolveDraftRoute decides the primary URL, CA and wire token of a draft. A v1
// token behaves exactly as before (URL required from the request, CA optional).
// For a v2 token the URL and CA come from the token; when the request also
// carries them they must match after canonicalization, never silently
// preferring the typed value. The wire token is always the v1 spelling of
// selector+secret, so the primary never receives the embedded URL/CA.
func resolveDraftRoute(request enrollmentDraftRequest) (primaryURL, caPEM, wireToken string, embedded, ok bool) {
	claims, parsed := enrollment.ParseTokenClaims(request.Token)
	if !parsed || claims.Version != 2 {
		for index := range claims.Secret {
			claims.Secret[index] = 0
		}
		return request.PrimaryURL, request.CAPEM, request.Token, false, true
	}
	defer zeroBytes(claims.Secret)
	if request.PrimaryURL != "" {
		canonical, err := enrollment.CanonicalizePrimaryURL(request.PrimaryURL)
		if err != nil || canonical != claims.PrimaryURL {
			return "", "", "", true, false
		}
	}
	if strings.TrimSpace(request.CAPEM) != "" {
		normalized, _, err := configuration.NormalizePrimaryTrustCA(request.CAPEM)
		if err != nil || normalized != claims.CAPEM {
			return "", "", "", true, false
		}
	}
	return claims.PrimaryURL, claims.CAPEM, claims.LegacyWire(), true, true
}

func (h *enrollmentWorkflowHandler) handlePreview(w http.ResponseWriter, r *http.Request) {
	const op = "preview"
	request, ok := decodeEnrollmentWorkflowBody[enrollmentPreviewRequest](h, w, r, op)
	if !ok {
		return
	}
	if request.DraftID == "" {
		h.reject(w, op, "decode", "draft_id_missing", http.StatusBadRequest, "invalid enrollment preview request", "")
		return
	}
	if h.cache == nil {
		h.reject(w, op, "draft_lookup", "draft_expired", http.StatusGone, "enrollment draft expired; re-enter the token", "cache=nil")
		return
	}
	draft, ok := h.cache.GetByID(request.DraftID)
	if !ok {
		h.reject(w, op, "draft_lookup", "draft_expired", http.StatusGone, "enrollment draft expired; re-enter the token", "")
		return
	}
	if !h.requireTLSAuthority(w, r, draft.PrimaryURL, op) {
		return
	}
	if h.validator == nil || h.identity == nil {
		h.reject(w, op, "workflow", "workflow_unavailable", http.StatusServiceUnavailable, "enrollment workflow unavailable",
			fmt.Sprintf("validator_nil=%t identity_nil=%t", h.validator == nil, h.identity == nil))
		return
	}
	overlay, err := h.localOverlay(r.Context())
	if err != nil {
		h.reject(w, op, "overlay", "overlay_unavailable", http.StatusConflict, "requested node values are not authoritative", overlayDetail(err))
		return
	}
	if !requestedOverlayMatches(request.RequestedNodeID, request.RequestedIngress, overlay) {
		h.reject(w, op, "overlay", "overlay_mismatch", http.StatusConflict, "requested node values are not authoritative", "")
		return
	}
	if ok, detail := h.validatePreviewEnvelope(r, draft.Envelope); !ok {
		h.reject(w, op, "validate", "snapshot_denied", http.StatusConflict, "primary snapshot denied", detail)
		return
	}
	h.writePreview(w, http.StatusOK, draft, overlay)
}

func (h *enrollmentWorkflowHandler) handleConfirm(w http.ResponseWriter, r *http.Request) {
	const op = "confirm"
	request, ok := decodeEnrollmentWorkflowBody[enrollmentConfirmRequest](h, w, r, op)
	if !ok {
		return
	}
	if request.DraftID == "" {
		h.reject(w, op, "decode", "draft_id_missing", http.StatusBadRequest, "invalid enrollment confirm request", "")
		return
	}
	if h.cache == nil {
		h.reject(w, op, "draft_lookup", "draft_expired", http.StatusGone, "enrollment draft expired; re-enter the token", "cache=nil")
		return
	}
	draft, ok := h.cache.GetByID(request.DraftID)
	if !ok {
		h.reject(w, op, "draft_lookup", "draft_expired", http.StatusGone, "enrollment draft expired; re-enter the token", "")
		return
	}
	if !h.requireTLSAuthority(w, r, draft.PrimaryURL, op) {
		return
	}
	if h.converter == nil || h.validator == nil || h.identity == nil {
		h.reject(w, op, "workflow", "converter_unavailable", http.StatusConflict, "enrollment conversion denied",
			fmt.Sprintf("converter_nil=%t validator_nil=%t identity_nil=%t", h.converter == nil, h.validator == nil, h.identity == nil))
		return
	}
	if h.exchangeGrant == nil || h.acceptGrant == nil || h.loadClusterKEK == nil {
		h.reject(w, op, "workflow", "workflow_unavailable", http.StatusServiceUnavailable, "enrollment workflow unavailable",
			fmt.Sprintf("exchange_nil=%t accept_nil=%t kek_loader_nil=%t", h.exchangeGrant == nil, h.acceptGrant == nil, h.loadClusterKEK == nil))
		return
	}
	kekValidator, ok := h.validator.(kekEnvelopeValidator)
	if !ok {
		h.reject(w, op, "workflow", "validator_without_kek_support", http.StatusServiceUnavailable, "enrollment workflow unavailable", "")
		return
	}
	// One confirm per draft at a time: the grant is single-use and the retained
	// recipient key must not be raced.
	if _, busy := h.confirming.LoadOrStore(request.DraftID, struct{}{}); busy {
		h.reject(w, op, "lock", "confirm_in_progress", http.StatusConflict, "enrollment confirm already in progress", "")
		return
	}
	defer h.confirming.Delete(request.DraftID)

	token, recipient, ok := h.cache.AttemptSecrets(request.DraftID)
	if !ok {
		h.reject(w, op, "attempt_secrets", "draft_expired", http.StatusGone, "enrollment draft expired; re-enter the token", "attempt_secrets=missing")
		return
	}
	defer recipient.Destroy()
	local, err := h.identity(r.Context())
	if err != nil {
		h.reject(w, op, "identity", "identity_lookup_failed", http.StatusConflict, "identity transition denied", causeDetail(err))
		return
	}
	if !local.NodeID.IsValid() || !local.InstallationID.IsValid() || local.NodeID != draft.Overlay.NodeID {
		h.reject(w, op, "identity", "identity_mismatch", http.StatusConflict, "identity transition denied",
			fmt.Sprintf("node_valid=%t installation_valid=%t node_matches_draft=%t", local.NodeID.IsValid(), local.InstallationID.IsValid(), local.NodeID == draft.Overlay.NodeID))
		return
	}
	// Eligibility is evaluated per request (never at boot) so a demoted
	// installation can enroll without a restart, and before the exchange so a
	// refused identity never consumes a token.
	if !identity.RoleCanEnrollAsNode(local.Role, local.ClusterKeyID != "") && !h.converterAllowsReEnroll(local) {
		reason := "role_ineligible"
		if local.Role == domain.TopologyRoleNode {
			reason = "node_not_unlinked"
		}
		h.reject(w, op, "eligibility", reason, http.StatusConflict, "enrollment conversion denied",
			fmt.Sprintf("role=%s key_present=%t", local.Role, local.ClusterKeyID != ""))
		return
	}
	ingress, err := h.currentIngress(r.Context())
	if err != nil {
		h.reject(w, op, "ingress", "ingress_unavailable", http.StatusConflict, "identity transition denied", causeDetail(err))
		return
	}
	binding, exchangeRequest, ok := h.confirmBinding(request.DraftID, draft, local, &recipient)
	if !ok {
		h.reject(w, op, "binding", "binding_invalid", http.StatusConflict, "enrollment grant denied", "")
		return
	}

	// The owner-loaded CA (if any) verifies every call to this primary below.
	ctx := r.Context()
	if draft.PrimaryCAPEM != "" {
		ctx = syncpublication.WithTrustOverride(ctx, primaryOrigin(draft.PrimaryURL), draft.PrimaryCAPEM)
	}
	// 1. exchange (the primary consumes the token and seals to our key)
	exchanged, err := h.exchangeGrant(ctx, draft.PrimaryURL, token, exchangeRequest)
	if err != nil {
		h.writeExchangeFailure(w, request.DraftID, err)
		return
	}
	// 2. open + persist KEK and credential atomically
	accepted, err := h.acceptGrant(r.Context(), &recipient, binding, exchanged.SealedPayload, exchanged.PayloadHash)
	if err != nil {
		if errors.Is(err, enrollment.ErrBootstrapPersistence) {
			h.reject(w, op, "accept", "accept_persistence_failed", http.StatusServiceUnavailable, "enrollment workflow unavailable", causeDetail(err))
			return
		}
		h.reject(w, op, "accept", "accept_denied", http.StatusConflict, "enrollment grant denied", causeDetail(err))
		return
	}
	// 3. strict validation with the KEK the grant just delivered
	keyID, err := uuid.Parse(accepted.ClusterKeyID)
	if err != nil {
		h.reject(w, op, "accept", "cluster_key_id_invalid", http.StatusConflict, "enrollment grant denied", "")
		return
	}
	kek, err := h.loadClusterKEK(r.Context(), keyID)
	if err != nil || kek == nil {
		h.reject(w, op, "kek_load", "kek_load_failed", http.StatusServiceUnavailable, "enrollment workflow unavailable",
			fmt.Sprintf("kek_nil=%t %s", kek == nil, causeDetail(err)))
		return
	}
	result := kekValidator.ValidateWithKEK(r.Context(), *draft.Envelope, kek)
	kek.Destroy()
	if !result.OK || len(result.Issues) != 0 {
		h.reject(w, op, "validate", "snapshot_denied", http.StatusConflict, "primary snapshot denied",
			fmt.Sprintf("ok=%t issues=%d", result.OK, len(result.Issues)))
		return
	}
	// 3a. persist the owner-loaded CA (replacing any previous one for this
	// origin) so continuous sync keeps verifying the primary. Failing closed
	// here avoids a node that enrolls but can never sync.
	if draft.PrimaryCAPEM != "" {
		if h.recordTrust == nil {
			h.reject(w, op, "record_trust", "record_trust_unavailable", http.StatusServiceUnavailable, "enrollment workflow unavailable", "recorder_nil=true")
			return
		}
		if err := h.recordTrust(r.Context(), primaryOrigin(draft.PrimaryURL), draft.PrimaryCAPEM); err != nil {
			h.reject(w, op, "record_trust", "record_trust_failed", http.StatusServiceUnavailable, "enrollment workflow unavailable", causeDetail(err))
			return
		}
	}
	// 3b. remember the primary before the role commit point, so a node can
	// never exist without the binding continuous sync needs. A failed convert
	// leaves a binding on a non-node, which the sync loop ignores.
	if h.recordSync != nil {
		binding := configuration.NodeSyncBinding{
			PrimaryURL:            draft.PrimaryURL,
			CredentialID:          accepted.CredentialID,
			PrimaryInstallationID: draft.Envelope.Transient.SourcePrimaryID.String(),
		}
		if err := h.recordSync(r.Context(), binding, draft.Envelope.ContentHash); err != nil {
			h.reject(w, op, "record_sync", "record_sync_failed", http.StatusServiceUnavailable, "enrollment workflow unavailable", causeDetail(err))
			return
		}
	}
	// 4. archive/import/apply, then the role commit point
	converted, err := h.converter.Convert(r.Context(), enrollment.NodeConversionInput{
		Envelope: draft.Envelope, LocalNodeID: local.NodeID, LocalIngress: ingress,
		LocalInstallId: local.InstallationID, ArchiveTTL: h.archiveTTL, ApplyWaitTimeout: h.applyWaitTimeout,
		ClusterKeyID: &keyID,
	})
	if err != nil {
		h.reject(w, op, "convert", "convert_failed", http.StatusConflict, genericConversionReason(err),
			"class="+conversionClass(err)+" "+causeDetail(err))
		return
	}
	result2 := converted
	h.cache.DeleteByID(request.DraftID)
	response := enrollmentConfirmResponse{
		Role: result2.Identity.Role, Generation: uint64(result2.Identity.LeadershipGeneration),
		NodeID: result2.Identity.NodeID.String(), ArchiveID: result2.ArchiveID.String(), ApplyJobID: result2.ApplyJobID.String(),
	}
	if result2.Identity.ClusterKeyID != nil {
		response.ClusterKeyID = result2.Identity.ClusterKeyID.String()
	}
	if h.logger != nil {
		h.logger.Printf("enrollment confirm committed role=%s generation=%d", response.Role, response.Generation)
	}
	writeWorkflowJSON(w, http.StatusOK, response)
}

// enrollmentEligibility is implemented by the production converter, which sees
// the live identity including the "explicitly unlinked" overlay the handler's
// own role/key rule cannot know.
type enrollmentEligibility interface {
	CanEnrollAsNode() bool
}

// converterAllowsReEnroll lets only a NODE through the exception, and only when
// the converter itself (live identity) says it is eligible, which for a NODE
// holding its cluster key means the primary explicitly unlinked it.
func (h *enrollmentWorkflowHandler) converterAllowsReEnroll(local EnrollmentLocalIdentity) bool {
	if local.Role != domain.TopologyRoleNode {
		return false
	}
	eligible, ok := h.converter.(enrollmentEligibility)
	return ok && eligible.CanEnrollAsNode()
}

// confirmBinding derives the grant binding deterministically from the draft so
// a retry after a transient failure re-requests the exact same grant:
//   - attemptId is the draft id (a canonical UUID minted once per draft);
//   - previewDigest is SHA-256 over a domain tag, the envelope content hash,
//     the canonical primary URL, the local node id and the local installation
//     id, i.e. exactly what the owner previewed and nothing time-varying;
//   - the recipient key is the draft's retained ephemeral key.
func (h *enrollmentWorkflowHandler) confirmBinding(draftID string, draft enrollment.Draft, local EnrollmentLocalIdentity, recipient *enrollment.BootstrapRecipient) (enrollment.BootstrapBinding, syncpublication.ExchangeRequest, bool) {
	envelope := draft.Envelope
	if envelope == nil || envelope.Transient.SourcePrimaryID == uuid.Nil || envelope.Transient.LeadershipGeneration == 0 || envelope.ContentHash == "" {
		return enrollment.BootstrapBinding{}, syncpublication.ExchangeRequest{}, false
	}
	if parsed, err := uuid.Parse(draftID); err != nil || parsed.String() != draftID {
		return enrollment.BootstrapBinding{}, syncpublication.ExchangeRequest{}, false
	}
	publicKey, err := recipient.PublicKey()
	if err != nil {
		return enrollment.BootstrapBinding{}, syncpublication.ExchangeRequest{}, false
	}
	wire, err := publicKey.Wire()
	if err != nil {
		return enrollment.BootstrapBinding{}, syncpublication.ExchangeRequest{}, false
	}
	hasher := sha256.New()
	for _, part := range []string{previewDigestDomain, envelope.ContentHash, draft.PrimaryURL, local.NodeID.String(), local.InstallationID.String()} {
		var length [4]byte
		length[0], length[1], length[2], length[3] = byte(len(part)>>24), byte(len(part)>>16), byte(len(part)>>8), byte(len(part))
		hasher.Write(length[:])
		hasher.Write([]byte(part))
	}
	digest := hasher.Sum(nil)
	binding := enrollment.BootstrapBinding{
		ProtocolVersion: enrollment.BootstrapProtocolVersion, AttemptID: draftID,
		SourcePrimaryInstallationID: envelope.Transient.SourcePrimaryID.String(),
		TargetInstallationID:        local.InstallationID.String(), TargetNodeID: local.NodeID.String(),
		LeadershipGeneration: uint64(envelope.Transient.LeadershipGeneration), VerifiedPreviewDigest: digest,
	}
	return binding, syncpublication.ExchangeRequest{
		AttemptID: binding.AttemptID, InstallationID: binding.TargetInstallationID, NodeID: binding.TargetNodeID,
		RecipientPublicKey: json.RawMessage(wire), PreviewDigest: hex.EncodeToString(digest),
		PrimaryInstallationID: binding.SourcePrimaryInstallationID, LeadershipGeneration: binding.LeadershipGeneration,
	}, true
}

// writeExchangeFailure maps a transport outcome to a generic response. The
// error text is never forwarded: it may be attacker-influenced. A definitive
// denial means the token is dead, so it is dropped from memory immediately.
func (h *enrollmentWorkflowHandler) writeExchangeFailure(w http.ResponseWriter, draftID string, err error) {
	const op, step = "confirm", "exchange"
	var limited *syncpublication.ExchangeRateLimitedError
	switch {
	case errors.As(err, &limited):
		if seconds := int64(limited.RetryAfter / time.Second); seconds > 0 {
			w.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
		}
		h.reject(w, op, step, "exchange_rate_limited", http.StatusTooManyRequests, "enrollment rate limited", causeDetail(err))
	case errors.Is(err, syncpublication.ErrExchangeRateLimited):
		h.reject(w, op, step, "exchange_rate_limited", http.StatusTooManyRequests, "enrollment rate limited", causeDetail(err))
	case errors.Is(err, syncpublication.ErrExchangeUnavailable):
		h.reject(w, op, step, "exchange_unavailable", http.StatusServiceUnavailable, "enrollment workflow unavailable", causeDetail(err))
	default: // denied, invalid and anything unknown fail closed
		reason := "exchange_unknown"
		switch {
		case errors.Is(err, syncpublication.ErrExchangeDenied):
			reason = "exchange_denied"
		case errors.Is(err, syncpublication.ErrExchangeInvalid):
			reason = "exchange_invalid"
		}
		h.cache.DeleteByID(draftID)
		h.reject(w, op, step, reason, http.StatusConflict, "enrollment grant denied", causeDetail(err))
	}
}

func (h *enrollmentWorkflowHandler) handleRecover(w http.ResponseWriter, r *http.Request) {
	const op = "recover"
	request, ok := decodeEnrollmentWorkflowBody[enrollmentRecoverRequest](h, w, r, op)
	if !ok {
		return
	}
	selector, valid := h.acceptToken(r.Context(), request.Token)
	if !valid || h.cache == nil {
		h.reject(w, op, "token", "token_denied_or_no_cache", http.StatusGone, "enrollment draft expired; re-enter the token", fmt.Sprintf("token_valid=%t cache_nil=%t", valid, h.cache == nil))
		return
	}
	draft, ok := h.cache.GetBySelector(selector)
	if !ok {
		h.reject(w, op, "draft_lookup", "draft_expired", http.StatusGone, "enrollment draft expired; re-enter the token", "")
		return
	}
	if !h.requireTLSAuthority(w, r, draft.PrimaryURL, op) {
		return
	}
	if h.validator == nil || h.identity == nil {
		h.reject(w, op, "workflow", "workflow_unavailable", http.StatusConflict, "enrollment preview unavailable",
			fmt.Sprintf("validator_nil=%t identity_nil=%t", h.validator == nil, h.identity == nil))
		return
	}
	if ok, detail := h.validatePreviewEnvelope(r, draft.Envelope); !ok {
		h.reject(w, op, "validate", "snapshot_denied", http.StatusConflict, "enrollment preview unavailable", detail)
		return
	}
	overlay, err := h.localOverlay(r.Context())
	if err != nil {
		h.reject(w, op, "overlay", "overlay_unavailable", http.StatusConflict, "enrollment preview unavailable", overlayDetail(err))
		return
	}
	h.writePreview(w, http.StatusOK, draft, overlay)
}

// previewEnvelopeValidator is implemented by validators that can check a
// snapshot for a read-only preview before the cluster KEK is available.
type previewEnvelopeValidator interface {
	ValidatePreview(context.Context, replicationsnapshot.Envelope) replicationsnapshot.ValidationResult
}

func (h *enrollmentWorkflowHandler) validateEnvelope(r *http.Request, envelope *replicationsnapshot.Envelope) bool {
	ok, _ := h.validate(r, envelope, false)
	return ok
}

func (h *enrollmentWorkflowHandler) validatePreviewEnvelope(r *http.Request, envelope *replicationsnapshot.Envelope) (bool, string) {
	return h.validate(r, envelope, true)
}

// validate reports the outcome plus a log-safe detail: only the issue COUNT is
// exposed, never issue contents.
func (h *enrollmentWorkflowHandler) validate(r *http.Request, envelope *replicationsnapshot.Envelope, preview bool) (bool, string) {
	if envelope == nil || h.validator == nil {
		return false, fmt.Sprintf("envelope_nil=%t validator_nil=%t", envelope == nil, h.validator == nil)
	}
	var result replicationsnapshot.ValidationResult
	if previewer, ok := h.validator.(previewEnvelopeValidator); ok && preview {
		result = previewer.ValidatePreview(r.Context(), *envelope)
	} else {
		result = h.validator.Validate(r.Context(), *envelope)
	}
	return result.OK && len(result.Issues) == 0, fmt.Sprintf("ok=%t issues=%d", result.OK, len(result.Issues))
}

// overlayError labels which part of the local overlay lookup failed while
// keeping the underlying error reachable.
type overlayError struct {
	stage string
	err   error
}

func (e *overlayError) Error() string { return "local overlay " + e.stage + ": " + e.err.Error() }
func (e *overlayError) Unwrap() error { return e.err }
func (e *overlayError) Is(target error) bool {
	return e.stage != "ingress" && target == ErrEnrollmentWorkflowUnavailable
}

func overlayDetail(err error) string {
	stage := "unknown"
	var failure *overlayError
	if errors.As(err, &failure) {
		stage = failure.stage
	}
	return "stage=" + stage + " " + causeDetail(err)
}

func (h *enrollmentWorkflowHandler) localOverlay(ctx context.Context) (enrollment.NodeLocalOverlay, error) {
	if h.identity == nil || ctx == nil {
		return enrollment.NodeLocalOverlay{}, &overlayError{stage: "unavailable", err: ErrEnrollmentWorkflowUnavailable}
	}
	local, err := h.identity(ctx)
	if err != nil {
		return enrollment.NodeLocalOverlay{}, &overlayError{stage: "identity_lookup", err: err}
	}
	if !local.NodeID.IsValid() {
		return enrollment.NodeLocalOverlay{}, &overlayError{stage: "identity_node_invalid", err: ErrEnrollmentWorkflowUnavailable}
	}
	ingress, err := h.currentIngress(ctx)
	if err != nil {
		return enrollment.NodeLocalOverlay{}, &overlayError{stage: "ingress", err: err}
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
	response := enrollmentPreviewResponse{
		DraftID: draft.DraftID, EnvelopePreview: newEnvelopePreview(draft.Envelope, overlay),
		NodeLocalOverlay: overlay, ExpiresAt: draft.ExpiresAt,
	}
	if draft.PrimaryCAPEM != "" {
		if _, fingerprint, err := configuration.NormalizePrimaryTrustCA(draft.PrimaryCAPEM); err == nil {
			response.PrimaryCAFingerprint = fingerprint
		}
	}
	writeWorkflowJSON(w, status, response)
}

// primaryOrigin reduces a canonical primary URL to its "https://host:port"
// trust key.
func primaryOrigin(primaryURL string) string {
	parsed, err := url.Parse(primaryURL)
	if err != nil || parsed.Host == "" {
		return ""
	}
	return "https://" + strings.ToLower(parsed.Host)
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

// acceptToken validates the token shape and returns its selector. The token is
// minted and owned by the primary, so on the owner-session main API (trusted
// transport) the node never consults its own token store: it forwards the token
// and the primary verifies and consumes it. The dedicated unauthenticated
// listener keeps requiring a locally verifiable token as its only gate.
func (h *enrollmentWorkflowHandler) acceptToken(ctx context.Context, token string) (string, bool) {
	selector, secret, valid := parseHTTPEnrollmentToken(token)
	zeroBytes(secret)
	if !valid {
		return "", false
	}
	if h.trustedTransport {
		return selector, true
	}
	if h.verifyToken == nil || h.verifyToken(ctx, token) != nil {
		return "", false
	}
	return selector, true
}

func (h *enrollmentWorkflowHandler) requireTLSAuthority(w http.ResponseWriter, r *http.Request, primaryURL, op string) bool {
	if h.trustedTransport {
		return true
	}
	if r.TLS == nil || !r.TLS.HandshakeComplete || r.TLS.Version < tls.VersionTLS13 {
		h.reject(w, op, "tls_authority", "secure_transport_required", http.StatusBadRequest, "secure transport required", "")
		return false
	}
	parsed, err := url.Parse(primaryURL)
	if err != nil || parsed.Host == "" || r.Host != parsed.Host || !identityProofSNIMatches(parsed.Hostname(), r.TLS.ServerName) {
		h.reject(w, op, "tls_authority", "authority_mismatch", http.StatusMisdirectedRequest, "invalid enrollment authority", "")
		return false
	}
	if h.certificate == nil {
		h.reject(w, op, "tls_authority", "certificate_provider_missing", http.StatusServiceUnavailable, "enrollment workflow unavailable", "")
		return false
	}
	certificate, err := h.certificate(r.Context())
	if err != nil || certificate == nil || certificate.VerifyHostname(parsed.Hostname()) != nil {
		h.reject(w, op, "tls_authority", "certificate_invalid", http.StatusMisdirectedRequest, "invalid enrollment authority", causeDetail(err))
		return false
	}
	return true
}

// conversionClass names the class of a Convert failure for logs. It is the
// diagnostic twin of genericConversionReason, which hides the class from the
// HTTP response.
func conversionClass(err error) string {
	switch {
	case errors.Is(err, enrollment.ErrNodeConversionAborted):
		return "aborted"
	case errors.Is(err, enrollment.ErrNodeConversionDenied):
		return "denied"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, enrollment.ErrNodeConversionImport):
		return "import"
	case errors.Is(err, enrollment.ErrNodeConversionApplyWait):
		return "apply_wait"
	case errors.Is(err, enrollment.ErrNodeConversionUnavailable):
		return "unavailable"
	default:
		return "other"
	}
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

func decodeEnrollmentWorkflowBody[T any](h *enrollmentWorkflowHandler, w http.ResponseWriter, r *http.Request, op string) (T, bool) {
	var value T
	if r.ContentLength > maxEnrollmentWorkflowBodyBytes {
		h.reject(w, op, "decode", "body_too_large", http.StatusBadRequest, "invalid enrollment workflow request", "")
		return value, false
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxEnrollmentWorkflowBodyBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		h.reject(w, op, "decode", "body_invalid", http.StatusBadRequest, "invalid enrollment workflow request", causeDetail(err))
		return value, false
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		h.reject(w, op, "decode", "body_trailing_data", http.StatusBadRequest, "invalid enrollment workflow request", "")
		return value, false
	}
	return value, true
}

var _ EnrollmentSnapshotValidator = (*replicationsnapshot.Validator)(nil)
