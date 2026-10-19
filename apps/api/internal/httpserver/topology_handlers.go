package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/auth"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/identity"
)

const (
	TopologyPromotePath = "/api/topology/promote"
	TopologyDemotePath  = "/api/topology/demote"

	maxTopologyBody = 4 << 10

	// promotePublicationAttempts bounds the best-effort enqueue of the apply
	// that publishes the promoted node's configuration as a snapshot.
	promotePublicationAttempts     = 3
	promotePublicationRetryBackoff = 100 * time.Millisecond

	splitBrainWarningCode    = "SPLIT_BRAIN_RISK"
	splitBrainWarningMessage = "This installation is now PRIMARY at a higher leadership generation. " +
		"If the previous primary is still running it must not accept writes; stop it or convert it to NODE. " +
		"Nodes of the previous primary were not repointed and must be re-enrolled against this installation."
)

// topologyRequest is the shared body of promote and demote. Confirmation is a
// mandatory explicit boolean; ExpectedGeneration, when present, must match the
// durable leadership generation the caller last observed.
type topologyRequest struct {
	Confirm            *bool   `json:"confirm"`
	ExpectedGeneration *uint64 `json:"expectedGeneration,omitempty"`
}

func (s *Server) decodeTopologyRequest(w http.ResponseWriter, r *http.Request) (topologyRequest, bool) {
	var request topologyRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxTopologyBody))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid topology request", "code": "INVALID_REQUEST"})
		return topologyRequest{}, false
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid topology request", "code": "INVALID_REQUEST"})
		return topologyRequest{}, false
	}
	if request.Confirm == nil || !*request.Confirm {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "explicit confirmation required", "code": "CONFIRMATION_REQUIRED"})
		return topologyRequest{}, false
	}
	return request, true
}

func (r topologyRequest) expectedGeneration() *domain.LeadershipGeneration {
	if r.ExpectedGeneration == nil {
		return nil
	}
	generation := domain.LeadershipGeneration(*r.ExpectedGeneration)
	return &generation
}

// handleTopologyPromote converts NODE (or a stale-primary) into PRIMARY. It
// authenticates without the ordinary-mutation writability gate because the
// installation is, by definition, not writable before promotion.
func (s *Server) handleTopologyPromote(w http.ResponseWriter, r *http.Request) {
	setNoStore(w)
	user, ok := s.requireUserReadOnly(w, r, auth.RoleOwner)
	if !ok {
		return
	}
	svc := s.identityService()
	if svc == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "topology identity unavailable", "code": "IDENTITY_UNAVAILABLE"})
		return
	}
	request, ok := s.decodeTopologyRequest(w, r)
	if !ok {
		return
	}
	// Read the sync state first: promotion clears the node's sync binding.
	syncWarning := s.promoteSyncWarning(r.Context())
	result, err := svc.Promote(r.Context(), identity.PromoteOptions{
		ActorUserID:        user.ID,
		ExpectedGeneration: request.expectedGeneration(),
	})
	if err != nil {
		s.writeTopologyError(w, "promote", err)
		return
	}
	response := map[string]any{
		"ok":                 true,
		"identity":           s.publicIdentity(),
		"previousRole":       string(result.PreviousRole),
		"previousGeneration": result.PreviousGeneration,
		"revokedCredentials": result.RevokedCredentials,
		"nodesMustReenroll":  true,
		"warning": map[string]any{
			"code":    splitBrainWarningCode,
			"message": splitBrainWarningMessage,
		},
	}
	if syncWarning != nil {
		response["syncWarning"] = syncWarning
	}
	if result.ClusterKeyErr != nil {
		// The key is then minted on first use; publication below keeps retrying.
		s.log.Printf("topology promote: cluster key deferred: %v", result.ClusterKeyErr)
	}
	response["publication"] = s.enqueuePromotePublication(r.Context(), user.ID)
	writeJSON(w, http.StatusOK, response)
}

// enqueuePromotePublication enqueues an apply of the node's current
// configuration so the canonical publication pipeline publishes it as the
// snapshot of the new generation (the node's desired state equals what it last
// applied). It is best effort with a short bounded retry: a failure never
// undoes the promote, it is logged and reported in the response, and the next
// ordinary apply publishes anyway.
func (s *Server) enqueuePromotePublication(ctx context.Context, actorID string) map[string]any {
	if s.config == nil {
		return map[string]any{"status": "unavailable"}
	}
	var lastErr error
	for attempt := 1; attempt <= promotePublicationAttempts; attempt++ {
		apply, err := s.config.CreateApplyJob(ctx, actorID)
		if err == nil {
			noteApply(ctx, apply)
			return map[string]any{"status": "queued", "applyJobId": apply.Job.ID}
		}
		lastErr = err
		if attempt == promotePublicationAttempts {
			break
		}
		select {
		case <-ctx.Done():
			attempt = promotePublicationAttempts
		case <-time.After(time.Duration(attempt) * promotePublicationRetryBackoff):
		}
	}
	s.log.Printf("topology promote: snapshot publication was not enqueued: %v", lastErr)
	return map[string]any{
		"status":  "failed",
		"code":    "PUBLICATION_ENQUEUE_FAILED",
		"message": "The promotion succeeded but the configuration snapshot was not queued for publication; apply the configuration to publish it.",
	}
}

// handleTopologyDemote converts PRIMARY into NODE after revoking the
// credentials of every node this primary enrolled. The enrollment draft,
// preview and confirm flow then attaches the installation to a new primary.
func (s *Server) handleTopologyDemote(w http.ResponseWriter, r *http.Request) {
	setNoStore(w)
	user, ok := s.requireUserReadOnly(w, r, auth.RoleOwner)
	if !ok {
		return
	}
	svc := s.identityService()
	if svc == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "topology identity unavailable", "code": "IDENTITY_UNAVAILABLE"})
		return
	}
	request, ok := s.decodeTopologyRequest(w, r)
	if !ok {
		return
	}
	result, err := svc.Demote(r.Context(), identity.DemoteOptions{
		ActorUserID:        user.ID,
		ExpectedGeneration: request.expectedGeneration(),
	})
	if err != nil {
		s.writeTopologyError(w, "demote", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                 true,
		"identity":           s.publicIdentity(),
		"previousRole":       string(result.PreviousRole),
		"revokedCredentials": result.RevokedCredentials,
		"nextStep":           "enroll",
	})
}

func (s *Server) writeTopologyError(w http.ResponseWriter, operation string, err error) {
	switch {
	case errors.Is(err, identity.ErrGenerationMismatch):
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "error": "leadership generation changed; refresh and retry", "code": "GENERATION_MISMATCH"})
	case errors.Is(err, identity.ErrInvalidTransition):
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "error": "topology transition is not allowed from the current role", "code": "INVALID_TRANSITION"})
	case errors.Is(err, identity.ErrTopologyWorkflowUnavailable):
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "topology workflow unavailable", "code": "TOPOLOGY_UNAVAILABLE"})
	default:
		s.log.Printf("topology %s: %v", operation, err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "topology change failed", "code": "TOPOLOGY_CHANGE_FAILED"})
	}
}

// handleEnrollmentWorkflow authenticates an Owner and delegates to the shared
// enrollment workflow handler. It uses the read-only authentication path
// because a NODE (or about-to-be NODE) installation is not writable.
func (s *Server) handleEnrollmentWorkflow(w http.ResponseWriter, r *http.Request) {
	setNoStore(w)
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method not allowed", "code": "METHOD_NOT_ALLOWED"})
		return
	}
	if _, ok := s.requireUserReadOnly(w, r, auth.RoleOwner); !ok {
		return
	}
	if s.enrollmentWorkflow == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "enrollment workflow unavailable", "code": "ENROLLMENT_UNAVAILABLE"})
		return
	}
	s.enrollmentWorkflow.ServeHTTP(w, r)
}
