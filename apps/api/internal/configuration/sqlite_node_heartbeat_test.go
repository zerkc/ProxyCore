package configuration_test

import (
	"context"
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
)

func TestSQLiteRecordNodeHeartbeatStoresSeenAndGateway(t *testing.T) {
	f := newGrantFixture(t)
	node := f.enrollOne()
	at := f.now.Add(time.Minute)
	err := f.store.RecordNodeHeartbeat(context.Background(), configuration.NodeHeartbeat{
		NodeID: node, At: at, IngressIPv4: "203.0.113.7", IngressIPv6: "2001:DB8::1",
	})
	if err != nil {
		t.Fatal(err)
	}
	nodes, err := f.store.ListEnrolledNodes(context.Background())
	if err != nil || len(nodes) != 1 {
		t.Fatalf("list = %v, %v", nodes, err)
	}
	got := nodes[0]
	if got.Status != configuration.EnrolledNodeLinked || got.LastSeenAt == nil || !got.LastSeenAt.Equal(at) ||
		got.IngressIPv4 != "203.0.113.7" || got.IngressIPv6 != "2001:db8::1" {
		t.Fatalf("node = %+v", got)
	}

	// A later beat without ingress keeps the stored gateway; invalid values are ignored.
	later := at.Add(time.Minute)
	if err := f.store.RecordNodeHeartbeat(context.Background(), configuration.NodeHeartbeat{
		NodeID: node, At: later, IngressIPv4: "not-an-ip", IngressIPv6: "10.0.0.1",
	}); err != nil {
		t.Fatal(err)
	}
	nodes, _ = f.store.ListEnrolledNodes(context.Background())
	got = nodes[0]
	if !got.LastSeenAt.Equal(later) || got.IngressIPv4 != "203.0.113.7" || got.IngressIPv6 != "2001:db8::1" {
		t.Fatalf("after invalid beat = %+v", got)
	}
}

func TestSQLiteRecordNodeHeartbeatSkipsUnlinkedNode(t *testing.T) {
	f := newGrantFixture(t)
	node := f.enrollOne()
	if _, err := f.store.UnlinkEnrolledNode(context.Background(), node, f.now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := f.store.RecordNodeHeartbeat(context.Background(), configuration.NodeHeartbeat{NodeID: node, At: f.now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	nodes, err := f.store.ListEnrolledNodes(context.Background())
	if err != nil || len(nodes) != 1 {
		t.Fatalf("list = %v, %v", nodes, err)
	}
	if nodes[0].Status != configuration.EnrolledNodeUnlinked || nodes[0].LastSeenAt != nil || nodes[0].UnlinkedAt == nil {
		t.Fatalf("unlinked node must stay listed and untouched: %+v", nodes[0])
	}
}
