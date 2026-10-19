package enrollment

import (
	"regexp"
	"strings"
)

const (
	sanitizedMessageLimit = 240
	redactedPlaceholder   = "<redacted>"
)

// sanitizeRules run in order; broader structures (URLs, key=value pairs) go
// first so later, narrower rules never see their remains.
var sanitizeRules = []*regexp.Regexp{
	regexp.MustCompile(`(?i)[a-z][a-z0-9+.-]*://\S+`),
	regexp.MustCompile(`(?i)[\w.-]*(?:token|key|secret|password|passwd|credential|authorization)[\w.-]*\s*[=:]\s*\S+`),
	regexp.MustCompile(`(?i)\bpcenr\w*_[\w.+/=-]*`),
	regexp.MustCompile(`[\w.+-]+@[\w-]+(?:\.[\w-]+)+`),
	regexp.MustCompile(`(?i)[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`),
	regexp.MustCompile(`(?i)[0-9a-f]{32,}`),
	regexp.MustCompile(`(?i)(?:[0-9a-f]{0,4}:){2,7}[0-9a-f]{0,4}`),
	regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}\b`),
	regexp.MustCompile(`[A-Za-z0-9+/_=-]{24,}`),
}

var whitespaceRun = regexp.MustCompile(`\s+`)

// SanitizedMessage renders err's text for operator logs with everything that
// could identify or authenticate (tokens, keys, URLs, hashes, addresses, ids)
// replaced by a placeholder, whitespace collapsed and length bounded. It is
// deliberately lossy: it exists so a failure reads as prose, never as data.
func SanitizedMessage(err error) string {
	if err == nil {
		return "none"
	}
	text := err.Error()
	for _, rule := range sanitizeRules {
		text = rule.ReplaceAllString(text, redactedPlaceholder)
	}
	text = strings.TrimSpace(whitespaceRun.ReplaceAllString(text, " "))
	if text == "" {
		return "empty"
	}
	if runes := []rune(text); len(runes) > sanitizedMessageLimit {
		text = string(runes[:sanitizedMessageLimit-3]) + "..."
	}
	return text
}
