package enrollment

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
)

func TestSanitizedMessage(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, "none"},
		{"plain text kept", errors.New("archive insert failed: no rows"), "archive insert failed: no rows"},
		{"whitespace collapsed", errors.New("a \n\t b   c"), "a b c"},
		{"uuid", errors.New("archive 123e4567-e89b-12d3-a456-426614174000 missing"), "archive <redacted> missing"},
		{"hex hash", errors.New("hash " + strings.Repeat("ab", 32) + " mismatch"), "hash <redacted> mismatch"},
		{"url", errors.New("get https://primary.example:3443/api/x?y=1 failed"), "get <redacted> failed"},
		{"email", errors.New("owner admin@example.com denied"), "owner <redacted> denied"},
		{"ipv4", errors.New("dial 192.0.2.7:443 refused"), "dial <redacted>:443 refused"},
		{"ipv6", errors.New("dial 2001:db8::1 refused"), "dial <redacted> refused"},
		{"long token", errors.New("bad pcenr1_AbCdEfGhIjKlMnOpQrStUvWx value"), "bad <redacted> value"},
		{"base64", errors.New("payload dGhpcyBpcyBhIHNlY3JldCBwYXlsb2Fk== end"), "payload <redacted> end"},
		{"key value", errors.New("rejected token=abc123 now"), "rejected <redacted> now"},
		{"password colon", errors.New("password: hunter2"), "<redacted>"},
		{"wrapped", fmt.Errorf("outer: %w", errors.New("inner")), "outer: inner"},
		{"prose secrets colon kept", errors.New("import secrets: secret is incomplete (index 3)"), "import secrets: secret is incomplete (index 3)"},
		{"prose with redacted id kept", errors.New("import secrets: secret 123e4567-e89b-12d3-a456-426614174000 cannot be unwrapped"), "import secrets: secret <redacted> cannot be unwrapped"},
		{"prose key colon kept", errors.New("cluster key: unavailable"), "cluster key: unavailable"},
		{"secret equals value", errors.New("bad secret=sup3rS3cr3t now"), "bad <redacted> now"},
		{"secret equals plain word", errors.New("bad secret=hunter now"), "bad <redacted> now"},
		{"password colon long token", errors.New("password: hunter2xxxxxxxxxxxx"), "<redacted>"},
		{"token colon mixed value", errors.New("rejected token: ab12cd34 by peer"), "rejected <redacted> by peer"},
		{"api key colon long letters", errors.New("api key: abcdefghijklmnop end"), "api <redacted> end"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SanitizedMessage(tc.err); got != tc.want {
				t.Fatalf("SanitizedMessage() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSanitizedMessageTruncates(t *testing.T) {
	got := SanitizedMessage(errors.New(strings.Repeat("word ", 200)))
	if n := utf8.RuneCountInString(got); n > sanitizedMessageLimit {
		t.Fatalf("length = %d, want <= %d", n, sanitizedMessageLimit)
	}
	multi := SanitizedMessage(errors.New(strings.Repeat("é ", 300)))
	if !utf8.ValidString(multi) {
		t.Fatalf("truncation split a rune: %q", multi)
	}
}

func TestSanitizedMessageNeverLeaks(t *testing.T) {
	secrets := []string{
		"pcenr1_SECRETSECRETSECRETSECRET0001",
		"https://primary.example/exchange",
		"user@example.com",
		"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		"123e4567-e89b-12d3-a456-426614174000",
		"10.20.30.40",
		"hunter2hunter2",
	}
	err := fmt.Errorf("step failed: %w", errors.New(strings.Join(append(secrets[:len(secrets)-1:len(secrets)-1], "secret="+secrets[len(secrets)-1]), " ")))
	got := SanitizedMessage(err)
	for _, s := range secrets {
		if strings.Contains(got, s) {
			t.Fatalf("message leaks %q: %q", s, got)
		}
	}
}

func TestImportSecretDiagnosticsAreReadableAndNamedInLogs(t *testing.T) {
	err := fmt.Errorf("importer: enqueue apply job: %w", fmt.Errorf("%w (index 2)", configuration.ErrImportSecretPurposeMissing))
	if got := SanitizedMessage(err); !strings.Contains(got, "import secrets: secret purpose is missing (index 2)") {
		t.Fatalf("sanitized message hides the reason: %q", got)
	}
	if got := RedactedCause(err); !strings.Contains(got, "ErrImportSecretPurposeMissing") {
		t.Fatalf("sentinel not named: %q", got)
	}
}
