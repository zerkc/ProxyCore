package apply

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/zerkc/ProxyCore/apps/api/internal/secrets"
)

// inlineSecret matches `key=value` or `key: value` where the key looks
// sensitive, using the same words as secrets.RedactSecrets.
var inlineSecret = regexp.MustCompile(`(?i)((?:pass(?:word)?|secret|token|private[ _.-]?key|ciphertext|authorization)\w*["']?\s*[:=]\s*)("[^"]*"|'[^']*'|\S+)`)

// scrubMessage removes secret values from an error message before it is stored
// on a job. A message that is itself a JSON document goes through
// secrets.RedactSecrets (sensitive keys become [REDACTED]); free text has the
// values of sensitive-looking key=value pairs replaced.
func scrubMessage(message string) string {
	trimmed := strings.TrimSpace(message)
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		var decoded any
		if err := json.Unmarshal([]byte(trimmed), &decoded); err == nil {
			return string(mustMarshal(secrets.RedactSecrets(decoded)))
		}
	}
	return inlineSecret.ReplaceAllString(message, "${1}[REDACTED]")
}
