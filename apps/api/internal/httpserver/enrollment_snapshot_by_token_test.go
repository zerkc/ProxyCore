package httpserver_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/httpserver"
	"github.com/zerkc/ProxyCore/apps/api/internal/identity"
)

func TestEnrollmentSnapshotByTokenHappyPathReturnsExactBody(t *testing.T) {
	fixture := newSnapshotByTokenFixture(t, []byte("canonical-snapshot"))
	var verified atomic.Int32
	handler := httpserver.NewEnrollmentSnapshotByTokenHandler(httpserver.EnrollmentSnapshotByTokenHandlerOptions{
		Store: fixture.store, Identity: fixture.identity, Certificate: snapshotByTokenCertificate,
		ParseToken: httpserver.ParseHTTPEnrollmentToken,
		VerifyToken: func(_ context.Context, token string) error {
			if token != fixture.token {
				return errors.New("unexpected token")
			}
			verified.Add(1)
			return nil
		},
		ClusterKeyLoader: fixture.loadKey,
	})
	response := fixture.request(handler, http.MethodPost, bytes.NewBufferString(`{"token":"`+fixture.token+`"}`))
	if response.Code != http.StatusOK || !bytes.Equal(response.Body.Bytes(), fixture.body) {
		t.Fatalf("status=%d body=%q want=%q", response.Code, response.Body.Bytes(), fixture.body)
	}
	if verified.Load() != 1 || fixture.store.selector != fixture.selector {
		t.Fatalf("verified=%d selector=%q want %q", verified.Load(), fixture.store.selector, fixture.selector)
	}
	if response.Header().Get("Content-Type") != "application/octet-stream" ||
		response.Header().Get("Cache-Control") != "no-store" ||
		response.Header().Get("X-Content-Type-Options") != "nosniff" ||
		response.Header().Get("Location") != "" || response.Header().Get("Set-Cookie") != "" {
		t.Fatalf("unsafe response headers: %v", response.Header())
	}
}

func TestEnrollmentSnapshotByTokenNoPublishableSnapshotReturnsNoContent(t *testing.T) {
	fixture := newSnapshotByTokenFixture(t, []byte("unused"))
	fixture.store.read = func(context.Context, string, configuration.SnapshotPublicationIdentityRecord) ([]byte, error) {
		return nil, httpserver.ErrNoPublishableSnapshot
	}
	handler := newSnapshotByTokenHandler(fixture)
	response := fixture.request(handler, http.MethodPost, bytes.NewBufferString(`{"token":"`+fixture.token+`"}`))
	if response.Code != http.StatusNoContent || response.Body.Len() != 0 {
		t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
	}
}

func TestEnrollmentSnapshotByTokenStoreDeniedDoesNotBecomeReadiness(t *testing.T) {
	fixture := newSnapshotByTokenFixture(t, []byte("unused"))
	fixture.store.read = func(context.Context, string, configuration.SnapshotPublicationIdentityRecord) ([]byte, error) {
		return nil, httpserver.ErrSnapshotByTokenDenied
	}
	response := fixture.request(newSnapshotByTokenHandler(fixture), http.MethodPost, bytes.NewBufferString(`{"token":"`+fixture.token+`"}`))
	if response.Code != http.StatusForbidden || response.Body.String() != `{"error":"Permission denied"}` {
		t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
	}
}

func TestEnrollmentSnapshotByTokenStoreGoneMapsTo410(t *testing.T) {
	fixture := newSnapshotByTokenFixture(t, []byte("unused"))
	fixture.store.read = func(context.Context, string, configuration.SnapshotPublicationIdentityRecord) ([]byte, error) {
		return nil, httpserver.ErrSnapshotByTokenGone
	}
	response := fixture.request(newSnapshotByTokenHandler(fixture), http.MethodPost, bytes.NewBufferString(`{"token":"`+fixture.token+`"}`))
	if response.Code != http.StatusGone || response.Body.String() != `{"error":"Gone"}` {
		t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
	}
}

func TestEnrollmentSnapshotByTokenRejectsConsumedRevokedAndExpiredTokens(t *testing.T) {
	for _, name := range []string{"consumed", "revoked", "expired"} {
		t.Run(name, func(t *testing.T) {
			fixture := newSnapshotByTokenFixture(t, []byte("snapshot"))
			fixture.verifyErr = errors.New(name)
			handler := newSnapshotByTokenHandler(fixture)
			response := fixture.request(handler, http.MethodPost, bytes.NewBufferString(`{"token":"`+fixture.token+`"}`))
			if response.Code != http.StatusForbidden || response.Body.String() != `{"error":"Permission denied"}` {
				t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
			}
			assertSnapshotByTokenRedacted(t, response.Body.String(), fixture.token, fixture.key)
		})
	}
}

func TestEnrollmentSnapshotByTokenRejectsInvalidJSONAndTokenFormat(t *testing.T) {
	fixture := newSnapshotByTokenFixture(t, []byte("snapshot"))
	handler := newSnapshotByTokenHandler(fixture)
	for _, test := range []struct {
		name string
		body string
	}{
		{name: "invalid token", body: `{"token":"not-a-token"}`},
		{name: "unknown field", body: `{"token":"` + fixture.token + `","extra":true}`},
		{name: "trailing token", body: `{"token":"` + fixture.token + `"}{"token":"` + fixture.token + `"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := fixture.request(handler, http.MethodPost, bytes.NewBufferString(test.body))
			if response.Code != http.StatusBadRequest || response.Body.String() != `{"error":"Invalid request"}` {
				t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
			}
			assertSnapshotByTokenRedacted(t, response.Body.String(), fixture.token, fixture.key)
		})
	}
}

func TestEnrollmentSnapshotByTokenRejectsTLS12WrongSNIAndAuthority(t *testing.T) {
	fixture := newSnapshotByTokenFixture(t, []byte("snapshot"))
	handler := newSnapshotByTokenHandler(fixture)
	cases := []struct {
		name    string
		host    string
		server  string
		version uint16
	}{
		{name: "TLS 1.2", host: "primary.example:3443", server: "primary.example", version: tls.VersionTLS12},
		{name: "wrong SNI", host: "primary.example:3443", server: "wrong.example", version: tls.VersionTLS13},
		{name: "wrong authority", host: "wrong.example:3443", server: "wrong.example", version: tls.VersionTLS13},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, httpserver.EnrollmentSnapshotByTokenPath, bytes.NewBufferString(`{"token":"`+fixture.token+`"}`))
			request.Host = test.host
			request.TLS = &tls.ConnectionState{Version: test.version, HandshakeComplete: true, ServerName: test.server}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest || response.Header().Get("Location") != "" {
				t.Fatalf("status=%d location=%q", response.Code, response.Header().Get("Location"))
			}
		})
	}
}

func TestEnrollmentSnapshotByTokenRejectsIneligibleIdentityAndUnavailableKey(t *testing.T) {
	for _, role := range []domain.TopologyRole{domain.TopologyRoleNode, domain.TopologyRoleStandalone, domain.TopologyRoleStalePrimary} {
		t.Run(string(role), func(t *testing.T) {
			fixture := newSnapshotByTokenFixture(t, []byte("snapshot"))
			fixture.identity.current.Role = role
			response := fixture.request(newSnapshotByTokenHandler(fixture), http.MethodPost, bytes.NewBufferString(`{"token":"`+fixture.token+`"}`))
			if response.Code != http.StatusForbidden {
				t.Fatalf("role=%s status=%d want %d", role, response.Code, http.StatusForbidden)
			}
		})
	}
	fixture := newSnapshotByTokenFixture(t, []byte("snapshot"))
	fixture.keyErr = errors.New("key unavailable")
	response := fixture.request(newSnapshotByTokenHandler(fixture), http.MethodPost, bytes.NewBufferString(`{"token":"`+fixture.token+`"}`))
	if response.Code != http.StatusServiceUnavailable || response.Body.String() != `{"error":"Service unavailable"}` {
		t.Fatalf("key status=%d body=%q", response.Code, response.Body.String())
	}
}

func TestEnrollmentSnapshotByTokenMethodAndMuxBoundaries(t *testing.T) {
	fixture := newSnapshotByTokenFixture(t, []byte("snapshot"))
	handler := newSnapshotByTokenHandler(fixture)
	request := fixture.request(handler, http.MethodGet, nil)
	if request.Code != http.StatusMethodNotAllowed || request.Header().Get("Allow") != http.MethodPost {
		t.Fatalf("method status=%d allow=%q", request.Code, request.Header().Get("Allow"))
	}
	mux := httpserver.NewEnrollmentSnapshotByTokenMux(handler, http.NotFoundHandler())
	root := httptest.NewRecorder()
	mux.ServeHTTP(root, httptest.NewRequest(http.MethodGet, "/", nil))
	if root.Code != http.StatusNotFound || root.Header().Get("Location") != "" {
		t.Fatalf("root status=%d location=%q", root.Code, root.Header().Get("Location"))
	}
}

type snapshotByTokenStoreFunc struct {
	read     func(context.Context, string, configuration.SnapshotPublicationIdentityRecord) ([]byte, error)
	selector string
	identity configuration.SnapshotPublicationIdentityRecord
}

func (s *snapshotByTokenStoreFunc) ReadSnapshotPublicationByToken(ctx context.Context, selector string, current configuration.SnapshotPublicationIdentityRecord) ([]byte, error) {
	s.selector, s.identity = selector, current
	if s.read == nil {
		return nil, nil
	}
	return s.read(ctx, selector, current)
}

type snapshotByTokenIdentitySource struct {
	current identity.Identity
	loaded  bool
}

func (s *snapshotByTokenIdentitySource) Current() (identity.Identity, bool) {
	return s.current, s.loaded
}

type snapshotByTokenKey struct {
	key       []byte
	destroyed atomic.Bool
}

func (k *snapshotByTokenKey) CopyBytes() []byte { return append([]byte(nil), k.key...) }
func (k *snapshotByTokenKey) Destroy() {
	for i := range k.key {
		k.key[i] = 0
	}
	k.destroyed.Store(true)
}

type snapshotByTokenFixture struct {
	token     string
	selector  string
	body      []byte
	key       []byte
	store     *snapshotByTokenStoreFunc
	identity  *snapshotByTokenIdentitySource
	verifyErr error
	keyErr    error
}

func newSnapshotByTokenFixture(t *testing.T, body []byte) *snapshotByTokenFixture {
	t.Helper()
	token := "pcenr1_" + strings.Repeat("a", 32) + "_" + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x42}, 32))
	selector, _, ok := httpserver.ParseHTTPEnrollmentToken(token)
	if !ok {
		t.Fatal("test token did not parse")
	}
	keyID := uuid.MustParse("aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee")
	fixture := &snapshotByTokenFixture{
		token: token, selector: selector, body: body, key: bytes.Repeat([]byte{0x7a}, 32),
		identity: &snapshotByTokenIdentitySource{current: identity.Identity{
			InstallationID: domain.NewInstallationID(), NodeID: domain.NewNodeID(), Role: domain.TopologyRolePrimary,
			LeadershipGeneration: 4, LatestKnownGeneration: 4, ClusterKeyID: &keyID,
		}, loaded: true},
	}
	fixture.store = &snapshotByTokenStoreFunc{read: func(context.Context, string, configuration.SnapshotPublicationIdentityRecord) ([]byte, error) {
		return append([]byte(nil), body...), nil
	}}
	return fixture
}

func (f *snapshotByTokenFixture) loadKey(context.Context) (httpserver.SnapshotByTokenKeyMaterial, error) {
	if f.keyErr != nil {
		return nil, f.keyErr
	}
	return &snapshotByTokenKey{key: append([]byte(nil), f.key...)}, nil
}

func (f *snapshotByTokenFixture) request(handler http.Handler, method string, body *bytes.Buffer) *httptest.ResponseRecorder {
	if body == nil {
		body = bytes.NewBuffer(nil)
	}
	request := httptest.NewRequest(method, httpserver.EnrollmentSnapshotByTokenPath, body)
	request.Host = "primary.example:3443"
	request.TLS = &tls.ConnectionState{Version: tls.VersionTLS13, HandshakeComplete: true, ServerName: "primary.example"}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func newSnapshotByTokenHandler(fixture *snapshotByTokenFixture) *httpserver.EnrollmentSnapshotByTokenHandler {
	return httpserver.NewEnrollmentSnapshotByTokenHandler(httpserver.EnrollmentSnapshotByTokenHandlerOptions{
		Store: fixture.store, Identity: fixture.identity, Certificate: snapshotByTokenCertificate,
		ParseToken:       httpserver.ParseHTTPEnrollmentToken,
		VerifyToken:      func(_ context.Context, _ string) error { return fixture.verifyErr },
		ClusterKeyLoader: fixture.loadKey,
	})
}

func snapshotByTokenCertificate(context.Context) (*x509.Certificate, error) {
	return &x509.Certificate{DNSNames: []string{"primary.example"}}, nil
}

func assertSnapshotByTokenRedacted(t *testing.T, body string, token string, key []byte) {
	t.Helper()
	for _, forbidden := range []string{token, string(key), "pcnode1", "Authorization", "Set-Cookie"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("response leaked forbidden fragment (len=%d hash=%x): %q", len(forbidden), sha256.Sum256([]byte(forbidden)), redactBody(body))
		}
	}
}

func redactBody(value string) string {
	if len(value) <= 80 {
		return value
	}
	return value[:40] + "...[redacted " + itoa(len(value)-80) + " bytes]..." + value[len(value)-40:]
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	negative := n < 0
	if negative {
		n = -n
	}
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	if negative {
		digits = append([]byte{'-'}, digits...)
	}
	return string(digits)
}
