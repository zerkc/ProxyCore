package domain

// MvpRecordTypes are the DNS record types ProxyCore supports.
var MvpRecordTypes = []string{"A", "AAAA", "CNAME", "TXT", "MX", "SRV"}

// Ingress addresses the proxy answers on.
type Ingress struct {
	IPv4 string `json:"ipv4,omitempty"`
	IPv6 string `json:"ipv6,omitempty"`
}

// ResolverEndpoint is a literal upstream resolver.
type ResolverEndpoint struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

// ResolverPool groups resolver endpoints.
type ResolverPool struct {
	ID        string             `json:"id"`
	Endpoints []ResolverEndpoint `json:"endpoints"`
}

// ForwardingRule maps a DNS suffix to a resolver pool.
type ForwardingRule struct {
	Suffix string       `json:"suffix"`
	Pool   ResolverPool `json:"pool"`
}

// Settings is the installation-wide configuration.
type Settings struct {
	Ingress             Ingress          `json:"ingress"`
	DefaultPool         *ResolverPool    `json:"defaultPool,omitempty"`
	ForwardingRules     []ForwardingRule `json:"forwardingRules"`
	RetentionMaxAgeDays int              `json:"retentionMaxAgeDays"`
	RetentionMaxSizeMb  int              `json:"retentionMaxSizeMb"`
}

// DNSRecordInput is the mutable shape used when creating or updating a record.
type DNSRecordInput struct {
	ID       string         `json:"id"`
	Name     string         `json:"name"`
	Type     string         `json:"type"`
	Value    any            `json:"value"`
	TTL      *int           `json:"ttl,omitempty"`
	Enabled  bool           `json:"enabled"`
	Comment  *string        `json:"comment,omitempty"`
	Proxied  bool           `json:"proxied"`
	Proxy    map[string]any `json:"proxy,omitempty"`
}

// DNSRecord is a validated, canonical record.
type DNSRecord struct {
	ID      string         `json:"id"`
	Name    string         `json:"name"`
	Type    string         `json:"type"`
	Value   any            `json:"value"`
	TTL     int            `json:"ttl"`
	Enabled bool           `json:"enabled"`
	Comment *string        `json:"comment,omitempty"`
	Proxied bool           `json:"proxied"`
	Proxy   map[string]any `json:"proxy,omitempty"`
}

// StreamRoute forwards a listener to an upstream.
type StreamRoute struct {
	ID            string         `json:"id"`
	Enabled       bool           `json:"enabled"`
	Protocol      string         `json:"protocol"`
	ListenAddress string         `json:"listenAddress"`
	ListenPort    int            `json:"listenPort"`
	Upstream      UpstreamTarget `json:"upstream"`
}

// UpstreamTarget is an IP:port with a protocol.
type UpstreamTarget struct {
	IP       string `json:"ip"`
	Port     int    `json:"port"`
	Protocol string `json:"protocol"`
}

// ZoneState is a zone and its records.
type ZoneState struct {
	ID      string      `json:"id"`
	Name    string      `json:"name"`
	Enabled bool        `json:"enabled"`
	Records []DNSRecord `json:"records"`
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
