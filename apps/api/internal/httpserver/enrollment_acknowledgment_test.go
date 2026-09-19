package httpserver_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/httpserver"
	"github.com/zerkc/ProxyCore/apps/api/internal/identity"
	syncpublication "github.com/zerkc/ProxyCore/apps/api/internal/sync"
)

func TestEnrollmentAcknowledgmentRejectsTLS12(t *testing.T) {
	handler := httpserver.NewEnrollmentSnapshotAcknowledgementHandler(httpserver.EnrollmentSnapshotAcknowledgementHandlerOptions{})
	request := httptest.NewRequest(http.MethodPost, httpserver.EnrollmentSnapshotAcknowledgementPath, nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want %d", response.Code, http.StatusBadRequest)
	}
}

func TestEnrollmentAcknowledgmentHappyPathUsesCanonicalURLAndSafeHeaders(t *testing.T) {
	fixture := newAcknowledgementFixture(t)
	called := false
	store := acknowledgementAuthorizerFunc(func(_ context.Context, request configuration.SnapshotAcknowledgementRequest) error {
		called = true
		if request.NodeID != fixture.nodeID || request.ContentHash != fixture.contentHash ||
			!request.AppliedAt.Equal(fixture.appliedAt) || request.CredentialID != fixture.credential.ID() {
			t.Fatalf("unexpected acknowledgement request: %+v", request)
		}
		if request.PresentedCredential != fixture.credential.BearerCopy() {
			t.Fatal("presented credential was not passed to the authorizer")
		}
		principal, err := request.Authenticate(request.PresentedCredential, fixture.credentialRecord())
		if err != nil || principal.NodeID != fixture.nodeID {
			return configuration.ErrSnapshotAcknowledgementDenied
		}
		return nil
	})
	handler := httpserver.NewEnrollmentSnapshotAcknowledgementHandler(httpserver.EnrollmentSnapshotAcknowledgementHandlerOptions{
		Authorizer: store, Identity: fixture.identity, Certificate: acknowledgementCertificate,
	})
	response := fixture.request(handler, "HTTPS://PRIMARY.EXAMPLE:03443/", fixture.credential.BearerCopy())
	if response.Code != http.StatusNoContent || response.Body.Len() != 0 || !called {
		t.Fatalf("status=%d bodyLen=%d bodyHash=%x called=%t", response.Code, response.Body.Len(), sha256.Sum256(response.Body.Bytes()), called)
	}
	if response.Header().Get("Cache-Control") != "no-store" ||
		response.Header().Get("X-Content-Type-Options") != "nosniff" ||
		response.Header().Get("Location") != "" || response.Header().Get("Set-Cookie") != "" {
		t.Fatalf("unsafe acknowledgement headers: %v", response.Header())
	}
}

func TestEnrollmentAcknowledgmentRejectsMalformedBodyAndURL(t *testing.T) {
	fixture := newAcknowledgementFixture(t)
	store := acknowledgementAuthorizerFunc(func(context.Context, configuration.SnapshotAcknowledgementRequest) error { return nil })
	handler := httpserver.NewEnrollmentSnapshotAcknowledgementHandler(httpserver.EnrollmentSnapshotAcknowledgementHandlerOptions{
		Authorizer: store, Identity: fixture.identity, Certificate: acknowledgementCertificate,
	})
	cases := []string{
		`{"nodeId":"` + fixture.nodeID + `","primaryUrl":"https://primary.example:3443/","contentHash":"` + fixture.contentHash + `","appliedAt":"` + fixture.appliedAt.Format(time.RFC3339Nano) + `","extra":true}`,
		`{"nodeId":"` + fixture.nodeID + `","primaryUrl":"https://primary.example:3443/","contentHash":"` + fixture.contentHash + `","appliedAt":"` + fixture.appliedAt.Format(time.RFC3339Nano) + `"}{}`,
		`{"nodeId":"` + fixture.nodeID + `","primaryUrl":"http://primary.example:3443/","contentHash":"` + fixture.contentHash + `","appliedAt":"` + fixture.appliedAt.Format(time.RFC3339Nano) + `"}`,
	}
	for _, body := range cases {
		response := fixture.requestBody(handler, body, fixture.credential.BearerCopy())
		if response.Code != http.StatusBadRequest {
			t.Fatalf("body=%s status=%d, want %d", body, response.Code, http.StatusBadRequest)
		}
	}
}

func TestEnrollmentAcknowledgmentMapsAuthLifecycleAndStoreErrors(t *testing.T) {
	fixture := newAcknowledgementFixture(t)
	cases := []struct {
		name       string
		bearer     string
		storeError error
		want       int
	}{
		{name: "missing bearer", want: http.StatusUnauthorized},
		{name: "malformed bearer", bearer: "pcnode1_invalid", want: http.StatusForbidden},
		{name: "revoked", bearer: fixture.credential.BearerCopy(), storeError: configuration.ErrSnapshotAcknowledgementRevoked, want: http.StatusGone},
		{name: "denied", bearer: fixture.credential.BearerCopy(), storeError: configuration.ErrSnapshotAcknowledgementDenied, want: http.StatusForbidden},
		{name: "unavailable", bearer: fixture.credential.BearerCopy(), storeError: configuration.ErrSnapshotAcknowledgementUnavailable, want: http.StatusServiceUnavailable},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			store := acknowledgementAuthorizerFunc(func(_ context.Context, request configuration.SnapshotAcknowledgementRequest) error {
				if test.storeError != nil {
					if errors.Is(test.storeError, configuration.ErrSnapshotAcknowledgementRevoked) {
						return test.storeError
					}
					if _, err := request.Authenticate(request.PresentedCredential, fixture.credentialRecord()); err != nil {
						return configuration.ErrSnapshotAcknowledgementDenied
					}
					return test.storeError
				}
				return nil
			})
			handler := httpserver.NewEnrollmentSnapshotAcknowledgementHandler(httpserver.EnrollmentSnapshotAcknowledgementHandlerOptions{
				Authorizer: store, Identity: fixture.identity, Certificate: acknowledgementCertificate,
			})
			response := fixture.request(handler, "https://primary.example:3443/", test.bearer)
			if response.Code != test.want {
				t.Fatalf("status=%d bodyLen=%d bodyHash=%x, want %d", response.Code, response.Body.Len(), sha256.Sum256(response.Body.Bytes()), test.want)
			}
			for _, secret := range []string{fixture.credential.BearerCopy(), "bootstrap", "cluster-kek", fixture.contentHash} {
				if strings.Contains(response.Body.String(), secret) {
					t.Fatalf("response leaked secret (len=%d hash=%x): %s", len(secret), sha256.Sum256([]byte(secret)), redactAcknowledgementBody(response.Body.String()))
				}
			}
		})
	}
}

func TestEnrollmentAcknowledgmentRejectsWrongNodeAndIneligibleIdentity(t *testing.T) {
	fixture := newAcknowledgementFixture(t)
	store := acknowledgementAuthorizerFunc(func(_ context.Context, request configuration.SnapshotAcknowledgementRequest) error {
		principal, err := request.Authenticate(request.PresentedCredential, fixture.credentialRecord())
		if err != nil || principal.NodeID != request.NodeID {
			return configuration.ErrSnapshotAcknowledgementDenied
		}
		return nil
	})
	handler := httpserver.NewEnrollmentSnapshotAcknowledgementHandler(httpserver.EnrollmentSnapshotAcknowledgementHandlerOptions{
		Authorizer: store, Identity: fixture.identity, Certificate: acknowledgementCertificate,
	})
	wrongNode := `{"nodeId":"11112222-3333-4444-8999-aabbccddeeff","primaryUrl":"https://primary.example:3443/","contentHash":"` + fixture.contentHash + `","appliedAt":"` + fixture.appliedAt.Format(time.RFC3339Nano) + `"}`
	if response := fixture.requestBody(handler, wrongNode, fixture.credential.BearerCopy()); response.Code != http.StatusForbidden {
		t.Fatalf("wrong node status=%d", response.Code)
	}
	for _, role := range []string{"node", "standalone-primary", "stale-primary"} {
		fixture.identity.current.Role = domainRole(role)
		if response := fixture.request(handler, "https://primary.example:3443/", fixture.credential.BearerCopy()); response.Code != http.StatusForbidden {
			t.Fatalf("role=%s status=%d", role, response.Code)
		}
	}
}

type acknowledgementAuthorizerFunc func(context.Context, configuration.SnapshotAcknowledgementRequest) error

func (f acknowledgementAuthorizerFunc) RecordSnapshotAcknowledgementForCredential(ctx context.Context, request configuration.SnapshotAcknowledgementRequest) error {
	return f(ctx, request)
}

type acknowledgementFixture struct {
	credential  syncpublication.NodeCredential
	identity    *publicationIdentity
	nodeID      string
	contentHash string
	appliedAt   time.Time
}

func newAcknowledgementFixture(t *testing.T) *acknowledgementFixture {
	t.Helper()
	credential, err := syncpublication.NewNodeCredential("11112222-3333-4444-8999-aabbccddeeff", bytes.Repeat([]byte{0x42}, syncpublication.NodeCredentialSecretBytes))
	if err != nil {
		t.Fatal(err)
	}
	primaryID := "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	keyID := uuidForAcknowledgement()
	fixture := &acknowledgementFixture{
		credential: credential, identity: &publicationIdentity{current: identityForAcknowledgement(primaryID, keyID)},
		nodeID:      "99990000-1111-4222-8333-444455556666",
		contentHash: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		appliedAt:   time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}
	t.Cleanup(func() { fixture.credential.Destroy() })
	return fixture
}

func (f *acknowledgementFixture) credentialRecord() configuration.SnapshotPublicationCredentialRecord {
	return configuration.SnapshotPublicationCredentialRecord{
		ID: f.credential.ID(), NodeID: f.nodeID, CredentialHash: f.credential.Hash(),
		HashVersion: syncpublication.NodeCredentialHashVersion,
		PrimaryID:   string(f.identity.current.InstallationID),
	}
}

func (f *acknowledgementFixture) request(handler http.Handler, primaryURL, bearer string) *httptest.ResponseRecorder {
	body := `{"nodeId":"` + f.nodeID + `","primaryUrl":"` + primaryURL + `","contentHash":"` + f.contentHash + `","appliedAt":"` + f.appliedAt.Format(time.RFC3339Nano) + `"}`
	return f.requestBody(handler, body, bearer)
}

func (f *acknowledgementFixture) requestBody(handler http.Handler, body, bearer string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, httpserver.EnrollmentSnapshotAcknowledgementPath, bytes.NewBufferString(body))
	request.Host = "primary.example:3443"
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	request.TLS = &tls.ConnectionState{Version: tls.VersionTLS13, HandshakeComplete: true, ServerName: "primary.example"}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func acknowledgementCertificate(context.Context) (*x509.Certificate, error) {
	return &x509.Certificate{DNSNames: []string{"primary.example"}}, nil
}

func domainRole(value string) domain.TopologyRole { return domain.TopologyRole(value) }

func redactAcknowledgementBody(value string) string {
	if len(value) <= 80 {
		return value
	}
	return value[:40] + "...[redacted " + acknowledgementBodyMidLen(len(value)-80) + " bytes]..." + value[len(value)-40:]
}

func acknowledgementBodyMidLen(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

func uuidForAcknowledgement() *uuid.UUID {
	id := uuid.MustParse("aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee")
	return &id
}

func identityForAcknowledgement(primaryID string, keyID *uuid.UUID) identity.Identity {
	return identity.Identity{
		InstallationID: domain.InstallationID(primaryID), NodeID: domain.NewNodeID(), Role: domain.TopologyRolePrimary,
		LeadershipGeneration: 4, LatestKnownGeneration: 4, ClusterKeyID: keyID,
	}
}
