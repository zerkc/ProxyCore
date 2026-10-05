package sqlitedb

import (
	"fmt"
	"strings"
	"testing"
)

func TestUUIDLookupValid(t *testing.T) {
	const canonical = "a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11"
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"canonical", canonical, canonical},
		{"uppercase", "A0EEBC99-9C0B-4EF8-BB6D-6BB9BD380A11", canonical},
		{"compact", "a0eebc999c0b4ef8bb6d6bb9bd380a11", canonical},
		{"braced", "{" + canonical + "}", canonical},
		{"braced compact", "{A0EEBC999C0B4EF8BB6D6BB9BD380A11}", canonical},
		{"mixed case", "a0EeBc99-9C0b-4eF8-bB6d-6Bb9bD380A11", canonical},
		{"partial hyphens", "a0eebc99-9c0b4ef8-bb6d6bb9bd380a11", canonical},
		{"all seven hyphens", "a0ee-bc99-9c0b-4ef8-bb6d-6bb9-bd38-0a11", canonical},
		{"nil UUID", "00000000000000000000000000000000", "00000000-0000-0000-0000-000000000000"},
		{"all bits set", "FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF", "ffffffff-ffff-ffff-ffff-ffffffffffff"},
		{"unrestricted version and variant", "01234567-89ab-fdef-0123-456789abcdef", "01234567-89ab-fdef-0123-456789abcdef"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseUUIDLookupText(tt.input)
			if err != nil || got != tt.want {
				t.Fatalf("ParseUUIDLookupText() = %q, %v; want %q, nil", got, err, tt.want)
			}
		})
	}
}

// Each bit controls one of the seven optional four-digit separators.
// Expected canonical values are literal goldens, not produced by the codec.
func TestUUIDLookupAllHyphenLayouts(t *testing.T) {
	goldens := []string{
		"a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11",
		"00000000-0000-0000-0000-000000000000",
		"ffffffff-ffff-ffff-ffff-ffffffffffff",
		"01234567-89ab-1def-8123-456789abcdef",
		"01234567-89ab-7def-9123-456789abcdef",
		"01234567-89ab-fdef-0123-456789abcdef",
	}
	for pattern, want := range goldens {
		digits := strings.ReplaceAll(want, "-", "")
		for mask := 0; mask < 128; mask++ {
			for _, upper := range []bool{false, true} {
				for _, braced := range []bool{false, true} {
					name := fmt.Sprintf("pattern%d/layout%03d/upper%t/braced%t", pattern, mask, upper, braced)
					t.Run(name, func(t *testing.T) {
						var input strings.Builder
						if braced {
							input.WriteByte('{')
						}
						for group := 0; group < 8; group++ {
							part := digits[group*4 : group*4+4]
							if upper {
								part = strings.ToUpper(part)
							}
							input.WriteString(part)
							if group < 7 && mask&(1<<group) != 0 {
								input.WriteByte('-')
							}
						}
						if braced {
							input.WriteByte('}')
						}
						got, err := ParseUUIDLookupText(input.String())
						if err != nil || got != want {
							t.Fatalf("ParseUUIDLookupText(%q) = %q, %v; want %q, nil", input.String(), got, err, want)
						}
					})
				}
			}
		}
	}
}

func TestUUIDLookupInvalid(t *testing.T) {
	const digits = "a0eebc999c0b4ef8bb6d6bb9bd380a11"
	tests := []struct {
		name  string
		input string
	}{
		{"empty", ""},
		{"only opening brace", "{"},
		{"empty braces", "{}"},
		{"missing opening brace", digits + "}"},
		{"missing closing brace", "{" + digits},
		{"nested braces", "{{" + digits + "}}"},
		{"extra closing brace", "{" + digits + "}}"},
		{"parentheses", "(" + digits + ")"},
		{"URN prefix", "urn:uuid:" + digits},
		{"leading space", " " + digits},
		{"trailing space", digits + " "},
		{"leading tab", "\t" + digits},
		{"trailing newline", digits + "\n"},
		{"space inside braces", "{ " + digits + "}"},
		{"leading hyphen", "-" + digits},
		{"final hyphen", digits + "-"},
		{"hyphen before closing brace", "{" + digits + "-}"},
		{"too short", digits[:31]},
		{"extra hex", digits + "0"},
		{"extra pair", digits + "00"},
		{"nonhex lower", "g" + digits[1:]},
		{"nonhex upper", "G" + digits[1:]},
		{"plus sign", "+" + digits[1:]},
		{"nonASCII hex lookalike", "ａ" + digits[1:]},
		{"nonASCII whitespace", digits + "\u00a0"},
		{"NUL inside", digits[:8] + "\x00" + digits[9:]},
		{"NUL suffix", digits + "\x00"},
		{"invalid UTF8 inside", "\xff" + digits[1:]},
		{"invalid UTF8 suffix", digits + "\xff"},
		{"trailing suffix", digits + "xyz"},
		{"braced trailing suffix", "{" + digits + "}xyz"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertUUIDLookupInvalid(t, tt.input)
		})
	}
	// Reject a single hyphen at every boundary not allowed by PostgreSQL.
	for position := 1; position < len(digits); position++ {
		if position%4 == 0 {
			continue
		}
		t.Run(fmt.Sprintf("wrong hyphen position %d", position), func(t *testing.T) {
			assertUUIDLookupInvalid(t, digits[:position]+"-"+digits[position:])
		})
	}
	for position := 4; position < len(digits); position += 4 {
		t.Run(fmt.Sprintf("double hyphen position %d", position), func(t *testing.T) {
			assertUUIDLookupInvalid(t, digits[:position]+"--"+digits[position:])
		})
	}
	// Check both characters of every pair, including short input at each step.
	for position := 0; position < len(digits); position++ {
		t.Run(fmt.Sprintf("nonhex position %d", position), func(t *testing.T) {
			assertUUIDLookupInvalid(t, digits[:position]+"z"+digits[position+1:])
		})
		t.Run(fmt.Sprintf("truncated length %d", position), func(t *testing.T) {
			assertUUIDLookupInvalid(t, digits[:position])
		})
	}
}

func assertUUIDLookupInvalid(t *testing.T, input string) {
	t.Helper()
	got, err := ParseUUIDLookupText(input)
	if err == nil || got != "" {
		t.Fatalf("ParseUUIDLookupText(%q) = %q, %v; want empty output and error", input, got, err)
	}
}
