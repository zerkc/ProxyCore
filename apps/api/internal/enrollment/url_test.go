package enrollment

import (
	"errors"
	"strings"
	"testing"
)

func TestCanonicalizePrimaryURL(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "dns is lower case and one trailing dot is removed",
			in:   "HTTPS://Primary.Example.COM.:3443",
			want: "https://primary.example.com:3443/",
		},
		{
			name: "root path is retained",
			in:   "https://primary.example:3443/",
			want: "https://primary.example:3443/",
		},
		{
			name: "ipv4 literal is retained",
			in:   "https://192.0.2.10:3443/",
			want: "https://192.0.2.10:3443/",
		},
		{
			name: "ipv6 literal is canonicalized and retained",
			in:   "https://[2001:0DB8:0:0:0:0:0:1]:3443/",
			want: "https://[2001:db8::1]:3443/",
		},
		{
			name: "leading port zeroes are canonicalized",
			in:   "https://primary.example:03443/",
			want: "https://primary.example:3443/",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := CanonicalizePrimaryURL(tt.in)
			if err != nil {
				t.Fatalf("CanonicalizePrimaryURL() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("CanonicalizePrimaryURL() = %q, want %q", got, tt.want)
			}
			if again, err := CanonicalizePrimaryURL(got); err != nil || again != got {
				t.Fatalf("canonical URL is not idempotent: got %q, err %v", again, err)
			}
		})
	}
}

func TestCanonicalizePrimaryURLRejectsInvalidURLs(t *testing.T) {
	longLabel := strings.Repeat("a", 64)
	longHost := "https://" + strings.Join([]string{
		strings.Repeat("a", 63),
		strings.Repeat("b", 63),
		strings.Repeat("c", 63),
		strings.Repeat("d", 63),
	}, ".") + ":3443/"
	tests := []struct {
		name string
		in   string
	}{
		{name: "empty", in: ""},
		{name: "missing scheme", in: "primary.example:3443"},
		{name: "insecure scheme", in: "http://primary.example:3443/"},
		{name: "non-https scheme", in: "https+unix://primary.example:3443/"},
		{name: "opaque URL", in: "https:primary.example:3443"},
		{name: "missing host", in: "https://:3443/"},
		{name: "missing port", in: "https://primary.example/"},
		{name: "empty port", in: "https://primary.example:/"},
		{name: "non-numeric port", in: "https://primary.example:abc/"},
		{name: "zero port", in: "https://primary.example:0/"},
		{name: "port too large", in: "https://primary.example:65536/"},
		{name: "userinfo", in: "https://user:password@primary.example:3443/"},
		{name: "query", in: "https://primary.example:3443/?redirect=https://other/"},
		{name: "empty query", in: "https://primary.example:3443/?"},
		{name: "fragment", in: "https://primary.example:3443/#redirect"},
		{name: "empty fragment", in: "https://primary.example:3443/#"},
		{name: "non-root path", in: "https://primary.example:3443/api"},
		{name: "double root path", in: "https://primary.example:3443//"},
		{name: "escaped root path", in: "https://primary.example:3443/%2F"},
		{name: "redirect path", in: "https://primary.example:3443/redirect"},
		{name: "dns double trailing dot", in: "https://primary.example..:3443/"},
		{name: "dns empty label", in: "https://primary..example:3443/"},
		{name: "dns leading hyphen", in: "https://-primary.example:3443/"},
		{name: "dns trailing hyphen", in: "https://primary-.example:3443/"},
		{name: "dns underscore", in: "https://primary_node.example:3443/"},
		{name: "dns wildcard", in: "https://*.example:3443/"},
		{name: "malformed ipv4", in: "https://999.999.999.999:3443/"},
		{name: "numeric ipv4-shaped DNS", in: "https://192.0.2.999:3443/"},
		{name: "ipv6 zone", in: "https://[fe80::1%25eth0]:3443/"},
		{name: "unbracketed ipv6", in: "https://2001:db8::1:3443/"},
		{name: "bracketed ipv4", in: "https://[192.0.2.10]:3443/"},
		{name: "host escape", in: "https://primary%2eexample:3443/"},
		{name: "dns label too long", in: "https://" + longLabel + ":3443/"},
		{name: "leading whitespace", in: " https://primary.example:3443/"},
		{name: "trailing whitespace", in: "https://primary.example:3443/ "},
		{name: "control character", in: "https://primary.example:3443/\x00"},
		{name: "host too long", in: longHost},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := CanonicalizePrimaryURL(tt.in)
			if !errors.Is(err, ErrInvalidPrimaryURL) {
				t.Fatalf("CanonicalizePrimaryURL() error = %v, want ErrInvalidPrimaryURL", err)
			}
		})
	}
}
