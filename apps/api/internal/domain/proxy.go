package domain

import (
	"regexp"
	"strings"
)

var (
	basicAuthUsernamePattern = regexp.MustCompile(`^[A-Za-z0-9._@+=-]{1,64}$`)
	basicAuthSecretPattern   = regexp.MustCompile(`^[A-Za-z0-9_-]{8,128}$`)
	headerNamePattern        = regexp.MustCompile("^[!#$%&'*+\\-.^_`|~0-9A-Za-z]+$")
	nginxBlockContextPattern = regexp.MustCompile(`(?im)^\s*(events|http|server|stream|location)\b`)
)

func proxyBool(proxy map[string]any, key string) bool {
	value, _ := proxy[key].(bool)
	return value
}

func proxyString(proxy map[string]any, key string) string {
	value, _ := proxy[key].(string)
	return value
}

// ValidateProxySettings validates a proxy settings object.
func ValidateProxySettings(proxy map[string]any) (result map[string]any, err error) {
	defer recoverValidation(&err)
	result = validateProxySettings(proxy, nil)
	return
}

func validateProxySettings(proxy map[string]any, capabilities map[string]bool) map[string]any {
	origin, _ := proxy["origin"].(map[string]any)
	assertDomain(origin != nil, "Proxy origin must be a literal IP", "ORIGIN_IP")
	assertDomain(ipVersion(proxyString(origin, "ip")) > 0, "Proxy origin must be a literal IP", "ORIGIN_IP")
	port, portOK := toInt(origin["port"])
	assertDomain(portOK && port >= 1 && port <= 65_535, "Port is invalid", "ORIGIN_PORT")
	protocol := proxyString(origin, "protocol")
	assertDomain(protocol == "http" || protocol == "https", "Proxy origin protocol must be http or https", "ORIGIN_PROTOCOL")
	_, tlsPresent := proxy["tlsEnabled"].(bool)
	assertDomain(tlsPresent, "TLS setting is required", "TLS_REQUIRED")
	tlsEnabled := proxyBool(proxy, "tlsEnabled")

	if basicAuth, ok := proxy["basicAuth"].(map[string]any); ok && basicAuth != nil {
		assertDomain(tlsEnabled, "Basic Auth requires client TLS", "BASIC_AUTH_TLS")
		assertDomain(basicAuthUsernamePattern.MatchString(proxyString(basicAuth, "username")), "Basic Auth username is invalid", "BASIC_AUTH_USERNAME")
		assertDomain(basicAuthSecretPattern.MatchString(proxyString(basicAuth, "passwordSecretId")), "Basic Auth secret reference is invalid", "BASIC_AUTH_SECRET")
	}

	if proxyBool(proxy, "http3") {
		assertDomain(
			capabilities != nil && capabilities["http3Module"] && capabilities["tcp443Published"] && capabilities["udp443Published"],
			"HTTP/3 requires binary support and published TCP/UDP 443", "HTTP3_UNAVAILABLE",
		)
		assertDomain(tlsEnabled, "HTTP/3 requires client TLS", "HTTP3_TLS")
	}

	validatePathRules(sliceOf(proxy["pathRules"]))
	validateHeaderRules(sliceOf(proxy["headers"]))
	validateNginxDirectives(proxy["nginxDirectives"])

	if cache, ok := proxy["cache"].(map[string]any); ok && proxyBool(cache, "enabled") {
		_, hasBasicAuth := proxy["basicAuth"].(map[string]any)
		assertDomain(!hasBasicAuth, "Authenticated routes cannot enable cache", "CACHE_AUTH")
		assertDomain(!proxyBool(proxy, "websocket"), "WebSocket routes cannot enable cache", "CACHE_WEBSOCKET")
	}

	timeouts, _ := proxy["timeouts"].(map[string]any)
	connect := timeoutOrDefault(timeouts, "connectSeconds", 5)
	sendRead := timeoutOrDefault(timeouts, "sendReadSeconds", 60)
	clientHeader := timeoutOrDefault(timeouts, "clientHeaderSeconds", 15)
	bodyLimit := timeoutOrDefault(timeouts, "bodyLimitMb", 10)
	assertDomain(connect > 0 && connect <= 300, "Connect timeout is invalid", "")
	assertDomain(sendRead > 0 && sendRead <= 3_600, "Send/read timeout is invalid", "")
	assertDomain(clientHeader > 0 && clientHeader <= 300, "Client timeout is invalid", "")
	assertDomain(bodyLimit > 0 && bodyLimit <= 1_024, "Body limit is invalid", "")

	return proxy
}

func timeoutOrDefault(timeouts map[string]any, key string, fallback int) int {
	if timeouts == nil {
		return fallback
	}
	if value, ok := toInt(timeouts[key]); ok {
		return value
	}
	return fallback
}

func validateNginxDirectives(value any) {
	if value == nil {
		return
	}
	directives, ok := value.(string)
	assertDomain(ok, "Nginx directives must be text", "NGINX_DIRECTIVES_TYPE")
	normalized := strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(directives, "\r\n", "\n"), "\r", "\n"))
	if normalized == "" {
		return
	}
	assertDomain(len(normalized) <= 32_000, "Nginx directives are too long", "NGINX_DIRECTIVES_LENGTH")
	assertDomain(!strings.Contains(normalized, "\x00"), "Nginx directives contain an invalid character", "NGINX_DIRECTIVES_CHARACTERS")
	assertDomain(!containsUnquotedBrace(normalized), "Nginx directives cannot contain configuration blocks", "NGINX_DIRECTIVES_BLOCK")
	assertDomain(!nginxBlockContextPattern.MatchString(normalized), "Nginx block directives are not allowed here", "NGINX_DIRECTIVES_CONTEXT")
}

func containsUnquotedBrace(value string) bool {
	var quote rune
	escaped := false
	for _, character := range value {
		if escaped {
			escaped = false
			continue
		}
		if quote != 0 && character == '\\' {
			escaped = true
			continue
		}
		if quote != 0 {
			if character == quote {
				quote = 0
			}
			continue
		}
		if character == '"' || character == '\'' {
			quote = character
			continue
		}
		if character == '{' || character == '}' {
			return true
		}
	}
	return false
}

func validatePathRules(rules []any) {
	seen := map[string]bool{}
	patterns := []string{}
	for _, raw := range rules {
		rule, _ := raw.(map[string]any)
		assertDomain(rule != nil, "Path rule must be an object", "PATH_RULE")
		pattern := validateDNSPath(proxyString(rule, "pattern"))
		kind := proxyString(rule, "kind")
		key := kind + ":" + pattern
		assertDomain(!seen[key], "Duplicate path rule: "+pattern, "PATH_DUPLICATE")
		for _, existing := range patterns {
			assertDomain(existing != pattern, "Ambiguous path rule: "+pattern, "PATH_AMBIGUOUS")
		}
		seen[key] = true
		patterns = append(patterns, pattern)
		action, _ := rule["action"].(map[string]any)
		if action != nil && proxyString(action, "type") == "redirect" {
			status, _ := toInt(action["status"])
			assertDomain(status == 301 || status == 302 || status == 307 || status == 308, "Redirect status is not allowed", "REDIRECT_STATUS")
			location := proxyString(action, "location")
			assertDomain(location == "" || !crlfPattern.MatchString(location), "Redirect location cannot contain newlines", "REDIRECT_LOCATION")
		} else if action != nil {
			if rewrite := proxyString(action, "rewrite"); rewrite != "" {
				validateDNSPath(rewrite)
			}
		}
	}
}

func validateHeaderRules(headers []any) {
	seen := map[string]bool{}
	for _, raw := range headers {
		header, _ := raw.(map[string]any)
		assertDomain(header != nil, "Header rule must be an object", "HEADER_RULE")
		name := proxyString(header, "name")
		assertDomain(headerNamePattern.MatchString(name), "Invalid header name: "+name, "HEADER_NAME")
		assertDomain(!crlfPattern.MatchString(proxyString(header, "value")), "Header value cannot contain newlines", "HEADER_VALUE")
		lower := strings.ToLower(name)
		assertDomain(!seen[lower], "Duplicate header: "+name, "HEADER_DUPLICATE")
		seen[lower] = true
	}
}

func validateDNSPath(path string) string {
	normalized := strings.TrimSpace(path)
	assertDomain(strings.HasPrefix(normalized, "/"), "Path must start with /", "PATH_START")
	assertDomain(!regexp.MustCompile(`[\r\n?#\[\]]`).MatchString(normalized), "Path contains unsupported characters", "PATH_CHARACTERS")
	assertDomain(len(normalized) <= 2_048, "Path is too long", "PATH_LENGTH")
	if len(normalized) > 1 {
		return strings.TrimRight(normalized, "/")
	}
	return normalized
}

// ValidateStreamRoutes validates a set of stream routes.
func ValidateStreamRoutes(routes []StreamRoute) (result []StreamRoute, err error) {
	defer recoverValidation(&err)
	result = validateStreamRoutes(routes)
	return
}

func validateStreamRoutes(routes []StreamRoute) []StreamRoute {
	listeners := map[string]bool{}
	for _, route := range routes {
		assertDomain(ipVersion(route.ListenAddress) > 0, "Stream listen address must be a literal IP", "STREAM_LISTEN_IP")
		assertDomain(route.ListenPort >= 1 && route.ListenPort <= 65_535, "Port is invalid", "STREAM_LISTEN_PORT")
		assertDomain(route.Upstream.Protocol == route.Protocol, "Stream protocol and upstream protocol must match", "STREAM_PROTOCOL")
		assertDomain(ipVersion(route.Upstream.IP) > 0, "Stream upstream must be a literal IP", "STREAM_UPSTREAM_IP")
		assertDomain(route.Upstream.Port >= 1 && route.Upstream.Port <= 65_535, "Port is invalid", "STREAM_UPSTREAM_PORT")
		key := route.Protocol + ":" + route.ListenAddress + ":" + itoa(route.ListenPort)
		assertDomain(!listeners[key], "Stream listener conflict: "+key, "STREAM_CONFLICT")
		listeners[key] = true
	}
	return routes
}

func sliceOf(value any) []any {
	if value == nil {
		return nil
	}
	if slice, ok := value.([]any); ok {
		return slice
	}
	return nil
}
