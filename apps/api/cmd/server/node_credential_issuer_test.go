package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"crypto/tls"
	"crypto/x509"

	"github.com/zerkc/ProxyCore/apps/api/internal/enrollment"
	"github.com/zerkc/ProxyCore/apps/api/internal/httpserver"
	nodesync "github.com/zerkc/ProxyCore/apps/api/internal/sync"
)

func TestNodeCredentialIssuerProducesAuthenticatableDistinctMaterial(t *testing.T) {
	issuer := nodeCredentialIssuer{}
	first, err := issuer.IssueNodeCredential(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second, err := issuer.IssueNodeCredential(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == second.ID || bytes.Equal(first.Secret, second.Secret) || first.Hash == second.Hash {
		t.Fatal("issued credentials must be distinct")
	}
	if err := issuer.ValidateNodeCredential(first); err != nil {
		t.Fatalf("validate: %v", err)
	}
	if first.HashVersion != nodesync.NodeCredentialHashVersion || len(first.Secret) != nodesync.NodeCredentialSecretBytes {
		t.Fatalf("hash version/secret length = %q/%d", first.HashVersion, len(first.Secret))
	}
	tampered := first
	tampered.Hash = second.Hash
	if issuer.ValidateNodeCredential(tampered) == nil {
		t.Fatal("tampered hash must be rejected")
	}
	if rendered := fmt.Sprintf("%v %+v %s", first, first, first); strings.Contains(rendered, string(first.Secret)) {
		t.Fatal("material formatting leaked the secret")
	}
	if _, err := issuer.IssueNodeCredential(canceledContext()); err == nil {
		t.Fatal("canceled context must not issue")
	}
}

func canceledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func TestGrantExchangeWithoutStoreAnswersUniformDenial(t *testing.T) {
	service := enrollment.NewPrimaryService(nil, enrollment.PrimaryServiceOptions{CredentialIssuer: nodeCredentialIssuer{}})
	handler := httpserver.NewEnrollmentGrantExchangeMux(httpserver.NewEnrollmentGrantExchangeHandler(httpserver.EnrollmentGrantExchangeHandlerOptions{
		Issuer: service,
		Certificate: func(context.Context) (*x509.Certificate, error) {
			return &x509.Certificate{DNSNames: []string{"primary.example"}}, nil
		},
	}), nil)
	token := "pcenr1_" + strings.Repeat("a1", 16) + "_" + strings.Repeat("A", 43)
	request := httptest.NewRequest(http.MethodPost, httpserver.EnrollmentGrantExchangePath, strings.NewReader("{}"))
	request.Host = "primary.example:3443"
	request.TLS = &tls.ConnectionState{Version: tls.VersionTLS13, HandshakeComplete: true, ServerName: "primary.example"}
	request.Header.Set("Authorization", "Enrollment "+token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized || response.Body.String() != `{"error":"ENROLLMENT_DENIED"}` {
		t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
	}
}
