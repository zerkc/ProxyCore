package httpserver_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/acme"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/enrollment"
	"github.com/zerkc/ProxyCore/apps/api/internal/httpserver"
	"github.com/zerkc/ProxyCore/apps/api/internal/identity"
)

type proofHandlerFixture struct {
	request                    enrollment.IdentityProofRequest
	id                         identity.Identity
	now                        time.Time
	host                       string
	hostname                   string
	leafPEM, leafKeyPEM, caPEM string
	certificate                *x509.Certificate
	current                    identity.Identity
	loaded                     bool
	materialErr                error
	identityCalls              int
	handler                    http.Handler
}

func newProofHandlerFixture(t *testing.T, primaryURL string) *proofHandlerFixture {
	t.Helper()
	parsed, err := url.Parse(primaryURL)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := acme.CreateInternalCA(30)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := acme.IssueSignedByCA([]string{parsed.Hostname()}, 30, ca.CertificatePEM, ca.PrivateKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	certificate := &x509.Certificate{DNSNames: []string{parsed.Hostname()}}
	id := identity.Identity{
		InstallationID:        domain.NewInstallationID(),
		NodeID:                domain.NewNodeID(),
		Role:                  domain.TopologyRolePrimary,
		LeadershipGeneration:  4,
		LatestKnownGeneration: 4,
	}
	request, err := enrollment.NewIdentityProofRequest(primaryURL)
	if err != nil {
		t.Fatal(err)
	}
	f := &proofHandlerFixture{
		request: request, id: id, current: id, loaded: true,
		now: time.Now().UTC().Truncate(time.Second), host: parsed.Host,
		hostname: parsed.Hostname(), leafPEM: leaf.CertificatePEM,
		leafKeyPEM: leaf.PrivateKeyPEM, caPEM: ca.CertificatePEM, certificate: certificate,
	}
	signer, err := enrollment.NewIdentityProofSigner(enrollment.IdentityProofSignerOptions{
		Identity: func(context.Context) (identity.Identity, bool, error) {
			f.identityCalls++
			return f.current, f.loaded, nil
		},
		Material: func(context.Context) (string, string, string, error) {
			if f.materialErr != nil {
				return "", "", "", f.materialErr
			}
			return f.leafPEM, f.leafKeyPEM, f.caPEM, nil
		},
		Now: func() time.Time { return f.now },
	})
	if err != nil {
		t.Fatal(err)
	}
	f.handler = httpserver.NewEnrollmentIdentityMux(httpserver.EnrollmentIdentityHandlerOptions{
		Signer:      signer,
		Certificate: func(context.Context) (*x509.Certificate, error) { return f.certificate, nil },
	})
	return f
}

func (f *proofHandlerFixture) body(_ *testing.T) []byte {
	body, _ := json.Marshal(f.request)
	return body
}

func (f *proofHandlerFixture) serve(body []byte, mutate func(*http.Request)) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, httpserver.EnrollmentIdentityProofPath, bytes.NewReader(body))
	req.Host = f.host
	req.TLS = &tls.ConnectionState{Version: tls.VersionTLS13, HandshakeComplete: true, ServerName: f.hostname}
	if mutate != nil {
		mutate(req)
	}
	response := httptest.NewRecorder()
	f.handler.ServeHTTP(response, req)
	return response
}

func TestEnrollmentIdentityMuxDoesNotRedirectUnknownPaths(t *testing.T) {
	f := newProofHandlerFixture(t, "https://primary.example:3443/")
	for _, test := range []struct {
		name, method, path string
		status             int
	}{
		{"unknown", http.MethodGet, "/api/health", http.StatusNotFound},
		{"trailing slash", http.MethodPost, httpserver.EnrollmentIdentityProofPath + "/", http.StatusNotFound},
		{"query", http.MethodPost, httpserver.EnrollmentIdentityProofPath + "?redirect=/", http.StatusNotFound},
		{"get", http.MethodGet, httpserver.EnrollmentIdentityProofPath, http.StatusMethodNotAllowed},
		{"put", http.MethodPut, httpserver.EnrollmentIdentityProofPath, http.StatusMethodNotAllowed},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(test.method, test.path, nil)
			response := httptest.NewRecorder()
			f.handler.ServeHTTP(response, req)
			if response.Code != test.status {
				t.Fatalf("status = %d, want %d", response.Code, test.status)
			}
			if location := response.Header().Get("Location"); location != "" {
				t.Fatalf("unexpected redirect to %q", location)
			}
		})
	}
}

func TestEnrollmentIdentityProofSignsLiveIdentityAndReturnsOnlyNoStoreProof(t *testing.T) {
	f := newProofHandlerFixture(t, "https://primary.example:3443/")
	response := f.serve(f.body(t), func(req *http.Request) {
		req.Header.Set("X-Forwarded-Host", "attacker.example:443")
		req.Header.Set("X-Forwarded-Proto", "http")
	})
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("headers/status = %d %q %q %q", response.Code, response.Header().Get("Cache-Control"), response.Header().Get("Content-Type"), response.Body.String())
	}
	var proof enrollment.IdentityProof
	if err := json.Unmarshal(response.Body.Bytes(), &proof); err != nil {
		t.Fatal(err)
	}
	if proof.InstallationID != f.id.InstallationID.String() || proof.NodeID != f.id.NodeID.String() || proof.LeadershipGeneration != uint64(f.id.LeadershipGeneration) || proof.Signature == "" || f.identityCalls != 1 {
		t.Fatalf("proof/provider state = %#v calls=%d", proof, f.identityCalls)
	}
	if err := enrollment.VerifyIdentityProof(proof, enrollment.IdentityProofVerifierOptions{
		Request: f.request, ExpectedInstallationID: f.id.InstallationID, ExpectedNodeID: f.id.NodeID,
		ExpectedLeadershipGeneration: f.id.LeadershipGeneration, LeafCertificatePEM: f.leafPEM,
		CACertificatePEM: f.caPEM, Now: f.now,
	}); err != nil {
		t.Fatalf("proof verification: %v", err)
	}
	for _, forbidden := range []string{"private key", "secret", "token", "credential", "kek"} {
		if strings.Contains(strings.ToLower(response.Body.String()), forbidden) {
			t.Fatalf("response leaked %q: %s", forbidden, response.Body.String())
		}
	}
}

func TestEnrollmentIdentityProofRejectsMalformedBoundedOrNonCanonicalJSON(t *testing.T) {
	f := newProofHandlerFixture(t, "https://primary.example:3443/")
	valid := f.body(t)
	var fields map[string]any
	if err := json.Unmarshal(valid, &fields); err != nil {
		t.Fatal(err)
	}
	fields["unknown"] = "rejected"
	unknown, _ := json.Marshal(fields)
	canonical := f.request
	canonical.PrimaryURL = "HTTPS://primary.example:3443/"
	noncanonical, _ := json.Marshal(canonical)
	for _, test := range []struct {
		name string
		body []byte
	}{
		{"malformed", []byte("{")},
		{"unknown field", unknown},
		{"noncanonical URL", noncanonical},
		{"trailing JSON", append(valid, []byte("{}")...)},
		{"oversized", bytes.Repeat([]byte("x"), 9<<10)},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := f.serve(test.body, nil)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
			}
		})
	}
}

func TestEnrollmentIdentityProofOwnsUnknownLengthOversizedJSONError(t *testing.T) {
	f := newProofHandlerFixture(t, "https://primary.example:3443/")
	fields := map[string]any{}
	if err := json.Unmarshal(f.body(t), &fields); err != nil {
		t.Fatal(err)
	}
	fields["unknown"] = strings.Repeat("x", 9<<10)
	body, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	response := f.serve(body, func(req *http.Request) { req.ContentLength = -1 })
	if response.Code != http.StatusBadRequest || response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("status/headers = %d %q %q; body = %s", response.Code, response.Header().Get("Cache-Control"), response.Header().Get("Content-Type"), response.Body.String())
	}
	var errorBody struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &errorBody); err != nil || errorBody.Error != "invalid identity proof request" || f.identityCalls != 0 {
		t.Fatalf("unexpected error response or signer execution: %#v calls=%d body=%s", errorBody, f.identityCalls, response.Body.String())
	}
	for _, forbidden := range []string{"signature", "private key", "secret", "token", "credential", "kek", "413", "maxbytes"} {
		if strings.Contains(strings.ToLower(response.Body.String()), forbidden) {
			t.Fatalf("response leaked %q: %s", forbidden, response.Body.String())
		}
	}
}

func TestEnrollmentIdentityProofChecksTLSAuthoritySNIAndCertificateSAN(t *testing.T) {
	f := newProofHandlerFixture(t, "https://primary.example:3443/")
	otherCertificate := &x509.Certificate{DNSNames: []string{"other.example"}}
	for _, test := range []struct {
		name   string
		status int
		mutate func(*proofHandlerFixture, *http.Request)
	}{
		{"plain HTTP", http.StatusBadRequest, func(_ *proofHandlerFixture, req *http.Request) { req.TLS = nil }},
		{"TLS 1.2", http.StatusBadRequest, func(_ *proofHandlerFixture, req *http.Request) { req.TLS.Version = tls.VersionTLS12 }},
		{"Host mismatch", http.StatusMisdirectedRequest, func(f *proofHandlerFixture, req *http.Request) {
			req.Host = "other.example:3443"
			req.Header.Set("X-Forwarded-Host", f.host)
		}},
		{"DNS SNI absent", http.StatusMisdirectedRequest, func(_ *proofHandlerFixture, req *http.Request) { req.TLS.ServerName = "" }},
		{"DNS SNI mismatch", http.StatusMisdirectedRequest, func(_ *proofHandlerFixture, req *http.Request) { req.TLS.ServerName = "other.example" }},
		{"certificate SAN mismatch", http.StatusMisdirectedRequest, func(f *proofHandlerFixture, _ *http.Request) { f.certificate = otherCertificate }},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := f.serve(f.body(t), func(req *http.Request) { test.mutate(f, req) })
			if response.Code != test.status {
				t.Fatalf("status = %d, want %d; body = %s", response.Code, test.status, response.Body.String())
			}
		})
	}
}

func TestEnrollmentIdentityProofAllowsIPWithoutSNI(t *testing.T) {
	f := newProofHandlerFixture(t, "https://127.0.0.1:3443/")
	f.certificate = &x509.Certificate{IPAddresses: []net.IP{net.ParseIP(f.hostname)}}
	response := f.serve(f.body(t), func(req *http.Request) { req.TLS.ServerName = "" })
	if response.Code != http.StatusOK {
		t.Fatalf("IP without SNI status = %d; body = %s", response.Code, response.Body.String())
	}
	if response := f.serve(f.body(t), func(req *http.Request) { req.TLS.ServerName = "other.example" }); response.Code != http.StatusMisdirectedRequest {
		t.Fatalf("IP with wrong SNI status = %d", response.Code)
	}
}

func TestEnrollmentIdentityProofMapsSignerFailuresWithoutLeaks(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*proofHandlerFixture)
	}{
		{"unloaded", func(f *proofHandlerFixture) { f.loaded = false }},
		{"ineligible role", func(f *proofHandlerFixture) { f.current.Role = domain.TopologyRoleNode }},
		{"stale generation", func(f *proofHandlerFixture) { f.current.LatestKnownGeneration++ }},
		{"material failure", func(f *proofHandlerFixture) { f.materialErr = errors.New("leak-private-key-token-credential-kek") }},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newProofHandlerFixture(t, "https://primary.example:3443/")
			test.mutate(f)
			response := f.serve(f.body(t), nil)
			if response.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
			}
			body := strings.ToLower(response.Body.String())
			for _, forbidden := range []string{"private key", "secret", "token", "credential", "kek", "leak-"} {
				if strings.Contains(body, forbidden) {
					t.Fatalf("response leaked %q: %s", forbidden, body)
				}
			}
		})
	}
}
