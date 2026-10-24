package renderers

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
)

// Http3Capabilities describes what the nginx binary and host publish for HTTP/3.
type Http3Capabilities struct {
	Http3Module     bool `json:"http3Module"`
	TCP443Published bool `json:"tcp443Published"`
	UDP443Published bool `json:"udp443Published"`
}

// proxySettings is the typed, normalized view of DNSRecord.Proxy that the
// nginx renderer works with. Optional fields that the TypeScript domain treats
// as "undefined" are pointers so defaults apply only when the value is absent.
type proxySettings struct {
	Origin struct {
		IP       string `json:"ip"`
		Port     int    `json:"port"`
		Protocol string `json:"protocol"`
	} `json:"origin"`
	TLSEnabled          bool   `json:"tlsEnabled"`
	CertificateID       string `json:"certificateId"`
	HTTP2               *bool  `json:"http2"`
	HTTP3               bool   `json:"http3"`
	RedirectHTTPToHTTPS *bool  `json:"redirectHttpToHttps"`
	BackendTLSVerify    bool   `json:"backendTlsVerify"`
	Websocket           bool   `json:"websocket"`
	Cache               *struct {
		Enabled bool `json:"enabled"`
	} `json:"cache"`
	BasicAuth *struct {
		Username         string `json:"username"`
		PasswordSecretID string `json:"passwordSecretId"`
	} `json:"basicAuth"`
	Timeouts struct {
		ConnectSeconds      *int `json:"connectSeconds"`
		SendReadSeconds     *int `json:"sendReadSeconds"`
		ClientHeaderSeconds *int `json:"clientHeaderSeconds"`
		BodyLimitMb         *int `json:"bodyLimitMb"`
	} `json:"timeouts"`
	Headers         []headerRule `json:"headers"`
	PathRules       []pathRule   `json:"pathRules"`
	NginxDirectives string       `json:"nginxDirectives"`
}

type headerRule struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type pathRule struct {
	Kind    string     `json:"kind"`
	Pattern string     `json:"pattern"`
	Action  pathAction `json:"action"`
}

type pathAction struct {
	Type     string  `json:"type"`
	Status   int     `json:"status"`
	Location *string `json:"location"`
	Rewrite  string  `json:"rewrite"`
}

// http2 mirrors `settings.http2 ?? settings.tlsEnabled`.
func (s proxySettings) http2() bool {
	if s.HTTP2 != nil {
		return *s.HTTP2
	}
	return s.TLSEnabled
}

// redirectHTTPToHTTPS mirrors `settings.redirectHttpToHttps ?? true`.
func (s proxySettings) redirectHTTPToHTTPS() bool {
	if s.RedirectHTTPToHTTPS != nil {
		return *s.RedirectHTTPToHTTPS
	}
	return true
}

func (s proxySettings) cacheEnabled() bool {
	return s.Cache != nil && s.Cache.Enabled
}

func intOr(value *int, fallback int) int {
	if value != nil {
		return *value
	}
	return fallback
}

// normalizeProxySettings validates a record's proxy map with the domain
// validator and returns the normalized typed settings, matching what the
// TypeScript validateProxySettings returns. The domain validator has no way to
// receive HTTP/3 capabilities, so the http3 flag is checked here and removed
// from the copy handed to the domain validator.
func normalizeProxySettings(proxy map[string]any, capabilities Http3Capabilities) (proxySettings, error) {
	var settings proxySettings
	forDomain := make(map[string]any, len(proxy))
	for key, value := range proxy {
		forDomain[key] = value
	}
	http3, _ := proxy["http3"].(bool)
	delete(forDomain, "http3")
	if _, err := domain.ValidateProxySettings(forDomain); err != nil {
		return settings, err
	}
	tlsEnabled, _ := proxy["tlsEnabled"].(bool)
	if http3 {
		if !(capabilities.Http3Module && capabilities.TCP443Published && capabilities.UDP443Published) {
			return settings, &domain.ValidationError{Message: "HTTP/3 requires binary support and published TCP/UDP 443", Code: "HTTP3_UNAVAILABLE"}
		}
		if !tlsEnabled {
			return settings, &domain.ValidationError{Message: "HTTP/3 requires client TLS", Code: "HTTP3_TLS"}
		}
	}

	raw, err := json.Marshal(proxy)
	if err != nil {
		return settings, fmt.Errorf("encode proxy settings: %w", err)
	}
	if err := json.Unmarshal(raw, &settings); err != nil {
		return settings, fmt.Errorf("decode proxy settings: %w", err)
	}

	settings.NginxDirectives = normalizeDirectives(settings.NginxDirectives)
	for index := range settings.Headers {
		settings.Headers[index].Name = strings.ToLower(settings.Headers[index].Name)
	}
	for index := range settings.PathRules {
		settings.PathRules[index].Pattern = normalizePath(settings.PathRules[index].Pattern)
	}
	return settings, nil
}

var lineBreakPattern = regexp.MustCompile(`\r\n?`)

// normalizeDirectives mirrors validateNginxDirectives' normalization.
func normalizeDirectives(directives string) string {
	return strings.TrimSpace(lineBreakPattern.ReplaceAllString(directives, "\n"))
}

// normalizePath mirrors validateDnsPath: trim, then drop trailing slashes
// unless the path is just "/".
func normalizePath(path string) string {
	normalized := strings.TrimSpace(path)
	if len(normalized) > 1 {
		return strings.TrimRight(normalized, "/")
	}
	return normalized
}
