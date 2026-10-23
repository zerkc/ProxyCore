package httpserver_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/cluster"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/enrollment"
	"github.com/zerkc/ProxyCore/apps/api/internal/httpserver"
	"github.com/zerkc/ProxyCore/apps/api/internal/identity"
	replicationsnapshot "github.com/zerkc/ProxyCore/apps/api/internal/snapshot"
)

// apply-wait stand-in: blocks until released, fails like WaitForApply when its
// context is cancelled, and only "commits" when it ran to completion.
type blockingConverter struct {
	started   chan struct{}
	release   chan struct{}
	committed chan bool
}

func (c *blockingConverter) Convert(ctx context.Context, in enrollment.NodeConversionInput) (enrollment.NodeConversionResult, error) {
	close(c.started)
	select {
	case <-c.release:
	case <-ctx.Done():
		c.committed <- false
		return enrollment.NodeConversionResult{}, ctx.Err()
	}
	c.committed <- true
	return enrollment.NodeConversionResult{
		ArchiveID: uuid.New(), ApplyJobID: uuid.New(),
		Identity: identity.Identity{NodeID: in.LocalNodeID, Role: domain.TopologyRoleNode, LeadershipGeneration: 1},
	}, nil
}

func TestConfirmConversionSurvivesClientAbortDuringApplyWait(t *testing.T) {
	f := newConfirmFixture(t, true)
	converter := &blockingConverter{started: make(chan struct{}), release: make(chan struct{}), committed: make(chan bool, 1)}
	handler := httpserver.NewEnrollmentIdentityMux(httpserver.EnrollmentIdentityHandlerOptions{
		Certificate: func(context.Context) (*x509.Certificate, error) {
			return &x509.Certificate{DNSNames: []string{"primary.example"}}, nil
		},
		Workflow: &httpserver.EnrollmentWorkflowHandlerOptions{
			Cache:       f.cache,
			VerifyToken: func(context.Context, string) error { return nil },
			Validator: kekValidatorFunc(func(context.Context, replicationsnapshot.Envelope, *cluster.KEK) replicationsnapshot.ValidationResult {
				return replicationsnapshot.ValidationResult{OK: true}
			}),
			Identity: func(context.Context) (httpserver.EnrollmentLocalIdentity, error) {
				return httpserver.EnrollmentLocalIdentity{InstallationID: f.localInstall, NodeID: f.localNode, Role: f.localRole}, nil
			},
			LocalIngress:   func(context.Context) (domain.Ingress, error) { return domain.Ingress{IPv4: "192.0.2.7"}, nil },
			Converter:      converter,
			ExchangeGrant:  f.exchange,
			AcceptGrant:    f.accept,
			LoadClusterKEK: f.loadKEK,
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	body, _ := json.Marshal(map[string]string{"draftId": f.draftID})
	req := httptest.NewRequest(http.MethodPost, httpserver.EnrollmentConfirmPath, bytes.NewReader(body)).WithContext(ctx)
	req.Host = "primary.example:3443"
	req.TLS = &tls.ConnectionState{Version: tls.VersionTLS13, HandshakeComplete: true, ServerName: "primary.example"}
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { handler.ServeHTTP(rec, req); close(done) }()

	select {
	case <-converter.started:
	case <-time.After(5 * time.Second):
		t.Fatal("conversion never started")
	}
	cancel() // the browser aborted the fetch mid apply-wait
	select {
	case ok := <-converter.committed:
		t.Fatalf("conversion ended after client abort, committed=%t", ok)
	case <-time.After(100 * time.Millisecond):
	}
	close(converter.release)
	select {
	case ok := <-converter.committed:
		if !ok {
			t.Fatal("conversion did not commit")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("conversion never finished")
	}
	<-done
	if _, ok := f.cache.GetByID(f.draftID); ok {
		t.Fatal("draft must be deleted once the conversion committed")
	}
}
