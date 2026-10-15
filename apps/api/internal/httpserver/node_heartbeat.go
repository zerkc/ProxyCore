package httpserver

import (
	"context"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
	syncpublication "github.com/zerkc/ProxyCore/apps/api/internal/sync"
)

// DefaultNodeHeartbeatInterval bounds how often one node causes a heartbeat
// write when its reported gateway did not change.
const DefaultNodeHeartbeatInterval = 15 * time.Second

// NodeHeartbeatSink persists one authenticated contact from an enrolled node.
// *configuration.SQLitePhase2Store satisfies it.
type NodeHeartbeatSink interface {
	RecordNodeHeartbeat(ctx context.Context, beat configuration.NodeHeartbeat) error
}

// recordNodeHeartbeat reports an authenticated node contact. It is strictly
// best-effort: a missing sink, a malformed header or a store failure never
// affects the node's request, and nothing secret is logged.
func recordNodeHeartbeat(ctx context.Context, sink NodeHeartbeatSink, nodeID string, r *http.Request) {
	if sink == nil || nodeID == "" || r == nil {
		return
	}
	beat := configuration.NodeHeartbeat{
		NodeID: nodeID,
		At:     time.Now().UTC(),
	}
	// Invalid values are dropped so a bad header can neither be stored nor
	// defeat the throttle.
	if v4, ok := configuration.CanonicalIngressIPv4(boundedHeader(r, syncpublication.IngressIPv4Header, 15)); ok {
		beat.IngressIPv4 = v4
	}
	if v6, ok := configuration.CanonicalIngressIPv6(boundedHeader(r, syncpublication.IngressIPv6Header, 45)); ok {
		beat.IngressIPv6 = v6
	}
	if err := sink.RecordNodeHeartbeat(ctx, beat); err != nil {
		log.Printf("node heartbeat not recorded: %v", err)
	}
}

func boundedHeader(r *http.Request, name string, max int) string {
	value := r.Header.Get(name)
	if len(value) > max {
		return ""
	}
	return value
}

type throttledNodeHeartbeat struct {
	inner    NodeHeartbeatSink
	interval time.Duration
	now      func() time.Time

	mu   sync.Mutex
	last map[string]throttledBeat
}

type throttledBeat struct {
	at   time.Time
	ipv4 string
	ipv6 string
}

// NewThrottledNodeHeartbeat skips a write when the same node already beat
// within interval and reported the same gateway. A zero interval uses
// DefaultNodeHeartbeatInterval. The throttle is per process; a restart only
// causes one extra write per node.
func NewThrottledNodeHeartbeat(inner NodeHeartbeatSink, interval time.Duration, now func() time.Time) NodeHeartbeatSink {
	if interval <= 0 {
		interval = DefaultNodeHeartbeatInterval
	}
	if now == nil {
		now = time.Now
	}
	return &throttledNodeHeartbeat{inner: inner, interval: interval, now: now, last: map[string]throttledBeat{}}
}

func (t *throttledNodeHeartbeat) RecordNodeHeartbeat(ctx context.Context, beat configuration.NodeHeartbeat) error {
	now := t.now()
	t.mu.Lock()
	previous, seen := t.last[beat.NodeID]
	changed := (beat.IngressIPv4 != "" && beat.IngressIPv4 != previous.ipv4) ||
		(beat.IngressIPv6 != "" && beat.IngressIPv6 != previous.ipv6)
	if seen && !changed && now.Sub(previous.at) < t.interval {
		t.mu.Unlock()
		return nil
	}
	next := throttledBeat{at: now, ipv4: previous.ipv4, ipv6: previous.ipv6}
	if beat.IngressIPv4 != "" {
		next.ipv4 = beat.IngressIPv4
	}
	if beat.IngressIPv6 != "" {
		next.ipv6 = beat.IngressIPv6
	}
	t.last[beat.NodeID] = next
	t.mu.Unlock()
	if err := t.inner.RecordNodeHeartbeat(ctx, beat); err != nil {
		// Let the next contact retry immediately.
		t.mu.Lock()
		delete(t.last, beat.NodeID)
		t.mu.Unlock()
		return err
	}
	return nil
}
