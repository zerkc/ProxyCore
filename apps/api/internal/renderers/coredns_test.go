package renderers

import (
	"strings"
	"testing"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
)

func renderCoreDNSFixture(t *testing.T, name string) CoreDNSCandidate {
	t.Helper()
	var input CoreDNSRenderInput
	loadFixture(t, name, &input)
	candidate, err := RenderCoreDNSCandidate(input)
	if err != nil {
		t.Fatalf("RenderCoreDNSCandidate(%s): %v", name, err)
	}
	return candidate
}

func TestCoreDNSRendersManagedZonesForwardingAndProxiedIngressAnswers(t *testing.T) {
	candidate := renderCoreDNSFixture(t, "coredns_basic")

	zone := candidate.Files["zones/home.arpa.zone"]
	assertContains(t, zone, "api 300 IN A 192.168.1.10")
	assertNotContains(t, zone, "192.168.1.20")
	assertNotContains(t, zone, "disabled")
	assertContains(t, candidate.Corefile, "file /etc/coredns/zones/home.arpa.zone")
	assertContains(t, candidate.Corefile, "forward . 10.0.0.53:53")
	if len(candidate.Checksum) != 64 {
		t.Errorf("checksum length = %d, want 64", len(candidate.Checksum))
	}
	assertGolden(t, "coredns_basic", candidate)
}

func TestCoreDNSRendersProxiedCNAMEAsIngressRecords(t *testing.T) {
	candidate := renderCoreDNSFixture(t, "coredns_cname")

	zone := candidate.Files["zones/home.arpa.zone"]
	assertContains(t, zone, "service 300 IN A 192.168.1.10")
	assertContains(t, zone, "service 300 IN AAAA fd00::10")
	assertNotContains(t, zone, "origin.home.arpa")
	assertGolden(t, "coredns_cname", candidate)
}

func TestCoreDNSGoldenParityWithTypeScriptOutput(t *testing.T) {
	// Covers zone/forwarding ordering, TXT escaping, MX/SRV/CNAME FQDNs,
	// record sort ties, IPv6 resolvers, default TTL and trailing-dot zone names.
	assertGolden(t, "coredns_full", renderCoreDNSFixture(t, "coredns_full"))
}

func TestCoreDNSErrors(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*CoreDNSRenderInput)
		wantErr string
	}{
		{"empty default pool", func(i *CoreDNSRenderInput) { i.DefaultPool.Endpoints = nil }, "Resolver pool needs an endpoint"},
		{"duplicate forwarding suffix", func(i *CoreDNSRenderInput) {
			i.ForwardingRules = []domain.ForwardingRule{
				{Suffix: "corp", Pool: i.DefaultPool},
				{Suffix: "CORP.", Pool: i.DefaultPool},
			}
		}, "Duplicate forwarding suffix: corp"},
		{"invalid zone name", func(i *CoreDNSRenderInput) { i.Zones[0].Name = "bad_zone" }, "Invalid DNS label"},
		{"proxied record without ingress", func(i *CoreDNSRenderInput) { i.Ingress = domain.Ingress{} }, "IPv4 proxy ingress is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var input CoreDNSRenderInput
			loadFixture(t, "coredns_basic", &input)
			tt.mutate(&input)
			_, err := RenderCoreDNSCandidate(input)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestLocaleCompare(t *testing.T) {
	tests := []struct {
		left, right string
		want        int
	}{
		{"a", "b", -1},
		{"b", "a", 1},
		{"a", "a", 0},
		{"A", "b", -1},
		{"a", "A", -1},
		{"host", "host-b", -1},
		{"api.home.arpa", "app.home.arpa", -1},
	}
	for _, tt := range tests {
		if got := localeCompare(tt.left, tt.right); got != tt.want {
			t.Errorf("localeCompare(%q, %q) = %d, want %d", tt.left, tt.right, got, tt.want)
		}
	}
}
