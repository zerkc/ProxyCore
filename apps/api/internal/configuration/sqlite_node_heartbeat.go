package configuration

import (
	"context"
	"database/sql"
	"errors"
	"net/netip"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

// ErrNodeHeartbeatStore reports a heartbeat or listing store failure. Callers
// must treat it as best-effort for the heartbeat and as unavailable for lists.
var ErrNodeHeartbeatStore = errors.New("node heartbeat store unavailable")

// Derived status of an enrolled node on the primary.
const (
	EnrolledNodeLinked   = "linked"
	EnrolledNodeUnlinked = "unlinked"
	EnrolledNodeRevoked  = "revoked"
)

// NodeHeartbeat is one authenticated contact from an enrolled node. The
// ingress fields are optional node-reported gateway addresses; invalid or
// empty values leave the stored address untouched.
type NodeHeartbeat struct {
	NodeID      string
	At          time.Time
	IngressIPv4 string
	IngressIPv6 string
}

// EnrolledNodeListing is the non-secret listing view of one enrolled node.
type EnrolledNodeListing struct {
	NodeID      string
	DisplayName string
	Status      string
	IngressIPv4 string
	IngressIPv6 string
	LastSeenAt  *time.Time
	EnrolledAt  time.Time
	UnlinkedAt  *time.Time
}

// CanonicalIngressIPv4 returns the canonical text of a plain IPv4 address.
func CanonicalIngressIPv4(value string) (string, bool) {
	addr, err := netip.ParseAddr(value)
	if err != nil || !addr.Is4() {
		return "", false
	}
	return addr.String(), true
}

// CanonicalIngressIPv6 returns the canonical text of a plain IPv6 address
// (no zone, not an IPv4-mapped address).
func CanonicalIngressIPv6(value string) (string, bool) {
	addr, err := netip.ParseAddr(value)
	if err != nil || !addr.Is6() || addr.Is4In6() || addr.Zone() != "" {
		return "", false
	}
	return addr.String(), true
}

// RecordNodeHeartbeat stamps last_seen_at (and the reported gateway) for a
// node that is neither revoked nor unlinked, in one small write.
func (s *SQLitePhase2Store) RecordNodeHeartbeat(ctx context.Context, beat NodeHeartbeat) error {
	if ctx == nil || s == nil || s.writer == nil {
		return ErrNodeHeartbeatStore
	}
	canonical, err := sqlitedb.ParseUUIDLookupText(beat.NodeID)
	if err != nil {
		return ErrNodeHeartbeatStore
	}
	stamp, err := sqliteEnrollmentTokenTime(beat.At)
	if err != nil {
		return ErrNodeHeartbeatStore
	}
	var v4, v6 any
	if value, ok := CanonicalIngressIPv4(beat.IngressIPv4); ok {
		v4 = value
	}
	if value, ok := CanonicalIngressIPv6(beat.IngressIPv6); ok {
		v6 = value
	}
	err = s.writer.Write(ctx, func(v sqlitedb.WriteValue) error {
		_, err := v.Conn().ExecContext(v.Context(), `update enrolled_nodes
			set last_seen_at = ?, ingress_ipv4 = coalesce(?, ingress_ipv4), ingress_ipv6 = coalesce(?, ingress_ipv6)
			where node_id = ? and revoked_at is null and unlinked_at is null`, stamp, v4, v6, canonical)
		return err
	})
	if err != nil {
		return ErrNodeHeartbeatStore
	}
	return nil
}

// ListEnrolledNodes returns every enrolled node, newest enrollment first,
// including revoked and unlinked ones. It never reads credential material.
func (s *SQLitePhase2Store) ListEnrolledNodes(ctx context.Context) ([]EnrolledNodeListing, error) {
	if ctx == nil || s == nil || s.db == nil {
		return nil, ErrNodeHeartbeatStore
	}
	rows, err := s.db.QueryContext(ctx, `select node_id, display_name, enrolled_at, revoked_at, unlinked_at,
		last_seen_at, ingress_ipv4, ingress_ipv6 from enrolled_nodes order by enrolled_at desc, node_id`)
	if err != nil {
		return nil, ErrNodeHeartbeatStore
	}
	defer rows.Close()
	nodes := []EnrolledNodeListing{}
	for rows.Next() {
		var (
			id                            string
			name, revoked, unlinked, seen sql.NullString
			enrolled                      string
			v4, v6                        sql.NullString
		)
		if err := rows.Scan(&id, &name, &enrolled, &revoked, &unlinked, &seen, &v4, &v6); err != nil {
			return nil, ErrNodeHeartbeatStore
		}
		// An undecodable or non-finite clock must not brick the whole list.
		var enrolledAt time.Time
		if decoded, err := sqliteEnqueueTime(&enrolled); err == nil && decoded != nil {
			enrolledAt = *decoded
		}
		record := EnrolledNodeListing{
			NodeID: id, DisplayName: name.String, EnrolledAt: enrolledAt,
			IngressIPv4: v4.String, IngressIPv6: v6.String, Status: EnrolledNodeLinked,
		}
		record.UnlinkedAt = nullableNodeTime(unlinked)
		record.LastSeenAt = nullableNodeTime(seen)
		switch {
		case record.UnlinkedAt != nil:
			record.Status = EnrolledNodeUnlinked
		case revoked.Valid:
			record.Status = EnrolledNodeRevoked
		}
		nodes = append(nodes, record)
	}
	if rows.Err() != nil {
		return nil, ErrNodeHeartbeatStore
	}
	return nodes, nil
}

func nullableNodeTime(value sql.NullString) *time.Time {
	if !value.Valid {
		return nil
	}
	text := value.String
	at, err := sqliteEnqueueTime(&text)
	if err != nil {
		return nil
	}
	return at
}
