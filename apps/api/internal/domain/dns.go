package domain

import (
	"fmt"
	"net"
	"regexp"
	"strings"
)

const (
	MinTTL     = 30
	MaxTTL     = 86_400
	DefaultTTL = 300
)

var (
	recordTypeSet   = map[string]bool{"A": true, "AAAA": true, "CNAME": true, "TXT": true, "MX": true, "SRV": true}
	dnsLabelPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
	crlfPattern     = regexp.MustCompile(`[\r\n]`)
)

// ipVersion mirrors node:net isIP: returns 4, 6, or 0.
func ipVersion(value string) int {
	ip := net.ParseIP(value)
	if ip == nil {
		return 0
	}
	if strings.Contains(value, ":") {
		return 6
	}
	return 4
}

// NormalizeDNSName validates and lowercases a DNS name.
func NormalizeDNSName(value string, allowWildcard bool) (result string, err error) {
	defer recoverValidation(&err)
	result = normalizeDNSName(value, allowWildcard)
	return
}

func normalizeDNSName(value string, allowWildcard bool) string {
	withoutRoot := strings.ToLower(strings.TrimRight(strings.TrimSpace(value), "."))
	assertDomain(len(withoutRoot) > 0, "DNS name cannot be empty", "DNS_NAME_EMPTY")
	labels := strings.Split(withoutRoot, ".")
	assertDomain(len(labels) <= 127, "DNS name has too many labels", "DNS_NAME_LABELS")
	for index, label := range labels {
		if allowWildcard && index == 0 && label == "*" {
			continue
		}
		assertDomain(len(label) <= 63 && dnsLabelPattern.MatchString(label), "Invalid DNS label: "+label, "DNS_LABEL_INVALID")
	}
	assertDomain(len(withoutRoot) <= 253, "DNS name is too long", "DNS_NAME_LENGTH")
	return withoutRoot
}

func canonicalRecordName(name, zoneName string) string {
	zone := normalizeDNSName(zoneName, false)
	value := strings.TrimSpace(name)
	if value == "" || value == "@" {
		return zone
	}
	normalized := normalizeDNSName(value, true)
	if normalized == zone || strings.HasSuffix(normalized, "."+zone) {
		return normalized
	}
	return normalized + "." + zone
}

// RecordSetOptions carries validation context for a record set.
type RecordSetOptions struct {
	ZoneName     string
	Ingress      *Ingress
	Certificates []CertificateStatus
}

// ValidateRecordSet validates and canonicalizes a set of records for a zone.
func ValidateRecordSet(records []DNSRecord, options RecordSetOptions) (result []DNSRecord, err error) {
	defer recoverValidation(&err)
	result = validateRecordSet(records, options)
	return
}

func validateRecordSet(records []DNSRecord, options RecordSetOptions) []DNSRecord {
	zoneName := normalizeDNSName(options.ZoneName, false)
	seenIDs := map[string]bool{}
	normalized := make([]DNSRecord, 0, len(records))
	for _, record := range records {
		assertDomain(len(strings.TrimSpace(record.ID)) > 0, "Record id is required", "RECORD_ID_REQUIRED")
		assertDomain(!seenIDs[record.ID], "Duplicate record id: "+record.ID, "RECORD_ID_DUPLICATE")
		seenIDs[record.ID] = true
		assertDomain(recordTypeSet[record.Type], "Unsupported record type: "+record.Type, "RECORD_TYPE")

		name := canonicalRecordName(record.Name, zoneName)
		ttl := DefaultTTL
		if record.TTL != 0 {
			ttl = record.TTL
		}
		assertDomain(ttl >= MinTTL && ttl <= MaxTTL, fmt.Sprintf("TTL must be between %d and %d", MinTTL, MaxTTL), "TTL_INVALID")
		validateRecordValue(record.Type, record.Value, name)

		if record.Proxied {
			assertDomain(record.Type == "A" || record.Type == "AAAA" || record.Type == "CNAME", record.Type+" records cannot be proxied", "PROXY_RECORD_TYPE")
			assertDomain(record.Proxy != nil, "Proxied records require proxy settings", "PROXY_SETTINGS_REQUIRED")
			validateIngressForRecord(record.Type, options.Ingress)
			proxy := validateProxySettings(record.Proxy, nil)
			if options.Certificates != nil {
				validateProxyCertificate(name, proxyBool(proxy, "tlsEnabled"), proxyString(proxy, "certificateId"), options.Certificates)
			}
		} else if record.Proxy != nil {
			validateProxySettings(record.Proxy, nil)
		}

		next := record
		next.Name = name
		next.TTL = ttl
		normalized = append(normalized, next)
	}

	enabledByName := map[string][]DNSRecord{}
	for _, record := range normalized {
		if record.Enabled {
			enabledByName[record.Name] = append(enabledByName[record.Name], record)
		}
	}
	for name, group := range enabledByName {
		hasCNAME := false
		proxied := 0
		for _, record := range group {
			if record.Type == "CNAME" {
				hasCNAME = true
			}
			if record.Proxied {
				proxied++
			}
		}
		if hasCNAME && len(group) > 1 {
			panic(fmt.Errorf("CNAME cannot coexist with other records for %s", name))
		}
		assertDomain(proxied <= 1, "Only one record may be proxied for "+name, "PROXY_HOST_CONFLICT")
	}
	return normalized
}

func validateRecordValue(recordType string, value any, name string) {
	switch recordType {
	case "A":
		str, ok := value.(string)
		assertDomain(ok && ipVersion(str) == 4, "Invalid A value for "+name, "A_VALUE")
	case "AAAA":
		str, ok := value.(string)
		assertDomain(ok && ipVersion(str) == 6, "Invalid AAAA value for "+name, "AAAA_VALUE")
	case "CNAME":
		str, ok := value.(string)
		assertDomain(ok, "Invalid CNAME value for "+name, "CNAME_VALUE")
		normalizeDNSName(str, true)
	case "TXT":
		str, ok := value.(string)
		assertDomain(ok && len(str) > 0 && len(str) <= 255 && !crlfPattern.MatchString(str), "Invalid TXT value for "+name, "TXT_VALUE")
	case "MX":
		assertMxValue(value, name)
	case "SRV":
		assertSrvValue(value, name)
	}
}

func assertMxValue(value any, name string) {
	object, ok := value.(map[string]any)
	priority, priorityOK := toInt(objectGet(object, "priority"))
	exchange, exchangeOK := objectGet(object, "exchange").(string)
	assertDomain(ok && priorityOK && priority >= 0 && priority <= 65_535 && exchangeOK, "Invalid MX value for "+name, "MX_VALUE")
	normalizeDNSName(exchange, true)
}

func assertSrvValue(value any, name string) {
	object, ok := value.(map[string]any)
	priority, pOK := toInt(objectGet(object, "priority"))
	weight, wOK := toInt(objectGet(object, "weight"))
	port, portOK := toInt(objectGet(object, "port"))
	target, targetOK := objectGet(object, "target").(string)
	assertDomain(
		ok && pOK && priority >= 0 && priority <= 65_535 &&
			wOK && weight >= 0 && weight <= 65_535 &&
			portOK && port >= 0 && port <= 65_535 && targetOK,
		"Invalid SRV value for "+name, "SRV_VALUE",
	)
	normalizeDNSName(target, true)
}

func validateIngressForRecord(recordType string, ingress *Ingress) {
	assertDomain(ingress != nil, "Proxy ingress addresses are required", "INGRESS_REQUIRED")
	switch recordType {
	case "A":
		assertDomain(ipVersion(ingress.IPv4) == 4, "IPv4 proxy ingress is required", "INGRESS_IPV4_REQUIRED")
	case "AAAA":
		assertDomain(ipVersion(ingress.IPv6) == 6, "IPv6 proxy ingress is required", "INGRESS_IPV6_REQUIRED")
	default:
		assertDomain(ipVersion(ingress.IPv4) == 4 || ipVersion(ingress.IPv6) == 6, "At least one proxy ingress address is required", "INGRESS_REQUIRED")
	}
}

func validateProxyCertificate(hostname string, tlsEnabled bool, certificateID string, certificates []CertificateStatus) {
	if !tlsEnabled {
		return
	}
	assertDomain(certificateID != "", "Proxied TLS record "+hostname+" requires a certificate", "CERTIFICATE_REQUIRED")
	if certificates == nil {
		return
	}
	var certificate *CertificateStatus
	for i := range certificates {
		if certificates[i].ID == certificateID {
			certificate = &certificates[i]
			break
		}
	}
	assertDomain(certificate != nil, "Certificate not found: "+certificateID, "CERTIFICATE_NOT_FOUND")
	assertDomain(certificate.Status == "active" || certificate.Status == "issued", "Certificate is not active: "+certificateID, "CERTIFICATE_NOT_ACTIVE")
	normalizedHostname := strings.TrimRight(strings.ToLower(hostname), ".")
	covered := false
	for _, candidate := range certificate.Hostnames {
		normalizedCandidate := strings.TrimRight(strings.ToLower(candidate), ".")
		if normalizedCandidate == normalizedHostname {
			covered = true
			break
		}
		if strings.HasPrefix(normalizedCandidate, "*.") &&
			strings.HasSuffix(normalizedHostname, normalizedCandidate[1:]) &&
			len(strings.Split(normalizedHostname, ".")) == len(strings.Split(normalizedCandidate, ".")) {
			covered = true
			break
		}
	}
	assertDomain(covered, "Certificate does not cover "+hostname, "CERTIFICATE_HOSTNAME")
}

// ValidateResolverPool validates a resolver pool.
func ValidateResolverPool(pool ResolverPool) (result ResolverPool, err error) {
	defer recoverValidation(&err)
	result = validateResolverPool(pool)
	return
}

func validateResolverPool(pool ResolverPool) ResolverPool {
	assertDomain(len(strings.TrimSpace(pool.ID)) > 0, "Resolver pool id is required", "POOL_ID")
	assertDomain(len(pool.Endpoints) > 0, "Resolver pool needs an endpoint", "POOL_EMPTY")
	endpoints := make([]ResolverEndpoint, 0, len(pool.Endpoints))
	for _, endpoint := range pool.Endpoints {
		endpoints = append(endpoints, validateResolverEndpoint(endpoint))
	}
	return ResolverPool{ID: pool.ID, Endpoints: endpoints}
}

func validateResolverEndpoint(endpoint ResolverEndpoint) ResolverEndpoint {
	assertDomain(ipVersion(endpoint.Host) > 0, "Resolver endpoint must be a literal IP", "RESOLVER_IP")
	assertDomain(endpoint.Port >= 1 && endpoint.Port <= 65_535, "Resolver endpoint port is invalid", "RESOLVER_PORT")
	return endpoint
}

// ValidateForwardingRules validates forwarding rules.
func ValidateForwardingRules(rules []ForwardingRule) (result []ForwardingRule, err error) {
	defer recoverValidation(&err)
	result = validateForwardingRules(rules)
	return
}

func validateForwardingRules(rules []ForwardingRule) []ForwardingRule {
	seen := map[string]bool{}
	out := make([]ForwardingRule, 0, len(rules))
	for _, rule := range rules {
		suffix := normalizeDNSName(rule.Suffix, false)
		assertDomain(!seen[suffix], "Duplicate forwarding suffix: "+suffix, "FORWARD_SUFFIX_DUPLICATE")
		seen[suffix] = true
		out = append(out, ForwardingRule{Suffix: suffix, Pool: validateResolverPool(rule.Pool)})
	}
	return out
}

func objectGet(object map[string]any, key string) any {
	if object == nil {
		return nil
	}
	return object[key]
}

func toInt(value any) (int, bool) {
	switch v := value.(type) {
	case float64:
		if v == float64(int(v)) {
			return int(v), true
		}
		return 0, false
	case int:
		return v, true
	case int64:
		return int(v), true
	}
	return 0, false
}
