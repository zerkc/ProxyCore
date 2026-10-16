package httpserver

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/auth"
	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
)

// TopologyNodesPath is the prefix of the primary-side node management routes.
// The unlink route is POST TopologyNodesPath+"/{id}/unlink" where id is the
// node UUID. It is intentionally absent from nodeReadOnlyExemptions: a NODE
// must never unlink anything.
const TopologyNodesPath = "/api/topology/nodes"

// NodeUnlinker revokes one enrolled node and marks it explicitly unlinked.
// *configuration.SQLitePhase2Store satisfies it.
type NodeUnlinker interface {
	UnlinkEnrolledNode(ctx context.Context, nodeID string, now time.Time) (time.Time, error)
}

// WithNodeUnlinker wires the primary-side node unlink store.
func WithNodeUnlinker(unlinker NodeUnlinker) Option {
	return func(s *Server) {
		s.nodeUnlinker = unlinker
	}
}

type nodeUnlinkResponse struct {
	NodeID     string    `json:"nodeId"`
	UnlinkedAt time.Time `json:"unlinkedAt"`
}

func (s *Server) handleUnlinkNode(w http.ResponseWriter, r *http.Request) {
	setNoStore(w)
	if _, ok := s.requireUserReadOnly(w, r, auth.RoleOwner); !ok {
		return
	}
	current, loaded := s.currentIdentityReadOnly()
	if !loaded || current.IsStalePrimary() ||
		(current.Role != domain.TopologyRolePrimary && current.Role != domain.TopologyRolePrimaryWithNodes) {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "only a primary can unlink nodes", "code": "TOPOLOGY_NOT_PRIMARY"})
		return
	}
	nodeID := r.PathValue("id")
	if parsed, err := uuid.Parse(nodeID); err != nil || parsed.String() != nodeID {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "node not found"})
		return
	}
	if s.nodeUnlinker == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "node unlink unavailable"})
		return
	}
	unlinkedAt, err := s.nodeUnlinker.UnlinkEnrolledNode(r.Context(), nodeID, time.Now().UTC())
	switch {
	case errors.Is(err, configuration.ErrEnrolledNodeNotFound):
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "node not found"})
	case err != nil:
		noteCause(r.Context(), "node_unlink", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "node unlink unavailable"})
	default:
		writeJSON(w, http.StatusOK, nodeUnlinkResponse{NodeID: nodeID, UnlinkedAt: unlinkedAt.UTC()})
	}
}

// NodeLister lists the enrolled nodes of this primary.
// *configuration.SQLitePhase2Store satisfies it.
type NodeLister interface {
	ListEnrolledNodes(ctx context.Context) ([]configuration.EnrolledNodeListing, error)
}

// WithNodeLister wires the primary-side enrolled node listing.
func WithNodeLister(lister NodeLister) Option {
	return func(s *Server) {
		s.nodeLister = lister
	}
}

type nodeListItem struct {
	NodeID      string     `json:"nodeId"`
	DisplayName string     `json:"displayName"`
	Status      string     `json:"status"`
	IngressIPv4 string     `json:"ingressIPv4"`
	IngressIPv6 string     `json:"ingressIPv6"`
	LastSeenAt  *time.Time `json:"lastSeenAt"`
	EnrolledAt  time.Time  `json:"enrolledAt"`
	UnlinkedAt  *time.Time `json:"unlinkedAt"`
}

type nodeListResponse struct {
	// ServerTime lets the UI judge staleness without trusting the browser clock.
	ServerTime time.Time      `json:"serverTime"`
	Nodes      []nodeListItem `json:"nodes"`
}

func (s *Server) handleListNodes(w http.ResponseWriter, r *http.Request) {
	setNoStore(w)
	if _, ok := s.requireUserReadOnly(w, r, auth.RoleOwner); !ok {
		return
	}
	current, loaded := s.currentIdentityReadOnly()
	if !loaded || current.IsStalePrimary() ||
		(current.Role != domain.TopologyRolePrimary && current.Role != domain.TopologyRolePrimaryWithNodes) {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "only a primary can list nodes", "code": "TOPOLOGY_NOT_PRIMARY"})
		return
	}
	if s.nodeLister == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "node list unavailable"})
		return
	}
	records, err := s.nodeLister.ListEnrolledNodes(r.Context())
	if err != nil {
		noteCause(r.Context(), "node_list", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "node list unavailable"})
		return
	}
	items := make([]nodeListItem, 0, len(records))
	for _, record := range records {
		items = append(items, nodeListItem{
			NodeID: record.NodeID, DisplayName: record.DisplayName, Status: record.Status,
			IngressIPv4: record.IngressIPv4, IngressIPv6: record.IngressIPv6,
			LastSeenAt: record.LastSeenAt, EnrolledAt: record.EnrolledAt.UTC(), UnlinkedAt: record.UnlinkedAt,
		})
	}
	writeJSON(w, http.StatusOK, nodeListResponse{ServerTime: time.Now().UTC(), Nodes: items})
}
