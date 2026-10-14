package httpserver

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
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
	if !h.trustedTransport && (r.TLS == nil || !r.TLS.HandshakeComplete || r.TLS.Version < tls.VersionTLS13) {
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
	selector, valid := h.acceptToken(r.Context(), request.Token)
	if !valid {
		writeIdentityProofError(w, http.StatusForbidden, "Permission denied")
		return
	}
	if h.cache == nil || h.fetchSnapshot == nil || h.validator == nil || h.identity == nil {
		writeIdentityProofError(w, http.StatusServiceUnavailable, "enrollment workflow unavailable")
		return
	}
	ctx := r.Context()
	caPEM := ""
	if strings.TrimSpace(request.CAPEM) != "" {
		normalized, _, err := configuration.NormalizePrimaryTrustCA(request.CAPEM)
		if err != nil {
			writeIdentityProofError(w, http.StatusBadRequest, "invalid CA certificate")
			return
		}
		caPEM = normalized
		ctx = syncpublication.WithTrustOverride(ctx, primaryOrigin(canonicalURL), caPEM)
	}
	envelope, err := h.fetchSnapshot(ctx, canonicalURL, request.Token)
	if errors.Is(err, syncpublication.ErrSnapshotClientUntrusted) {
		writeIdentityProofError(w, http.StatusConflict, "primary certificate not trusted; load the primary CA")
		return
	}
	if err != nil || envelope == nil {
		writeIdentityProofError(w, http.StatusConflict, "primary snapshot unavailable")
		return
	}
	if !h.validatePreviewEnvelope(r, envelope) {
		writeIdentityProofError(w, http.StatusConflict, "primary snapshot denied")
		return
	}
	overlay, err := h.localOverlay(r.Context())
	if err != nil {
		writeIdentityProofError(w, http.StatusConflict, "local identity unavailable")
		return
	}
	draft, err := h.cache.PutWithPrimaryURLAndToken(selector, canonicalURL, envelope, overlay, request.Token)
	if err != nil {
		writeIdentityProofError(w, http.StatusConflict, "enrollment draft unavailable")
		return
	}
	if caPEM != "" && h.cache.AttachPrimaryCA(draft.DraftID, caPEM) {
		draft.PrimaryCAPEM = caPEM
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
	if !h.validatePreviewEnvelope(r, draft.Envelope) {
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
	if h.exchangeGrant == nil || h.acceptGrant == nil || h.loadClusterKEK == nil {
		writeIdentityProofError(w, http.StatusServiceUnavailable, "enrollment workflow unavailable")
		return
	}
	kekValidator, ok := h.validator.(kekEnvelopeValidator)
	if !ok {
		writeIdentityProofError(w, http.StatusServiceUnavailable, "enrollment workflow unavailable")
		return
	}
	// One confirm per draft at a time: the grant is single-use and the retained
	// recipient key must not be raced.
	if _, busy := h.confirming.LoadOrStore(request.DraftID, struct{}{}); busy {
		writeIdentityProofError(w, http.StatusConflict, "enrollment confirm already in progress")
		return
	}
	defer h.confirming.Delete(request.DraftID)

	token, recipient, ok := h.cache.AttemptSecrets(request.DraftID)
	if !ok {
		writeIdentityProofError(w, http.StatusGone, "enrollment draft expired; re-enter the token")
		return
	}
	defer recipient.Destroy()
	local, err := h.identity(r.Context())
	if err != nil || !local.NodeID.IsValid() || !local.InstallationID.IsValid() || local.NodeID != draft.Overlay.NodeID {
		writeIdentityProofError(w, http.StatusConflict, "identity transition denied")
		return
	}
	// Eligibility is evaluated per request (never at boot) so a demoted
	// installation can enroll without a restart, and before the exchange so a
	// refused identity never consumes a token.
	if !identity.RoleCanEnrollAsNode(local.Role, local.ClusterKeyID != "") && !h.converterAllowsReEnroll(local) {
		writeIdentityProofError(w, http.StatusConflict, "enrollment conversion denied")
		return
	}
	ingress, err := h.currentIngress(r.Context())
	if err != nil {
		writeIdentityProofError(w, http.StatusConflict, "identity transition denied")
		return
	}
	binding, exchangeRequest, ok := h.confirmBinding(request.DraftID, draft, local, &recipient)
	if !ok {
		writeIdentityProofError(w, http.StatusConflict, "enrollment grant denied")
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
			writeIdentityProofError(w, http.StatusServiceUnavailable, "enrollment workflow unavailable")
			return
		}
		writeIdentityProofError(w, http.StatusConflict, "enrollment grant denied")
		return
	}
	// 3. strict validation with the KEK the grant just delivered
	keyID, err := uuid.Parse(accepted.ClusterKeyID)
	if err != nil {
		writeIdentityProofError(w, http.StatusConflict, "enrollment grant denied")
		return
	}
	kek, err := h.loadClusterKEK(r.Context(), keyID)
	if err != nil || kek == nil {
		writeIdentityProofError(w, http.StatusServiceUnavailable, "enrollment workflow unavailable")
		return
	}
	result := kekValidator.ValidateWithKEK(r.Context(), *draft.Envelope, kek)
	kek.Destroy()
	if !result.OK || len(result.Issues) != 0 {
		writeIdentityProofError(w, http.StatusConflict, "primary snapshot denied")
		return
	}
	// 3a. persist the owner-loaded CA (replacing any previous one for this
	// origin) so continuous sync keeps verifying the primary. Failing closed
	// here avoids a node that enrolls but can never sync.
	if draft.PrimaryCAPEM != "" {
		if h.recordTrust == nil || h.recordTrust(r.Context(), primaryOrigin(draft.PrimaryURL), draft.PrimaryCAPEM) != nil {
			writeIdentityProofError(w, http.StatusServiceUnavailable, "enrollment workflow unavailable")
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
			writeIdentityProofError(w, http.StatusServiceUnavailable, "enrollment workflow unavailable")
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
		writeIdentityProofError(w, http.StatusConflict, genericConversionReason(err))
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
	var limited *syncpublication.ExchangeRateLimitedError
	switch {
	case errors.As(err, &limited):
		if seconds := int64(limited.RetryAfter / time.Second); seconds > 0 {
			w.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
		}
		writeIdentityProofError(w, http.StatusTooManyRequests, "enrollment rate limited")
	case errors.Is(err, syncpublication.ErrExchangeRateLimited):
		writeIdentityProofError(w, http.StatusTooManyRequests, "enrollment rate limited")
	case errors.Is(err, syncpublication.ErrExchangeUnavailable):
		writeIdentityProofError(w, http.StatusServiceUnavailable, "enrollment workflow unavailable")
	default: // denied, invalid and anything unknown fail closed
		h.cache.DeleteByID(draftID)
		writeIdentityProofError(w, http.StatusConflict, "enrollment grant denied")
	}
}

func (h *enrollmentWorkflowHandler) handleRecover(w http.ResponseWriter, r *http.Request) {
	request, ok := decodeEnrollmentWorkflowBody[enrollmentRecoverRequest](w, r)
	if !ok {
		return
	}
	selector, valid := h.acceptToken(r.Context(), request.Token)
	if !valid || h.cache == nil {
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
	if h.validator == nil || h.identity == nil || !h.validatePreviewEnvelope(r, draft.Envelope) {
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

// previewEnvelopeValidator is implemented by validators that can check a
// snapshot for a read-only preview before the cluster KEK is available.
type previewEnvelopeValidator interface {
	ValidatePreview(context.Context, replicationsnapshot.Envelope) replicationsnapshot.ValidationResult
}

func (h *enrollmentWorkflowHandler) validateEnvelope(r *http.Request, envelope *replicationsnapshot.Envelope) bool {
	return h.validate(r, envelope, false)
}

func (h *enrollmentWorkflowHandler) validatePreviewEnvelope(r *http.Request, envelope *replicationsnapshot.Envelope) bool {
	return h.validate(r, envelope, true)
}

func (h *enrollmentWorkflowHandler) validate(r *http.Request, envelope *replicationsnapshot.Envelope, preview bool) bool {
	if envelope == nil || h.validator == nil {
		return false
	}
	var result replicationsnapshot.ValidationResult
	if previewer, ok := h.validator.(previewEnvelopeValidator); ok && preview {
		result = previewer.ValidatePreview(r.Context(), *envelope)
	} else {
		result = h.validator.Validate(r.Context(), *envelope)
	}
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

func (h *enrollmentWorkflowHandler) requireTLSAuthority(w http.ResponseWriter, r *http.Request, primaryURL string) bool {
	if h.trustedTransport {
		return true
	}
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
