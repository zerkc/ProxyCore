package apply_test

import (
	"encoding/json"
	"testing"

	"github.com/zerkc/ProxyCore/apps/api/internal/worker/apply"
)

func TestDesiredSnapshot(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"plain snapshot is unchanged", `{"zones":[]}`, `{"zones":[]}`},
		{"not an object is unchanged", `[1,2]`, `[1,2]`},
		{"envelope without configuration is unchanged", `{"replicated":{}}`, `{"replicated":{}}`},
		{"envelope unwraps to the configuration",
			`{"replicated":{"configuration":{"settings":{"ingress":{"ipv4":"203.0.113.1"}},"zones":[]}}}`,
			`{"settings":{"ingress":{"ipv4":"203.0.113.1"}},"zones":[]}`},
		{"node ipv4 replaces the whole ingress",
			`{"nodeLocal":{"ingress":{"ipv4":"198.51.100.77"}},"replicated":{"configuration":{"settings":{"ingress":{"ipv4":"203.0.113.1","ipv6":"2001:db8::1"},"defaultPool":{"id":"p"}},"zones":[]}}}`,
			`{"settings":{"ingress":{"ipv4":"198.51.100.77"},"defaultPool":{"id":"p"}},"zones":[]}`},
		{"node ipv6 only",
			`{"nodeLocal":{"ingress":{"ipv6":"2001:db8::7"}},"replicated":{"configuration":{"settings":{"ingress":{"ipv4":"203.0.113.1"}}}}}`,
			`{"settings":{"ingress":{"ipv6":"2001:db8::7"}}}`},
		{"empty node addresses do not override",
			`{"nodeLocal":{"ingress":{"ipv4":"","ipv6":5}},"replicated":{"configuration":{"settings":{"ingress":{"ipv4":"203.0.113.1"}}}}}`,
			`{"settings":{"ingress":{"ipv4":"203.0.113.1"}}}`},
		{"node ingress without settings leaves the configuration",
			`{"nodeLocal":{"ingress":{"ipv4":"198.51.100.77"}},"replicated":{"configuration":{"zones":[]}}}`,
			`{"zones":[]}`},
		{"numbers keep their exact text",
			`{"nodeLocal":{"ingress":{"ipv4":"198.51.100.77"}},"replicated":{"configuration":{"settings":{"retentionMaxSizeMb":50.0,"big":12345678901234567890}}}}`,
			`{"settings":{"retentionMaxSizeMb":50.0,"big":12345678901234567890,"ingress":{"ipv4":"198.51.100.77"}}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := json.RawMessage(tt.in)
			got := apply.DesiredSnapshot(input)
			if string(input) != tt.in {
				t.Fatal("the stored envelope must not be rewritten")
			}
			if tt.name == "numbers keep their exact text" {
				var object struct {
					Settings map[string]json.RawMessage `json:"settings"`
				}
				if err := json.Unmarshal(got, &object); err != nil ||
					string(object.Settings["retentionMaxSizeMb"]) != "50.0" || string(object.Settings["big"]) != "12345678901234567890" {
					t.Fatalf("got %s, %v", got, err)
				}
				return
			}
			equalJSON(t, string(got), tt.want)
		})
	}
}
