package enrollment

import (
	"regexp"
	"strings"
	"unicode"
)

const (
	sanitizedMessageLimit = 240
	redactedPlaceholder   = "<redacted>"

	credentialWords = `(?:token|key|secret|password|passwd|credential|authorization)`
	// credentialRunMinLength is the shortest colon-separated value redacted on
	// length alone: a run this long without spaces is not prose.
	credentialRunMinLength = 12
)

// sanitizeRule redacts every match of pattern. When keep is set it receives the
// first capture group of a match and returns true to leave that match intact.
type sanitizeRule struct {
	pattern *regexp.Regexp
	keep    func(value string) bool
}

func replaceAll(pattern *regexp.Regexp) sanitizeRule { return sanitizeRule{pattern: pattern} }

func (r sanitizeRule) apply(text string) string {
	if r.keep == nil {
		return r.pattern.ReplaceAllString(text, redactedPlaceholder)
	}
	return r.pattern.ReplaceAllStringFunc(text, func(match string) string {
		groups := r.pattern.FindStringSubmatch(match)
		if len(groups) > 1 && r.keep(groups[1]) {
			return match
		}
		return redactedPlaceholder
	})
}

var credentialRun = regexp.MustCompile(`^[A-Za-z0-9+/_=.-]+$`)

// isProseValue reports whether the value after "key:" reads as ordinary prose
// rather than a credential. A value is a credential when it is a long run of
// credential characters or mixes letters with digits (e.g. "hunter2").
func isProseValue(value string) bool {
	if len(value) >= credentialRunMinLength && credentialRun.MatchString(value) {
		return false
	}
	var letters, digits bool
	for _, r := range value {
		letters = letters || unicode.IsLetter(r)
		digits = digits || unicode.IsDigit(r)
	}
	return !(letters && digits)
}

// sanitizeRules run in order; broader structures (URLs, key=value pairs) go
// first so later, narrower rules never see their remains.
var sanitizeRules = []sanitizeRule{
	replaceAll(regexp.MustCompile(`(?i)[a-z][a-z0-9+.-]*://\S+`)),
	// key=value is an assignment: always redacted.
	replaceAll(regexp.MustCompile(`(?i)[\w.-]*` + credentialWords + `[\w.-]*\s*=\s*\S+`)),
	// key: value is also ordinary prose ("import secrets: secret is incomplete"),
	// so it is redacted only when the value looks like a credential.
	{pattern: regexp.MustCompile(`(?i)[\w.-]*` + credentialWords + `[\w.-]*\s*:\s*(\S+)`), keep: isProseValue},
	replaceAll(regexp.MustCompile(`(?i)\bpcenr\w*_[\w.+/=-]*`)),
	replaceAll(regexp.MustCompile(`[\w.+-]+@[\w-]+(?:\.[\w-]+)+`)),
	replaceAll(regexp.MustCompile(`(?i)[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)),
	replaceAll(regexp.MustCompile(`(?i)[0-9a-f]{32,}`)),
	replaceAll(regexp.MustCompile(`(?i)(?:[0-9a-f]{0,4}:){2,7}[0-9a-f]{0,4}`)),
	replaceAll(regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}\b`)),
	replaceAll(regexp.MustCompile(`[A-Za-z0-9+/_=-]{24,}`)),
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
		text = rule.apply(text)
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
