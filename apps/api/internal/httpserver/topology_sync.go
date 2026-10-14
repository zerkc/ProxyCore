package httpserver

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/auth"
	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	syncpublication "github.com/zerkc/ProxyCore/apps/api/internal/sync"
)

const (
	TopologySyncPath      = "/api/topology/sync"
	TopologySyncRetryPath = "/api/topology/sync/retry"

	syncWarningOutdatedCode   = "NODE_OUTDATED"
	syncWarningNeverCode      = "NODE_NEVER_SYNCED"
	syncWarningUnverifiedCode = "NODE_SYNC_UNVERIFIED"
)

// TopologySyncOptions wires continuous-sync visibility and the manual trigger.
// Every field is optional; a missing seam degrades that feature, not the API.
type TopologySyncOptions struct {
	// LoadState returns the node's sync binding and progress (bound=false when
	// the installation has none).
	LoadState func(context.Context) (configuration.NodeSyncState, bool, error)
	// PublishedHash returns the hash of the snapshot a PRIMARY currently
	// publishes (ok=false when it has none yet).
	PublishedHash func(context.Context) (hash string, ok bool, err error)
	// Retry runs one sync cycle now (NodeSyncer.Retry).
	Retry func(context.Context) error
	// Interval is the poll period, used to judge whether the last successful
	// contact is recent.
	Interval time.Duration
}

// WithTopologySync mounts GET /api/topology/sync and POST
// /api/topology/sync/retry data sources, and lets promote warn about an
// unsynchronized node.
func WithTopologySync(opts TopologySyncOptions) Option {
	return func(s *Server) {
		copied := opts
		s.topologySync = &copied
	}
}

type topologySyncStatus struct {
	OK            bool    `json:"ok"`
	Role          string  `json:"role"`
	Bound         bool    `json:"bound"`
	AppliedHash   *string `json:"appliedHash"`
	PrimaryHash   *string `json:"primaryHash"`
	Outdated      bool    `json:"outdated"`
	LastSyncAt    *string `json:"lastSyncAt"`
	LastAttemptAt *string `json:"lastAttemptAt"`
	LastError     *string `json:"lastError"`
	NextAttemptAt *string `json:"nextAttemptAt"`
	FailureCount  int     `json:"consecutiveFailures"`
	Acknowledged  bool    `json:"acknowledged"`
	// Unlinked is true only after the primary explicitly signalled NODE_UNLINKED;
	// UnlinkedAt is when the node learned it.
	Unlinked        bool    `json:"unlinked"`
	UnlinkedAt      *string `json:"unlinkedAt"`
	PublishedSource string  `json:"hashSource"`
}

func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func optionalTime(t time.Time) *string {
	if t.IsZero() {
		return nil
	}
	return optionalString(t.UTC().Format(time.RFC3339Nano))
}

// topologySyncStatus assembles the status for the current role.
func (s *Server) topologySyncStatus(ctx context.Context) (topologySyncStatus, error) {
	status := topologySyncStatus{OK: true, PublishedSource: "none"}
	current, loaded := s.currentIdentityReadOnly()
	if loaded {
		status.Role = string(current.Role)
	}
	opts := s.topologySync
	if opts == nil {
		return status, nil
	}
	switch current.Role {
	case domain.TopologyRoleNode:
		if opts.LoadState == nil {
			return status, nil
		}
		state, bound, err := opts.LoadState(ctx)
		if err != nil {
			return status, err
		}
		status.Bound = bound
		if !bound {
			status.Outdated = true
			return status, nil
		}
		status.PublishedSource = "primary"
		status.AppliedHash = optionalString(state.AppliedHash)
		status.PrimaryHash = optionalString(state.PrimaryHash)
		status.Outdated = state.Outdated()
		status.LastSyncAt = optionalTime(state.LastSuccessAt)
		status.LastAttemptAt = optionalTime(state.LastAttemptAt)
		status.LastError = optionalString(state.LastError)
		status.NextAttemptAt = optionalTime(state.NextAttemptAt)
		status.FailureCount = state.ConsecutiveFailures
		status.Acknowledged = state.AckedHash != "" && state.AckedHash == state.AppliedHash
		status.Unlinked = state.Unlinked
		status.UnlinkedAt = optionalTime(state.UnlinkedAt)
	default:
		// Every non-node role may publish snapshots (standalone-primary,
		// primary, primary-with-nodes); a role that has none reports "none".
		if opts.PublishedHash == nil {
			return status, nil
		}
		hash, ok, err := opts.PublishedHash(ctx)
		if err != nil {
			return status, err
		}
		if ok {
			status.PublishedSource = "published"
			status.AppliedHash = optionalString(hash)
			status.PrimaryHash = optionalString(hash)
		}
	}
	return status, nil
}

func (s *Server) handleTopologySyncStatus(w http.ResponseWriter, r *http.Request) {
	setNoStore(w)
	if _, ok := s.requireUserReadOnly(w, r, auth.RoleOwner); !ok {
		return
	}
	status, err := s.topologySyncStatus(r.Context())
	if err != nil {
		s.log.Printf("topology sync status: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "sync status unavailable", "code": "SYNC_STATUS_UNAVAILABLE"})
		return
	}
	writeJSON(w, http.StatusOK, status)
}

// handleTopologySyncRetry runs one sync cycle now. Failure messages come from
// the sync loop's fixed vocabulary; raw error text never reaches the response.
func (s *Server) handleTopologySyncRetry(w http.ResponseWriter, r *http.Request) {
	setNoStore(w)
	if _, ok := s.requireUserReadOnly(w, r, auth.RoleOwner); !ok {
		return
	}
	if s.topologySync == nil || s.topologySync.Retry == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "node sync unavailable", "code": "SYNC_UNAVAILABLE"})
		return
	}
	err := s.topologySync.Retry(r.Context())
	var failure *syncpublication.NodeSyncFailure
	switch {
	case err == nil:
		status, statusErr := s.topologySyncStatus(r.Context())
		if statusErr != nil {
			s.log.Printf("topology sync status: %v", statusErr)
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "sync": status})
	case errors.Is(err, syncpublication.ErrNodeSyncNotNode):
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "error": "this installation is not a node", "code": "NOT_A_NODE"})
	case errors.Is(err, syncpublication.ErrNodeSyncUnbound):
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "error": "this node is not enrolled against a primary", "code": "NODE_NOT_BOUND"})
	case errors.Is(err, syncpublication.ErrNodeSyncBusy):
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "error": "a sync cycle is already running", "code": "SYNC_IN_PROGRESS"})
	case errors.As(err, &failure):
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": failure.Message, "code": failure.Code})
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "sync was interrupted", "code": "SYNC_INTERRUPTED"})
	default:
		s.log.Printf("topology sync retry failed")
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "sync failed", "code": "SYNC_FAILED"})
	}
}

// promoteSyncWarning inspects the node's sync state BEFORE a promotion (which
// clears it) and returns a non-blocking warning when the node may not hold the
// primary's latest configuration. It returns nil for a current node and for
// installations that are not a node.
func (s *Server) promoteSyncWarning(ctx context.Context) map[string]any {
	current, loaded := s.currentIdentityReadOnly()
	if !loaded || current.Role != domain.TopologyRoleNode {
		return nil
	}
	warn := func(code, message string) map[string]any {
		return map[string]any{"code": code, "message": message}
	}
	if s.topologySync == nil || s.topologySync.LoadState == nil {
		return warn(syncWarningNeverCode, "This node has no sync status; it may not hold the primary's latest configuration.")
	}
	state, bound, err := s.topologySync.LoadState(ctx)
	if err != nil || !bound || state.AppliedHash == "" {
		return warn(syncWarningNeverCode, "This node has never synchronized with a primary; promoting it publishes only the configuration it holds locally.")
	}
	if state.Outdated() {
		return warn(syncWarningOutdatedCode, "This node is behind its primary; promoting it publishes an older configuration and drops the primary's newer changes.")
	}
	if state.ConsecutiveFailures > 0 {
		return warn(syncWarningUnverifiedCode, "This node could not reach its primary at the last attempt, so it may be outdated; recent primary changes could be lost.")
	}
	return nil
}
