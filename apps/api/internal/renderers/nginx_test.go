package renderers

import (
	"regexp"
	"strings"
	"testing"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
)

func renderNginxFixture(t *testing.T, name string) NginxCandidate {
	t.Helper()
	var input NginxRenderInput
	loadFixture(t, name, &input)
	candidate, err := RenderNginxCandidate(input)
	if err != nil {
		t.Fatalf("RenderNginxCandidate(%s): %v", name, err)
	}
	return candidate
}

func assertContains(t *testing.T, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Errorf("expected output to contain %q\n%s", needle, haystack)
	}
}

func assertNotContains(t *testing.T, haystack, needle string) {
	t.Helper()
	if strings.Contains(haystack, needle) {
		t.Errorf("expected output not to contain %q\n%s", needle, haystack)
	}
}

func TestNginxRendersTLSProxyServersTypedPathsHeadersAndStreams(t *testing.T) {
	candidate := renderNginxFixture(t, "nginx_tls")

	for _, want := range []string{
		"server_name api.home.arpa;",
		"proxy_pass http://10.0.0.20:8080;",
		"location = /health",
		"return 307 /ready;",
		"proxy_pass http://127.0.0.1:3000/api/acme-challenge/;",
		"proxy_set_header x-homelab proxycore;",
		"client_max_body_size 100m;",
		`add_header X-Robots-Tag "noindex" always;`,
		"listen 0.0.0.0:5432;",
		"return 308 https://$host$request_uri;",
	} {
		assertContains(t, candidate.Config, want)
	}
	assertNotContains(t, candidate.Config, "client_max_body_size 10m;")
	assertNotContains(t, candidate.Config, "password")
	if candidate.Files["nginx.conf"] != candidate.Config {
		t.Error("files[nginx.conf] must equal config")
	}
	if len(candidate.Checksum) != 64 {
		t.Errorf("checksum length = %d, want 64", len(candidate.Checksum))
	}
	assertGolden(t, "nginx_tls", candidate)
}

func TestNginxRendersBasicAuthFromCandidateFilesAndAvoidsDuplicateRootLocations(t *testing.T) {
	candidate := renderNginxFixture(t, "nginx_basic_auth")

	for _, want := range []string{
		"auth_basic_user_file /var/lib/proxycore/candidates/rev-2/nginx/basic-auth/secret-basic-1;",
		"ssl_certificate /var/lib/proxycore/candidates/rev-2/nginx/certs/cert-private.crt;",
		"proxy_pass https://10.0.0.40:3000/app;",
	} {
		assertContains(t, candidate.Config, want)
	}
	rootLocations := regexp.MustCompile(`location /\s*\{`).FindAllString(candidate.Config, -1)
	if len(rootLocations) != 1 {
		t.Errorf("root location count = %d, want 1", len(rootLocations))
	}
	if got := candidate.Files["basic-auth/secret-basic-1"]; got != "operator:{SHA}abc\n" {
		t.Errorf("basic-auth file = %q", got)
	}
	assertContains(t, candidate.Files["certs/cert-private.crt"], "BEGIN CERTIFICATE")
	assertNotContains(t, candidate.Config, "homelab-pass")
	assertGolden(t, "nginx_basic_auth", candidate)
}

func TestNginxRejectsTLSServerWithoutCertificateReference(t *testing.T) {
	var input NginxRenderInput
	loadFixture(t, "nginx_tls", &input)
	input.Records[0].Proxy = map[string]any{
		"origin":     map[string]any{"ip": "10.0.0.20", "port": float64(8080), "protocol": "http"},
		"tlsEnabled": true,
	}
	input.Streams = nil

	_, err := RenderNginxCandidate(input)
	if err == nil || !regexp.MustCompile(`(?i)certificate`).MatchString(err.Error()) {
		t.Fatalf("expected a certificate error, got %v", err)
	}
}

func TestNginxGoldenParityWithTypeScriptOutput(t *testing.T) {
	// These fixtures widen coverage beyond the original TS tests: HTTP/3, cache,
	// IPv6 origins and streams, redirect-less TLS, header quoting, directive
	// overrides, ACME upstream normalization and disabled records/streams.
	for _, name := range []string{"nginx_full", "nginx_default_paths"} {
		t.Run(name, func(t *testing.T) {
			assertGolden(t, name, renderNginxFixture(t, name))
		})
	}
}

func TestNginxErrors(t *testing.T) {
	base := func(t *testing.T) NginxRenderInput {
		var input NginxRenderInput
		loadFixture(t, "nginx_basic_auth", &input)
		return input
	}
	tests := []struct {
		name    string
		mutate  func(*NginxRenderInput)
		wantErr string
	}{
		{"proxied record without settings", func(i *NginxRenderInput) { i.Records[0].Proxy = nil }, "has no proxy settings"},
		{"invalid ACME upstream", func(i *NginxRenderInput) { i.ACMEUpstream = "ftp://nope" }, "Invalid NGINX ACME upstream: ftp://nope"},
		{"http3 without capabilities", func(i *NginxRenderInput) { i.Records[0].Proxy["http3"] = true }, "HTTP/3 requires binary support"},
		{"stream conflict", func(i *NginxRenderInput) {
			route := domain.StreamRoute{ID: "s", Enabled: true, Protocol: "tcp", ListenAddress: "0.0.0.0", ListenPort: 22,
				Upstream: domain.UpstreamTarget{IP: "10.0.0.2", Port: 22, Protocol: "tcp"}}
			i.Streams = []domain.StreamRoute{route, route}
		}, "Stream listener conflict"},
		{"invalid proxy settings from domain", func(i *NginxRenderInput) {
			i.Records[0].Proxy["origin"] = map[string]any{"ip": "not-an-ip", "port": float64(80), "protocol": "http"}
		}, "Proxy origin must be a literal IP"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := base(t)
			tt.mutate(&input)
			_, err := RenderNginxCandidate(input)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestNginxChecksumIsStableAndSensitiveToExtraFiles(t *testing.T) {
	var input NginxRenderInput
	loadFixture(t, "nginx_basic_auth", &input)
	first, err := RenderNginxCandidate(input)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := RenderNginxCandidate(input)
	if first.Checksum != again.Checksum {
		t.Error("checksum must be deterministic")
	}
	input.ExtraFiles["certs/cert-private.crt"] += "x"
	changed, _ := RenderNginxCandidate(input)
	if first.Checksum == changed.Checksum {
		t.Error("checksum must change when an extra file changes")
	}
}
