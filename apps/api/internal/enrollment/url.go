package enrollment

import (
	"errors"
	"net"
	"net/url"
	"strconv"
	"strings"
)

const (
	MaxPrimaryURLLength  = 2048
	MaxPrimaryHostLength = 253
)

var ErrInvalidPrimaryURL = errors.New("invalid primary HTTPS URL")

// CanonicalizePrimaryURL validates the origin used for enrollment and returns
// its stable HTTPS form. Enrollment addresses are exact origins: redirects,
// paths, query strings, fragments, and userinfo are deliberately unsupported.
func CanonicalizePrimaryURL(raw string) (string, error) {
	if raw == "" || len(raw) > MaxPrimaryURLLength || strings.TrimSpace(raw) != raw {
		return "", ErrInvalidPrimaryURL
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed == nil || !strings.EqualFold(parsed.Scheme, "https") || parsed.Opaque != "" || parsed.Host == "" || parsed.User != nil {
		return "", ErrInvalidPrimaryURL
	}
	if parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.RawFragment != "" || strings.Contains(raw, "#") {
		return "", ErrInvalidPrimaryURL
	}
	if (parsed.Path != "" && parsed.Path != "/") || parsed.RawPath != "" {
		return "", ErrInvalidPrimaryURL
	}

	host, portText, err := net.SplitHostPort(parsed.Host)
	if err != nil || host == "" || portText == "" {
		return "", ErrInvalidPrimaryURL
	}
	if strings.HasPrefix(parsed.Host, "[") && !strings.Contains(host, ":") {
		return "", ErrInvalidPrimaryURL
	}
	port, err := canonicalPrimaryPort(portText)
	if err != nil {
		return "", ErrInvalidPrimaryURL
	}
	host, err = canonicalPrimaryHost(host)
	if err != nil {
		return "", ErrInvalidPrimaryURL
	}
	return (&url.URL{Scheme: "https", Host: net.JoinHostPort(host, strconv.Itoa(port)), Path: "/"}).String(), nil
}

func canonicalPrimaryPort(raw string) (int, error) {
	for i := 0; i < len(raw); i++ {
		if raw[i] < '0' || raw[i] > '9' {
			return 0, ErrInvalidPrimaryURL
		}
	}
	port, err := strconv.ParseUint(raw, 10, 16)
	if err != nil || port == 0 {
		return 0, ErrInvalidPrimaryURL
	}
	return int(port), nil
}

func canonicalPrimaryHost(raw string) (string, error) {
	if raw == "" || strings.TrimSpace(raw) != raw || strings.Contains(raw, "%") {
		return "", ErrInvalidPrimaryURL
	}
	if ip := net.ParseIP(raw); ip != nil {
		// IPv4-mapped IPv6 has two textual identities; do not silently turn
		// its bracketed form into a different IPv4 origin.
		if strings.Contains(raw, ":") && ip.To4() != nil {
			return "", ErrInvalidPrimaryURL
		}
		return ip.String(), nil
	}

	canonical := strings.TrimSuffix(strings.ToLower(raw), ".")
	if canonical == "" || len(canonical) > MaxPrimaryHostLength {
		return "", ErrInvalidPrimaryURL
	}
	labels := strings.Split(canonical, ".")
	if len(labels) == 4 && allDecimalLabels(labels) {
		return "", ErrInvalidPrimaryURL
	}
	for _, label := range labels {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", ErrInvalidPrimaryURL
		}
		for i := 0; i < len(label); i++ {
			if (label[i] < 'a' || label[i] > 'z') && (label[i] < '0' || label[i] > '9') && label[i] != '-' {
				return "", ErrInvalidPrimaryURL
			}
		}
	}
	return canonical, nil
}

func allDecimalLabels(labels []string) bool {
	for _, label := range labels {
		for i := 0; i < len(label); i++ {
			if label[i] < '0' || label[i] > '9' {
				return false
			}
		}
	}
	return true
}
