// Package renderers turns validated ProxyCore domain state into the candidate
// configuration trees applied to nginx and CoreDNS. It is a byte-for-byte port
// of the TypeScript @proxycore/renderers package.
package renderers

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
)

const defaultACMEUpstream = "http://127.0.0.1:3000"

// NginxRenderInput is everything needed to render one nginx candidate.
type NginxRenderInput struct {
	Records       []domain.DNSRecord   `json:"records"`
	Streams       []domain.StreamRoute `json:"streams"`
	Capabilities  Http3Capabilities    `json:"capabilities"`
	CandidatePath string               `json:"candidatePath,omitempty"`
	// ACMEUpstream is the upstream used by the ACME HTTP-01 challenge location.
	// Empty means the host-published web port (nginx runs with network_mode: host).
	ACMEUpstream string `json:"acmeUpstream,omitempty"`
	// ExtraFiles are additional candidate files such as Basic Auth htpasswd and
	// TLS cert/key material.
	ExtraFiles map[string]string `json:"extraFiles,omitempty"`
}

// NginxCandidate is the rendered nginx tree and its checksum.
type NginxCandidate struct {
	Config   string            `json:"config"`
	Checksum string            `json:"checksum"`
	Files    map[string]string `json:"files"`
}

var (
	acmeUpstreamPattern  = regexp.MustCompile(`(?i)^https?://[^/\s]+$`)
	directiveNamePattern = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_-]*)\b`)
	commentPattern       = regexp.MustCompile(`#.*`)
	plainValuePattern    = regexp.MustCompile(`^[a-zA-Z0-9_./:$-]+$`)
)

// RenderNginxCandidate renders the nginx.conf candidate for the enabled,
// proxied records and enabled stream routes.
func RenderNginxCandidate(input NginxRenderInput) (NginxCandidate, error) {
	streams, err := domain.ValidateStreamRoutes(input.Streams)
	if err != nil {
		return NginxCandidate{}, err
	}
	records := make([]domain.DNSRecord, 0, len(input.Records))
	for _, record := range input.Records {
		if record.Enabled && record.Proxied {
			records = append(records, record)
		}
	}
	sort.SliceStable(records, func(i, j int) bool {
		return localeCompare(records[i].Name, records[j].Name) < 0
	})
	candidatePath := strings.TrimRight(input.CandidatePath, "/")
	acmeUpstream, err := normalizeACMEUpstream(input.ACMEUpstream)
	if err != nil {
		return NginxCandidate{}, err
	}

	serverBlocks := []string{renderACMEDefaultServer(acmeUpstream)}
	for _, record := range records {
		block, err := renderServer(record, input.Capabilities, candidatePath, acmeUpstream)
		if err != nil {
			return NginxCandidate{}, err
		}
		serverBlocks = append(serverBlocks, block)
	}
	servers := strings.Join(serverBlocks, "\n\n")
	indented := strings.Split(servers, "\n")
	for index, line := range indented {
		if len(line) > 0 {
			indented[index] = "    " + line
		}
	}

	streamConfig := ""
	if len(streams) > 0 {
		streamConfig = renderStreams(streams)
	}
	sections := []string{
		"events {}",
		"",
		"http {",
		"    include /etc/nginx/mime.types;",
		"    default_type application/octet-stream;",
		"    sendfile on;",
		"    map $http_upgrade $connection_upgrade {",
		"        default upgrade;",
		`        "" close;`,
		"    }",
		strings.Join(indented, "\n"),
		"}",
		streamConfig,
	}
	kept := make([]string, 0, len(sections))
	for _, section := range sections {
		if len(section) > 0 {
			kept = append(kept, section)
		}
	}
	// The TypeScript source joins every section with a blank line and then
	// collapses triple newlines once; the output shape depends on both steps.
	joined := strings.ReplaceAll(strings.Join(kept, "\n\n"), "\n\n\n", "\n\n")
	config := strings.TrimRightFunc(joined, unicode.IsSpace) + "\n"

	files := map[string]string{"nginx.conf": config}
	for path, content := range input.ExtraFiles {
		files[path] = content
	}
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	parts := make([]string, 0, len(paths))
	for _, path := range paths {
		parts = append(parts, path+"\n"+files[path])
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	return NginxCandidate{Config: config, Checksum: hex.EncodeToString(sum[:]), Files: files}, nil
}

func renderServer(record domain.DNSRecord, capabilities Http3Capabilities, candidatePath, acmeUpstream string) (string, error) {
	if record.Proxy == nil {
		return "", fmt.Errorf("Proxied record %s has no proxy settings", record.Name)
	}
	settings, err := normalizeProxySettings(record.Proxy, capabilities)
	if err != nil {
		return "", err
	}
	if settings.TLSEnabled && settings.CertificateID == "" {
		return "", fmt.Errorf("TLS proxied record %s requires a certificate", record.Name)
	}

	origin := settings.Origin.Protocol + "://" + formatHost(settings.Origin.IP) + ":" + strconv.Itoa(settings.Origin.Port)
	http2 := ""
	if settings.http2() {
		http2 = " http2"
	}
	lines := []string{"server {"}
	if settings.TLSEnabled {
		lines = append(lines, "    listen 443 ssl"+http2+";", "    listen [::]:443 ssl"+http2+";")
	} else {
		lines = append(lines, "    listen 80;", "    listen [::]:80;")
	}
	if settings.HTTP3 {
		lines = append(lines,
			"    listen 443 quic reuseport;",
			"    listen [::]:443 quic reuseport;",
			`    add_header Alt-Svc 'h3=":443"; ma=86400' always;`,
		)
	}
	lines = append(lines, "    server_name "+record.Name+";")
	lines = append(lines, renderACMEChallengeLocation(acmeUpstream)...)
	if settings.TLSEnabled {
		lines = append(lines,
			"    ssl_certificate "+certificateFilePath(settings.CertificateID, candidatePath, "crt")+";",
			"    ssl_certificate_key "+certificateFilePath(settings.CertificateID, candidatePath, "key")+";",
			"    ssl_protocols TLSv1.2 TLSv1.3;",
		)
	}
	if settings.BasicAuth != nil {
		lines = append(lines,
			`    auth_basic "ProxyCore";`,
			"    auth_basic_user_file "+basicAuthFilePath(settings.BasicAuth.PasswordSecretID, candidatePath)+";",
		)
	}
	lines = append(lines, renderCommonProxyDirectives(settings)...)
	lines = append(lines, renderPathLocations(settings.PathRules, origin, settings)...)
	lines = append(lines, "}")
	server := strings.Join(lines, "\n")

	if settings.TLSEnabled && settings.redirectHTTPToHTTPS() {
		redirect := []string{
			"server {",
			"    listen 80;",
			"    listen [::]:80;",
			"    server_name " + record.Name + ";",
		}
		redirect = append(redirect, renderACMEChallengeLocation(acmeUpstream)...)
		redirect = append(redirect, "    return 308 https://$host$request_uri;", "}", "")
		return strings.Join(redirect, "\n") + "\n" + server, nil
	}
	return server, nil
}

func renderACMEDefaultServer(acmeUpstream string) string {
	lines := []string{
		"server {",
		"    listen 80 default_server;",
		"    listen [::]:80 default_server;",
		"    server_name _;",
	}
	lines = append(lines, renderACMEChallengeLocation(acmeUpstream)...)
	lines = append(lines, "    location ^~ / {", "        return 404;", "    }", "}")
	return strings.Join(lines, "\n")
}

func renderACMEChallengeLocation(acmeUpstream string) []string {
	return []string{
		"    location ^~ /.well-known/acme-challenge/ {",
		"        proxy_pass " + acmeUpstream + "/api/acme-challenge/;",
		"        proxy_set_header Host $host;",
		"        proxy_set_header X-Forwarded-Proto $scheme;",
		"        proxy_set_header X-Real-IP $remote_addr;",
		"    }",
	}
}

func normalizeACMEUpstream(value string) (string, error) {
	if value == "" {
		value = defaultACMEUpstream
	}
	upstream := strings.TrimRight(strings.TrimSpace(value), "/")
	if !acmeUpstreamPattern.MatchString(upstream) {
		return "", errors.New("Invalid NGINX ACME upstream: " + value)
	}
	return upstream, nil
}

func renderCommonProxyDirectives(settings proxySettings) []string {
	custom := nginxDirectiveNames(settings.NginxDirectives)
	lines := []string{}
	addDefault := func(name, line string) {
		if !custom[name] {
			lines = append(lines, line)
		}
	}

	addDefault("proxy_set_header", "    proxy_set_header Host $host;")
	addDefault("proxy_set_header", "    proxy_set_header X-Real-IP $remote_addr;")
	addDefault("proxy_set_header", "    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;")
	addDefault("proxy_set_header", "    proxy_set_header X-Forwarded-Proto $scheme;")
	addDefault("proxy_http_version", "    proxy_http_version 1.1;")
	addDefault("proxy_connect_timeout", fmt.Sprintf("    proxy_connect_timeout %ds;", intOr(settings.Timeouts.ConnectSeconds, 5)))
	addDefault("proxy_send_timeout", fmt.Sprintf("    proxy_send_timeout %ds;", intOr(settings.Timeouts.SendReadSeconds, 60)))
	addDefault("proxy_read_timeout", fmt.Sprintf("    proxy_read_timeout %ds;", intOr(settings.Timeouts.SendReadSeconds, 60)))
	addDefault("client_header_timeout", fmt.Sprintf("    client_header_timeout %ds;", intOr(settings.Timeouts.ClientHeaderSeconds, 15)))
	addDefault("client_max_body_size", fmt.Sprintf("    client_max_body_size %dm;", intOr(settings.Timeouts.BodyLimitMb, 10)))
	if settings.Origin.Protocol == "https" {
		verify := "off"
		if settings.BackendTLSVerify {
			verify = "on"
		}
		addDefault("proxy_ssl_verify", "    proxy_ssl_verify "+verify+";")
	}
	if settings.Websocket {
		addDefault("proxy_set_header", "    proxy_set_header Upgrade $http_upgrade;")
		addDefault("proxy_set_header", "    proxy_set_header Connection $connection_upgrade;")
		addDefault("proxy_buffering", "    proxy_buffering off;")
	}
	if settings.cacheEnabled() {
		addDefault("proxy_cache", "    proxy_cache proxycore_cache;")
	}
	if !custom["proxy_set_header"] {
		for _, header := range settings.Headers {
			lines = append(lines, "    proxy_set_header "+header.Name+" "+nginxValue(header.Value)+";")
		}
	}
	lines = append(lines, renderNginxDirectives(settings.NginxDirectives)...)
	return lines
}

func renderNginxDirectives(directives string) []string {
	lines := []string{}
	for _, line := range strings.Split(directives, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" {
			lines = append(lines, "    "+trimmed)
		}
	}
	return lines
}

func nginxDirectiveNames(directives string) map[string]bool {
	names := map[string]bool{}
	for _, line := range strings.Split(directives, "\n") {
		stripped := strings.TrimSpace(commentPattern.ReplaceAllString(line, ""))
		if match := directiveNamePattern.FindStringSubmatch(stripped); match != nil {
			names[match[1]] = true
		}
	}
	return names
}

func renderPathLocations(rules []pathRule, origin string, settings proxySettings) []string {
	locations := append([]pathRule(nil), rules...)
	sort.SliceStable(locations, func(i, j int) bool {
		left, right := locations[i], locations[j]
		if left.Kind != right.Kind {
			return left.Kind == "exact"
		}
		return len(left.Pattern) > len(right.Pattern)
	})
	hasRootPrefix := false
	for _, rule := range locations {
		if rule.Kind == "prefix" && rule.Pattern == "/" {
			hasRootPrefix = true
		}
	}
	if !hasRootPrefix {
		locations = append(locations, pathRule{Kind: "prefix", Pattern: "/", Action: pathAction{Type: "proxy"}})
	}

	blocks := make([]string, 0, len(locations))
	for _, rule := range locations {
		location := "    location " + rule.Pattern + " {"
		if rule.Kind == "exact" {
			location = "    location = " + rule.Pattern + " {"
		}
		if rule.Action.Type == "redirect" {
			target := "/"
			if rule.Action.Location != nil {
				target = *rule.Action.Location
			}
			blocks = append(blocks, strings.Join([]string{
				location,
				"        return " + strconv.Itoa(rule.Action.Status) + " " + nginxValue(target) + ";",
				"    }",
			}, "\n"))
			continue
		}
		passTarget := origin
		if rule.Action.Rewrite != "" {
			passTarget = origin + rule.Action.Rewrite
		}
		lines := []string{location, "        proxy_pass " + passTarget + ";"}
		if settings.Websocket {
			lines = append(lines, "        proxy_buffering off;")
		}
		lines = append(lines, "    }")
		blocks = append(blocks, strings.Join(lines, "\n"))
	}
	return blocks
}

func renderStreams(streams []domain.StreamRoute) string {
	servers := []string{}
	for _, stream := range streams {
		if !stream.Enabled {
			continue
		}
		udp := stream.Protocol == "udp"
		suffix := ""
		if udp {
			suffix = " udp"
		}
		lines := []string{
			"    server {",
			"        listen " + formatHost(stream.ListenAddress) + ":" + strconv.Itoa(stream.ListenPort) + suffix + ";",
			"        proxy_pass " + formatHost(stream.Upstream.IP) + ":" + strconv.Itoa(stream.Upstream.Port) + ";",
		}
		if udp {
			lines = append(lines, "        proxy_timeout 60s;")
		}
		lines = append(lines, "    }")
		servers = append(servers, strings.Join(lines, "\n"))
	}
	return "stream {\n" + strings.Join(servers, "\n\n") + "\n}"
}

func basicAuthFilePath(secretID, candidatePath string) string {
	if candidatePath == "" {
		return "/run/proxycore/basic-auth/" + secretID
	}
	return candidatePath + "/basic-auth/" + secretID
}

func certificateFilePath(certificateID, candidatePath, extension string) string {
	if candidatePath == "" {
		return "/etc/nginx/certs/" + certificateID + "." + extension
	}
	return candidatePath + "/certs/" + certificateID + "." + extension
}

func formatHost(host string) string {
	if strings.Contains(host, ":") {
		return "[" + host + "]"
	}
	return host
}

func nginxValue(value string) string {
	if plainValuePattern.MatchString(value) {
		return value
	}
	escaped := strings.ReplaceAll(value, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `"`, `\"`)
	return `"` + escaped + `"`
}
