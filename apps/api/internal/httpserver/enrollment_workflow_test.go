package httpserver_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/enrollment"
	"github.com/zerkc/ProxyCore/apps/api/internal/httpserver"
	replicationsnapshot "github.com/zerkc/ProxyCore/apps/api/internal/snapshot"
)

func TestEnrollmentWorkflowDraftReturnsOnlyRedactedPreview(t *testing.T) {
	envelope := workflowTestEnvelope(t)
	cache := enrollment.NewDraftCache(enrollment.DraftCacheOptions{Now: func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) }})
	called := false
	handler := httpserver.NewEnrollmentIdentityMux(httpserver.EnrollmentIdentityHandlerOptions{
		Certificate: func(context.Context) (*x509.Certificate, error) {
			return &x509.Certificate{DNSNames: []string{"primary.example"}}, nil
		},
		Workflow: &httpserver.EnrollmentWorkflowHandlerOptions{
			Cache:       cache,
			VerifyToken: func(context.Context, string) error { return nil },
			FetchSnapshot: func(context.Context, string, string) (*replicationsnapshot.Envelope, error) {
				called = true
				return envelope, nil
			},
			Validator: snapshotValidatorFunc(func(context.Context, replicationsnapshot.Envelope) replicationsnapshot.ValidationResult {
				return replicationsnapshot.ValidationResult{OK: true}
			}),
			Identity: func(context.Context) (httpserver.EnrollmentLocalIdentity, error) {
				return httpserver.EnrollmentLocalIdentity{InstallationID: domain.NewInstallationID(), NodeID: domain.NewNodeID()}, nil
			},
			LocalIngress: func(context.Context) (domain.Ingress, error) {
				return domain.Ingress{IPv4: "192.0.2.7"}, nil
			},
		},
	})
	token := "pcenr1_0123456789abcdef0123456789abcdef_" + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x42}, 32))
	body, _ := json.Marshal(map[string]string{"token": token, "primaryURL": "https://primary.example:3443/"})
	req := httptest.NewRequest(http.MethodPost, httpserver.EnrollmentDraftPath, bytes.NewReader(body))
	req.Host = "primary.example:3443"
	req.TLS = &tls.ConnectionState{Version: tls.VersionTLS13, HandshakeComplete: true, ServerName: "primary.example"}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusOK || !called {
		t.Fatalf("draft status=%d called=%t body=%s", response.Code, called, response.Body.String())
	}
	for _, forbidden := range []string{"PRIVATE KEY", "ciphertext", "credential", "kek", "secret"} {
		if bytes.Contains(bytes.ToLower(response.Body.Bytes()), bytes.ToLower([]byte(forbidden))) {
			t.Fatalf("draft response leaked %q: %s", forbidden, response.Body.String())
		}
	}
}

type snapshotValidatorFunc func(context.Context, replicationsnapshot.Envelope) replicationsnapshot.ValidationResult

func (f snapshotValidatorFunc) Validate(ctx context.Context, envelope replicationsnapshot.Envelope) replicationsnapshot.ValidationResult {
	return f(ctx, envelope)
}

func TestEnrollmentWorkflowPreviewRecoveryAndTLSGuards(t *testing.T) {
	envelope := workflowTestEnvelope(t)
	cache := enrollment.NewDraftCache(enrollment.DraftCacheOptions{Now: func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) }})
	token := workflowTestToken()
	draft, err := cache.PutWithPrimaryURL("0123456789abcdef0123456789abcdef", "https://primary.example:3443/", envelope, enrollment.NodeLocalOverlay{NodeID: domain.NewNodeID(), Ingress: domain.Ingress{IPv4: "192.0.2.7"}, Role: domain.TopologyRoleNode})
	if err != nil {
		t.Fatal(err)
	}
	localNode := domain.NewNodeID()
	validatorCalls := 0
	handler := httpserver.NewEnrollmentIdentityMux(httpserver.EnrollmentIdentityHandlerOptions{
		Certificate: func(context.Context) (*x509.Certificate, error) {
			return &x509.Certificate{DNSNames: []string{"primary.example"}}, nil
		},
		Workflow: &httpserver.EnrollmentWorkflowHandlerOptions{
			Cache: cache, VerifyToken: func(context.Context, string) error { return nil },
			Validator: snapshotValidatorFunc(func(context.Context, replicationsnapshot.Envelope) replicationsnapshot.ValidationResult {
				validatorCalls++
				return replicationsnapshot.ValidationResult{OK: true}
			}),
			Identity: func(context.Context) (httpserver.EnrollmentLocalIdentity, error) {
				return httpserver.EnrollmentLocalIdentity{InstallationID: domain.NewInstallationID(), NodeID: localNode}, nil
			},
			LocalIngress: func(context.Context) (domain.Ingress, error) { return domain.Ingress{IPv4: "192.0.2.7"}, nil },
		},
	})
	previewBody, _ := json.Marshal(map[string]string{"draftId": draft.DraftID})
	previewRequest := httptest.NewRequest(http.MethodPost, httpserver.EnrollmentPreviewPath, bytes.NewReader(previewBody))
	previewRequest.Host = "primary.example:3443"
	previewRequest.TLS = &tls.ConnectionState{Version: tls.VersionTLS13, HandshakeComplete: true, ServerName: "primary.example"}
	preview := httptest.NewRecorder()
	handler.ServeHTTP(preview, previewRequest)
	if preview.Code != http.StatusOK || validatorCalls != 1 || strings.Contains(preview.Body.String(), "configuration") {
		t.Fatalf("preview status/calls/body=%d %d %s", preview.Code, validatorCalls, preview.Body.String())
	}

	recoverBody, _ := json.Marshal(map[string]string{"token": token})
	recoverRequest := httptest.NewRequest(http.MethodPost, httpserver.EnrollmentRecoverPath, bytes.NewReader(recoverBody))
	recoverRequest.Host = "primary.example:3443"
	recoverRequest.TLS = &tls.ConnectionState{Version: tls.VersionTLS13, HandshakeComplete: true, ServerName: "primary.example"}
	recovered := httptest.NewRecorder()
	handler.ServeHTTP(recovered, recoverRequest)
	if recovered.Code != http.StatusOK || strings.Contains(recovered.Body.String(), token) {
		t.Fatalf("recover status/body=%d %s", recovered.Code, recovered.Body.String())
	}

	wrongTLS := httptest.NewRequest(http.MethodPost, httpserver.EnrollmentPreviewPath, bytes.NewReader(previewBody))
	wrongTLS.Host = "primary.example:3443"
	wrongTLS.TLS = &tls.ConnectionState{Version: tls.VersionTLS12, HandshakeComplete: true, ServerName: "primary.example"}
	wrong := httptest.NewRecorder()
	handler.ServeHTTP(wrong, wrongTLS)
	if wrong.Code != http.StatusBadRequest || wrong.Header().Get("Location") != "" {
		t.Fatalf("TLS downgrade status/location=%d %q", wrong.Code, wrong.Header().Get("Location"))
	}
}

func workflowTestToken() string {
	return "pcenr1_0123456789abcdef0123456789abcdef_" + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x42}, 32))
}

func workflowTestEnvelope(t *testing.T) *replicationsnapshot.Envelope {
	t.Helper()
	envelope := &replicationsnapshot.Envelope{
		Transient: replicationsnapshot.TransientFields{
			SnapshotVersion:    domain.SnapshotVersionV1,
			ReplicationVersion: domain.ReplicationVersionV1,
			SourcePrimaryID:    uuid.New(),
			CapturedAt:         time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		},
		NodeLocal:  replicationsnapshot.NodeLocalFields{NodeID: domain.NewNodeID(), Role: domain.TopologyRolePrimary},
		Replicated: replicationsnapshot.ReplicatedFields{Configuration: map[string]any{"settings": map[string]any{}}},
	}
	hash, err := envelope.ExpectedHash()
	if err != nil {
		t.Fatal(err)
	}
	envelope.ContentHash = hash
	return envelope
}
