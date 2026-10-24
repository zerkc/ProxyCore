package renderers

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
)

// CoreDNSZoneInput is one managed zone and its records.
type CoreDNSZoneInput struct {
	Name    string             `json:"name"`
	Records []domain.DNSRecord `json:"records"`
}

// CoreDNSRenderInput is everything needed to render one CoreDNS candidate. The
// ingress is the applying node's own address, so each node renders its own tree.
type CoreDNSRenderInput struct {
	Zones           []CoreDNSZoneInput      `json:"zones"`
	Ingress         domain.Ingress          `json:"ingress"`
	DefaultPool     domain.ResolverPool     `json:"defaultPool"`
	ForwardingRules []domain.ForwardingRule `json:"forwardingRules"`
}

// CoreDNSCandidate is the rendered Corefile, zone files and checksum.
type CoreDNSCandidate struct {
	Corefile string            `json:"corefile"`
	Files    map[string]string `json:"files"`
	Checksum string            `json:"checksum"`
}

// RenderCoreDNSCandidate renders the Corefile and one zone file per managed zone.
func RenderCoreDNSCandidate(input CoreDNSRenderInput) (CoreDNSCandidate, error) {
	defaultPool, err := domain.ValidateResolverPool(input.DefaultPool)
	if err != nil {
		return CoreDNSCandidate{}, err
	}
	rules, err := domain.ValidateForwardingRules(input.ForwardingRules)
	if err != nil {
		return CoreDNSCandidate{}, err
	}
	files := map[string]string{}
	zoneBlocks := []string{}

	zones := append([]CoreDNSZoneInput(nil), input.Zones...)
	sort.SliceStable(zones, func(i, j int) bool {
		return localeCompare(zones[i].Name, zones[j].Name) < 0
	})
	for _, zone := range zones {
		normalizedZone, err := domain.NormalizeDNSName(zone.Name, false)
		if err != nil {
			return CoreDNSCandidate{}, err
		}
		ingress := input.Ingress
		records, err := domain.ValidateRecordSet(zone.Records, domain.RecordSetOptions{
			ZoneName: normalizedZone,
			Ingress:  &ingress,
		})
		if err != nil {
			return CoreDNSCandidate{}, err
		}
		relativePath := "zones/" + normalizedZone + ".zone"
		body, err := renderZoneFile(normalizedZone, records, input.Ingress)
		if err != nil {
			return CoreDNSCandidate{}, err
		}
		files[relativePath] = body
		zoneBlocks = append(zoneBlocks, normalizedZone+":53 {\n    file /etc/coredns/"+relativePath+"\n}")
	}

	sort.SliceStable(rules, func(i, j int) bool {
		return len(rules[i].Suffix) > len(rules[j].Suffix)
	})
	blocks := append([]string(nil), zoneBlocks...)
	for _, rule := range rules {
		blocks = append(blocks, rule.Suffix+":53 {\n"+renderForwardBlock(rule.Pool)+"\n}")
	}
	blocks = append(blocks, ".:53 {\n    errors\n    health\n    ready\n    loop\n    reload\n    cache 30\n"+renderForwardBlock(defaultPool)+"\n}")
	corefile := strings.Join(blocks, "\n\n") + "\n"

	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	parts := make([]string, 0, len(paths))
	for _, path := range paths {
		parts = append(parts, path+"\n"+files[path])
	}
	hash := sha256.New()
	hash.Write([]byte(corefile))
	hash.Write([]byte(strings.Join(parts, "\n")))
	return CoreDNSCandidate{Corefile: corefile, Files: files, Checksum: hex.EncodeToString(hash.Sum(nil))}, nil
}

func renderZoneFile(zone string, records []domain.DNSRecord, ingress domain.Ingress) (string, error) {
	body := []string{
		"$ORIGIN " + zone + ".",
		"$TTL 300",
		"@ 300 IN SOA ns1." + zone + ". hostmaster." + zone + ". 1 60 300 3600 60",
		"@ 300 IN NS ns1." + zone + ".",
	}

	enabled := make([]domain.DNSRecord, 0, len(records))
	sortKeys := map[string]string{}
	for _, record := range records {
		if !record.Enabled {
			continue
		}
		enabled = append(enabled, record)
		key, err := jsonStringify(record.Value)
		if err != nil {
			return "", err
		}
		sortKeys[record.ID] = key
	}
	sort.SliceStable(enabled, func(i, j int) bool {
		left, right := enabled[i], enabled[j]
		if byName := localeCompare(left.Name, right.Name); byName != 0 {
			return byName < 0
		}
		if byType := localeCompare(left.Type, right.Type); byType != 0 {
			return byType < 0
		}
		return localeCompare(sortKeys[left.ID], sortKeys[right.ID]) < 0
	})

	for _, record := range enabled {
		owner := ownerName(record.Name, zone)
		if record.Proxied {
			body = append(body, proxiedAnswers(owner, record, ingress)...)
			continue
		}
		body = append(body, fmt.Sprintf("%s %d IN %s %s", owner, record.TTL, record.Type, renderValue(record)))
	}
	return strings.Join(body, "\n") + "\n", nil
}

func proxiedAnswers(owner string, record domain.DNSRecord, ingress domain.Ingress) []string {
	lines := []string{}
	if (record.Type == "A" || record.Type == "CNAME") && ingress.IPv4 != "" {
		lines = append(lines, fmt.Sprintf("%s %d IN A %s", owner, record.TTL, ingress.IPv4))
	}
	if (record.Type == "AAAA" || record.Type == "CNAME") && ingress.IPv6 != "" {
		lines = append(lines, fmt.Sprintf("%s %d IN AAAA %s", owner, record.TTL, ingress.IPv6))
	}
	return lines
}

func renderValue(record domain.DNSRecord) string {
	switch record.Type {
	case "TXT":
		escaped := strings.ReplaceAll(stringValue(record.Value), `\`, `\\`)
		return `"` + strings.ReplaceAll(escaped, `"`, `\"`) + `"`
	case "MX":
		value := objectValue(record.Value)
		return numberValue(value["priority"]) + " " + ensureFQDN(stringValue(value["exchange"]))
	case "SRV":
		value := objectValue(record.Value)
		return numberValue(value["priority"]) + " " + numberValue(value["weight"]) + " " +
			numberValue(value["port"]) + " " + ensureFQDN(stringValue(value["target"]))
	case "CNAME":
		return ensureFQDN(stringValue(record.Value))
	default:
		return stringValue(record.Value)
	}
}

func renderForwardBlock(pool domain.ResolverPool) string {
	endpoints := make([]string, 0, len(pool.Endpoints))
	for _, endpoint := range pool.Endpoints {
		endpoints = append(endpoints, formatIP(endpoint.Host)+":"+strconv.Itoa(endpoint.Port))
	}
	return "    forward . " + strings.Join(endpoints, " ") + " {\n        policy sequential\n    }"
}

func ownerName(name, zone string) string {
	if name == zone {
		return "@"
	}
	if strings.HasSuffix(name, "."+zone) {
		return name[:len(name)-(len(zone)+1)]
	}
	return name
}

func ensureFQDN(value string) string {
	if strings.HasSuffix(value, ".") {
		return value
	}
	return value + "."
}

func formatIP(ip string) string {
	if strings.Contains(ip, ":") {
		return "[" + ip + "]"
	}
	return ip
}

func objectValue(value any) map[string]any {
	object, _ := value.(map[string]any)
	return object
}

// stringValue mirrors JavaScript String(value) for the scalar values a record
// can hold.
func stringValue(value any) string {
	switch v := value.(type) {
	case nil:
		return "null"
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	default:
		return fmt.Sprint(v)
	}
}

func numberValue(value any) string { return stringValue(value) }

// jsonStringify mirrors JSON.stringify for record values: compact output and no
// HTML escaping. Object key order is sorted, which can differ from JavaScript's
// insertion order; it only decides ties between records of the same name and type.
func jsonStringify(value any) (string, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return "", fmt.Errorf("encode record value: %w", err)
	}
	return strings.TrimSuffix(buffer.String(), "\n"), nil
}
