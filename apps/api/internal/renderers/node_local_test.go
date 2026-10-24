package renderers

import (
	"reflect"
	"strings"
	"testing"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
)

// Node-local CoreDNS rendering: a node's CoreDNS answers proxied records with
// its OWN ingress address, while DNS-only answers stay identical across nodes.
// The per-node ingress is a render-time parameter; the renderer is unchanged.

func minimalProxy() map[string]any {
	return map[string]any{
		"origin":     map[string]any{"ip": "192.0.1.20", "port": float64(8080), "protocol": "http"},
		"tlsEnabled": false,
	}
}

func nodeLocalZone() CoreDNSZoneInput {
	return CoreDNSZoneInput{Name: "home.arpa", Records: []domain.DNSRecord{
		{ID: "rec-dns-only", Name: "dns-only", Type: "A", Value: "192.0.1.10", TTL: 300, Enabled: true},
		{ID: "rec-proxied", Name: "app", Type: "A", Value: "192.0.1.20", TTL: 60, Enabled: true, Proxied: true, Proxy: minimalProxy()},
		{ID: "rec-proxied-cname", Name: "alias", Type: "CNAME", Value: "origin.home.arpa", TTL: 60, Enabled: true, Proxied: true, Proxy: minimalProxy()},
	}}
}

func renderNodeLocal(t *testing.T, zone CoreDNSZoneInput, ingress domain.Ingress) CoreDNSCandidate {
	t.Helper()
	candidate, err := RenderCoreDNSCandidate(CoreDNSRenderInput{
		Zones:       []CoreDNSZoneInput{zone},
		Ingress:     ingress,
		DefaultPool: domain.ResolverPool{ID: "default", Endpoints: []domain.ResolverEndpoint{{Host: "1.1.1.1", Port: 53}}},
	})
	if err != nil {
		t.Fatalf("RenderCoreDNSCandidate: %v", err)
	}
	return candidate
}

var (
	primaryIngress = domain.Ingress{IPv4: "192.0.2.10"}
	nodeBIngress   = domain.Ingress{IPv4: "192.0.2.20"}
)

func TestNodeLocalDNSOnlyAnswersAreIdenticalAcrossNodes(t *testing.T) {
	a := renderNodeLocal(t, nodeLocalZone(), primaryIngress)
	b := renderNodeLocal(t, nodeLocalZone(), nodeBIngress)

	dnsOnlyLines := func(candidate CoreDNSCandidate) []string {
		lines := []string{}
		for _, line := range strings.Split(candidate.Files["zones/home.arpa.zone"], "\n") {
			if strings.Contains(line, "dns-only") {
				lines = append(lines, line)
			}
		}
		return lines
	}
	if !reflect.DeepEqual(dnsOnlyLines(a), dnsOnlyLines(b)) {
		t.Errorf("DNS-only lines differ: %v vs %v", dnsOnlyLines(a), dnsOnlyLines(b))
	}
	if len(dnsOnlyLines(a)) != 1 {
		t.Errorf("expected exactly one DNS-only line, got %v", dnsOnlyLines(a))
	}
}

func TestNodeLocalProxiedAnswersUseTheApplyingNodesIngress(t *testing.T) {
	a := renderNodeLocal(t, nodeLocalZone(), primaryIngress).Files["zones/home.arpa.zone"]
	b := renderNodeLocal(t, nodeLocalZone(), nodeBIngress).Files["zones/home.arpa.zone"]

	assertContains(t, a, "app 60 IN A 192.0.2.10")
	assertNotContains(t, a, "192.0.2.20")
	assertContains(t, b, "app 60 IN A 192.0.2.20")
	assertNotContains(t, b, "192.0.2.10")
}

func TestNodeLocalProxiedCNAMEsUseTheApplyingNodesIngress(t *testing.T) {
	a := renderNodeLocal(t, nodeLocalZone(), primaryIngress).Files["zones/home.arpa.zone"]
	b := renderNodeLocal(t, nodeLocalZone(), nodeBIngress).Files["zones/home.arpa.zone"]

	assertContains(t, a, "alias 60 IN A 192.0.2.10")
	assertContains(t, b, "alias 60 IN A 192.0.2.20")
}

func TestNodeLocalDualStackAAAAUsesTheNodesIPv6Ingress(t *testing.T) {
	zone := CoreDNSZoneInput{Name: "home.arpa", Records: []domain.DNSRecord{
		{ID: "rec-aaaa", Name: "v6", Type: "AAAA", Value: "2001:db8::20", TTL: 60, Enabled: true, Proxied: true, Proxy: minimalProxy()},
	}}
	candidate := renderNodeLocal(t, zone, domain.Ingress{IPv4: "192.0.2.10", IPv6: "2001:db8::10"})

	assertContains(t, candidate.Files["zones/home.arpa.zone"], "v6 60 IN AAAA 2001:db8::10")
}

func TestNodeLocalChecksumDiffersBetweenNodes(t *testing.T) {
	// The renderer hashes the Corefile plus every file, so the per-node zone
	// content makes the candidate checksum per-node.
	a := renderNodeLocal(t, nodeLocalZone(), primaryIngress)
	b := renderNodeLocal(t, nodeLocalZone(), nodeBIngress)
	if a.Checksum == b.Checksum {
		t.Error("checksums must differ between nodes")
	}
}

func TestNodeLocalChecksumIsStableForTheSameIngressAndZoneSet(t *testing.T) {
	a := renderNodeLocal(t, nodeLocalZone(), primaryIngress)
	again := renderNodeLocal(t, nodeLocalZone(), primaryIngress)
	if a.Checksum != again.Checksum {
		t.Error("checksum must be stable for identical input")
	}
}
