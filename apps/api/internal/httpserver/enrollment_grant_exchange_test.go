package httpserver_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/enrollment"
	"github.com/zerkc/ProxyCore/apps/api/internal/httpserver"
)

const (
	exchangeAttempt      = "11112222-3333-4444-8999-aabbccddeeff"
	exchangePrimary      = "22223333-4444-4555-8999-aabbccddeeff"
	exchangeInstallation = "33334444-5555-4666-8999-aabbccddeeff"
	exchangeNode         = "44445555-6666-4777-8999-aabbccddeeff"
	exchangeCredential   = "55556666-7777-4888-8999-aabbccddeeff"
	exchangeKeyID        = "77778888-9999-4aaa-8bbb-ccccddddeeee"
)

// sealingIssuer mimics PrimaryService semantics: the first request seals a
// grant for its binding, an exact retry returns the same bytes and every
// other binding is denied.
type sealingIssuer struct {
	calls      int
	token      string
	attempt    string
	granted    enrollment.PrimaryGrantResponse
	denyAlways bool
	lastToken  string
}

func (s *sealingIssuer) IssueGrant(_ context.Context, request enrollment.PrimaryGrantRequest) (enrollment.PrimaryGrantResponse, error) {
	s.calls++
	s.lastToken = request.EnrollmentToken
	if s.denyAlways || request.EnrollmentToken != s.token {
		return enrollment.PrimaryGrantResponse{}, enrollment.ErrEnrollmentDenied
	}
	if s.attempt != "" {
		if request.AttemptID != s.attempt {
			return enrollment.PrimaryGrantResponse{}, enrollment.ErrEnrollmentDenied
		}
		return s.granted, nil
	}
	binding := enrollment.BootstrapBinding{
		ProtocolVersion:             enrollment.BootstrapProtocolVersion,
		AttemptID:                   request.AttemptID,
		SourcePrimaryInstallationID: request.ExpectedPrimaryInstallationID,
		TargetInstallationID:        request.TargetInstallationID,
		TargetNodeID:                request.TargetNodeID,
		LeadershipGeneration:        request.ExpectedLeadershipGeneration,
		VerifiedPreviewDigest:       request.VerifiedPreviewDigest,
	}
	grant, err := enrollment.NewBootstrapGrant(enrollment.BootstrapGrantMetadata{
		ProtocolVersion:             binding.ProtocolVersion,
		CredentialID:                exchangeCredential,
		ClusterKeyID:                exchangeKeyID,
		SourcePrimaryInstallationID: binding.SourcePrimaryInstallationID,
		TargetInstallationID:        binding.TargetInstallationID,
		TargetNodeID:                binding.TargetNodeID,
		AttemptID:                   binding.AttemptID,
		LeadershipGeneration:        binding.LeadershipGeneration,
		ExpiresAtUnix:               time.Now().Add(time.Hour).Unix(),
	}, bytes.Repeat([]byte{0x6d}, 32), bytes.Repeat([]byte{0x5c}, 32))
	if err != nil {
		return enrollment.PrimaryGrantResponse{}, err
	}
	defer grant.Destroy()
	sealed, err := enrollment.SealBootstrap(request.RecipientPublicKey, binding, grant)
	if err != nil {
		return enrollment.PrimaryGrantResponse{}, err
	}
	s.attempt = request.AttemptID
	s.granted = enrollment.PrimaryGrantResponse{
		SealedBootstrapPayload: sealed, PayloadHash: "abc", ExpiresAt: time.Now().Add(time.Hour).UTC(),
	}
	return s.granted, nil
}

type exchangeFixture struct {
	t         *testing.T
	token     string
	issuer    *sealingIssuer
	handler   http.Handler
	recipient enrollment.BootstrapRecipient
	body      map[string]any
	digest    []byte
}

func newExchangeFixture(t *testing.T, opts httpserver.EnrollmentGrantExchangeHandlerOptions) *exchangeFixture {
	t.Helper()
	token := "pcenr1_" + strings.Repeat("a1", 16) + "_" + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x42}, 32))
	recipient, err := enrollment.NewBootstrapRecipient()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(recipient.Destroy)
	public, err := recipient.PublicKey()
	if err != nil {
		t.Fatal(err)
	}
	wire, err := public.Wire()
	if err != nil {
		t.Fatal(err)
	}
	digest := bytes.Repeat([]byte{0x29}, 32)
	issuer := &sealingIssuer{token: token}
	opts.Issuer = issuer
	opts.Certificate = snapshotByTokenCertificate
	return &exchangeFixture{
		t: t, token: token, issuer: issuer, recipient: recipient, digest: digest,
		handler: httpserver.NewEnrollmentGrantExchangeMux(httpserver.NewEnrollmentGrantExchangeHandler(opts), nil),
		body: map[string]any{
			"attemptId": exchangeAttempt, "installationId": exchangeInstallation, "nodeId": exchangeNode,
			"recipientPublicKey": json.RawMessage(wire), "previewDigest": hex.EncodeToString(digest),
			"primaryInstallationId": exchangePrimary, "leadershipGeneration": 7,
		},
	}
}

func (f *exchangeFixture) do(method, auth string, raw []byte) *httptest.ResponseRecorder {
	if raw == nil {
		raw, _ = json.Marshal(f.body)
	}
	request := httptest.NewRequest(method, httpserver.EnrollmentGrantExchangePath, bytes.NewReader(raw))
	request.Host = "primary.example:3443"
	request.RemoteAddr = "192.0.2.10:5555"
	request.TLS = tlsState()
	if auth != "" {
		request.Header.Set("Authorization", auth)
	}
	response := httptest.NewRecorder()
	f.handler.ServeHTTP(response, request)
	return response
}

func assertUniformDenied(t *testing.T, response *httptest.ResponseRecorder, token string) {
	t.Helper()
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", response.Code)
	}
	if got := response.Body.String(); got != `{"error":"ENROLLMENT_DENIED"}` {
		t.Fatalf("body = %q", got)
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q", response.Header().Get("Cache-Control"))
	}
	if strings.Contains(response.Body.String(), token) {
		t.Fatal("response leaked the token")
	}
}

func TestGrantExchangeIssuesSealedPayloadAndExactRetryIsIdempotent(t *testing.T) {
	f := newExchangeFixture(t, httpserver.EnrollmentGrantExchangeHandlerOptions{})
	first := f.do(http.MethodPost, "Enrollment "+f.token, nil)
	if first.Code != http.StatusCreated || first.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status=%d cache=%q", first.Code, first.Header().Get("Cache-Control"))
	}
	var decoded struct {
		Sealed      []byte    `json:"sealedBootstrapPayload"`
		PayloadHash string    `json:"payloadHash"`
		ExpiresAt   time.Time `json:"expiresAt"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &decoded); err != nil || len(decoded.Sealed) == 0 || decoded.PayloadHash == "" {
		t.Fatalf("decode: %v %s", err, first.Body.String())
	}
	binding := enrollment.BootstrapBinding{
		ProtocolVersion: enrollment.BootstrapProtocolVersion, AttemptID: exchangeAttempt,
		SourcePrimaryInstallationID: exchangePrimary, TargetInstallationID: exchangeInstallation,
		TargetNodeID: exchangeNode, LeadershipGeneration: 7, VerifiedPreviewDigest: f.digest,
	}
	grant, err := f.recipient.OpenBootstrap(binding, decoded.Sealed)
	if err != nil {
		t.Fatalf("OpenBootstrap: %v", err)
	}
	grant.Destroy()
	second := f.do(http.MethodPost, "Enrollment "+f.token, nil)
	if second.Code != http.StatusCreated || second.Body.String() != first.Body.String() {
		t.Fatalf("retry status=%d identical=%v", second.Code, second.Body.String() == first.Body.String())
	}
}

func TestGrantExchangeDeniesEveryFailureUniformly(t *testing.T) {
	f := newExchangeFixture(t, httpserver.EnrollmentGrantExchangeHandlerOptions{})
	good, _ := json.Marshal(f.body)
	unknown := map[string]any{}
	for k, v := range f.body {
		unknown[k] = v
	}
	unknown["extra"] = true
	unknownRaw, _ := json.Marshal(unknown)
	otherToken := "pcenr1_" + strings.Repeat("b2", 16) + "_" + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x43}, 32))
	cases := map[string]struct {
		auth string
		raw  []byte
	}{
		"missing header":  {"", good},
		"bearer scheme":   {"Bearer " + f.token, good},
		"lowercase":       {"enrollment " + f.token, good},
		"malformed token": {"Enrollment nope", good},
		"unknown token":   {"Enrollment " + otherToken, good},
		"malformed body":  {"Enrollment " + f.token, []byte(`{`)},
		"unknown field":   {"Enrollment " + f.token, unknownRaw},
		"trailing data":   {"Enrollment " + f.token, append(append([]byte(nil), good...), []byte(`{}`)...)},
		"oversized":       {"Enrollment " + f.token, bytes.Repeat([]byte("a"), 16<<10)},
		"bad digest":      {"Enrollment " + f.token, []byte(strings.Replace(string(good), f.body["previewDigest"].(string), "zz", 1))},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			assertUniformDenied(t, f.do(http.MethodPost, tc.auth, tc.raw), f.token)
		})
	}
	if f.issuer.calls != 1 {
		// unknown token reaches the issuer once; bad shapes never do.
		t.Fatalf("issuer calls = %d", f.issuer.calls)
	}
}

func TestGrantExchangeOtherBindingAndUnavailableIssuerAreDenied(t *testing.T) {
	f := newExchangeFixture(t, httpserver.EnrollmentGrantExchangeHandlerOptions{})
	if r := f.do(http.MethodPost, "Enrollment "+f.token, nil); r.Code != http.StatusCreated {
		t.Fatalf("first status = %d", r.Code)
	}
	f.body["attemptId"] = "99998888-7777-4666-8555-444433332222"
	assertUniformDenied(t, f.do(http.MethodPost, "Enrollment "+f.token, nil), f.token)

	f.issuer.denyAlways = true
	assertUniformDenied(t, f.do(http.MethodPost, "Enrollment "+f.token, nil), f.token)

	unavailable := httpserver.NewEnrollmentGrantExchangeMux(httpserver.NewEnrollmentGrantExchangeHandler(
		httpserver.EnrollmentGrantExchangeHandlerOptions{Certificate: snapshotByTokenCertificate}), nil)
	request := httptest.NewRequest(http.MethodPost, httpserver.EnrollmentGrantExchangePath, strings.NewReader("{}"))
	request.Host, request.TLS = "primary.example:3443", tlsState()
	request.Header.Set("Authorization", "Enrollment "+f.token)
	response := httptest.NewRecorder()
	unavailable.ServeHTTP(response, request)
	assertUniformDenied(t, response, f.token)

	failing := httpserver.NewEnrollmentGrantExchangeHandler(httpserver.EnrollmentGrantExchangeHandlerOptions{
		Issuer: failingIssuer{}, Certificate: snapshotByTokenCertificate})
	response = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, httpserver.EnrollmentGrantExchangePath, strings.NewReader("{}"))
	request.Host, request.TLS = "primary.example:3443", tlsState()
	request.Header.Set("Authorization", "Enrollment "+f.token)
	failing.ServeHTTP(response, request)
	assertUniformDenied(t, response, f.token)
}

type failingIssuer struct{}

func (failingIssuer) IssueGrant(context.Context, enrollment.PrimaryGrantRequest) (enrollment.PrimaryGrantResponse, error) {
	return enrollment.PrimaryGrantResponse{}, errors.New("sqlite: database is locked token=secret")
}

func TestGrantExchangeRejectsMethodsAndInsecureTransport(t *testing.T) {
	f := newExchangeFixture(t, httpserver.EnrollmentGrantExchangeHandlerOptions{})
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		response := f.do(method, "Enrollment "+f.token, nil)
		if response.Code != http.StatusMethodNotAllowed || response.Header().Get("Cache-Control") != "no-store" ||
			response.Header().Get("Allow") != http.MethodPost {
			t.Fatalf("%s: status=%d", method, response.Code)
		}
	}
	raw, _ := json.Marshal(f.body)
	request := httptest.NewRequest(http.MethodPost, httpserver.EnrollmentGrantExchangePath, bytes.NewReader(raw))
	request.Host = "primary.example:3443"
	request.Header.Set("Authorization", "Enrollment "+f.token)
	response := httptest.NewRecorder()
	f.handler.ServeHTTP(response, request)
	assertUniformDenied(t, response, f.token)
	if f.issuer.calls != 0 {
		t.Fatalf("issuer reached without TLS or with wrong method: %d", f.issuer.calls)
	}
}

func TestGrantExchangeLimiterBlocksPerIPAndPerSelector(t *testing.T) {
	now := time.Date(2026, 10, 2, 3, 4, 5, 0, time.UTC)
	f := newExchangeFixture(t, httpserver.EnrollmentGrantExchangeHandlerOptions{
		Now: func() time.Time { return now }, Window: time.Minute, IPLimit: 3, SelectorLimit: 100,
	})
	for i := 0; i < 3; i++ {
		if r := f.do(http.MethodPost, "Enrollment "+f.token, nil); r.Code != http.StatusCreated {
			t.Fatalf("attempt %d status = %d", i, r.Code)
		}
	}
	blocked := f.do(http.MethodPost, "Enrollment "+f.token, nil)
	if blocked.Code != http.StatusTooManyRequests || blocked.Header().Get("Cache-Control") != "no-store" ||
		strings.Contains(blocked.Body.String(), f.token) {
		t.Fatalf("blocked status = %d", blocked.Code)
	}
	calls := f.issuer.calls
	now = now.Add(2 * time.Minute)
	if r := f.do(http.MethodPost, "Enrollment "+f.token, nil); r.Code != http.StatusCreated {
		t.Fatalf("after window status = %d", r.Code)
	}
	if f.issuer.calls != calls+1 {
		t.Fatalf("blocked attempt reached issuer")
	}

	sel := newExchangeFixture(t, httpserver.EnrollmentGrantExchangeHandlerOptions{
		Now: func() time.Time { return now }, IPLimit: 100, SelectorLimit: 2,
	})
	var last int
	for i := 0; i < 3; i++ {
		last = sel.do(http.MethodPost, "Enrollment "+sel.token, nil).Code
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("selector limit status = %d", last)
	}
}

func tlsState() *tls.ConnectionState {
	return &tls.ConnectionState{Version: tls.VersionTLS13, HandshakeComplete: true, ServerName: "primary.example"}
}
