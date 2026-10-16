package httpserver_test

import (
	"bytes"
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/cluster"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/enrollment"
	"github.com/zerkc/ProxyCore/apps/api/internal/httpserver"
	replicationsnapshot "github.com/zerkc/ProxyCore/apps/api/internal/snapshot"
	syncpublication "github.com/zerkc/ProxyCore/apps/api/internal/sync"
)

// withLogger rebuilds the fixture's handler with a logger injected, reusing the
// fixture's fakes for every seam.
func (f *confirmFixture) withLogger(logger *log.Logger, accept httpserver.EnrollmentGrantAcceptor) {
	if accept == nil {
		accept = f.accept
	}
	f.handler = httpserver.NewEnrollmentIdentityMux(httpserver.EnrollmentIdentityHandlerOptions{
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
			Converter:      fakeConverter{f},
			ExchangeGrant:  f.exchange,
			AcceptGrant:    accept,
			LoadClusterKEK: f.loadKEK,
			Logger:         logger,
		},
	})
}

func TestConfirmLogsRedactedReasonCodes(t *testing.T) {
	cases := []struct {
		name   string
		reason string
		setup  func(f *confirmFixture) httpserver.EnrollmentGrantAcceptor
	}{
		{"exchange denied", "exchange_denied", func(f *confirmFixture) httpserver.EnrollmentGrantAcceptor {
			f.exchangeErr = fmt.Errorf("primary said %s: %w", f.token, syncpublication.ErrExchangeDenied)
			return nil
		}},
		{"exchange invalid", "exchange_invalid", func(f *confirmFixture) httpserver.EnrollmentGrantAcceptor {
			f.exchangeErr = fmt.Errorf("primary said %s: %w", f.token, syncpublication.ErrExchangeInvalid)
			return nil
		}},
		{"exchange unknown", "exchange_unknown", func(f *confirmFixture) httpserver.EnrollmentGrantAcceptor {
			f.exchangeErr = errors.New("weird failure echoing " + f.token)
			return nil
		}},
		{"accept denied", "accept_denied", func(f *confirmFixture) httpserver.EnrollmentGrantAcceptor {
			f.acceptErrs = []error{fmt.Errorf("raw detail %s: %w", f.token, enrollment.ErrBootstrapDenied)}
			return nil
		}},
		{"cluster key id invalid", "cluster_key_id_invalid", func(f *confirmFixture) httpserver.EnrollmentGrantAcceptor {
			return func(context.Context, *enrollment.BootstrapRecipient, enrollment.BootstrapBinding, []byte, string) (enrollment.AcceptedBootstrap, error) {
				return enrollment.AcceptedBootstrap{ClusterKeyID: "not-a-uuid"}, nil
			}
		}},
		{"binding invalid", "binding_invalid", func(f *confirmFixture) httpserver.EnrollmentGrantAcceptor {
			// A draft whose snapshot has no leadership generation cannot be bound.
			bad := workflowTestEnvelope(t)
			bad.Transient.LeadershipGeneration = 0
			overlay := enrollment.NodeLocalOverlay{NodeID: f.localNode, Ingress: domain.Ingress{IPv4: "192.0.2.7"}, Role: domain.TopologyRoleNode}
			draft, err := f.cache.PutWithPrimaryURLAndToken("fedcba9876543210fedcba9876543210", "https://primary.example:3443/", bad, overlay, f.token)
			if err != nil {
				t.Fatal(err)
			}
			f.draftID = draft.DraftID
			return nil
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newConfirmFixture(t, true)
			var buf bytes.Buffer
			accept := tc.setup(f)
			f.withLogger(log.New(&buf, "", 0), accept)
			rec := f.confirm()
			if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "enrollment grant denied") {
				t.Fatalf("response must stay generic: status=%d body=%s", rec.Code, rec.Body.String())
			}
			output := buf.String()
			if want := "reason=" + tc.reason; !strings.Contains(output, want) {
				t.Fatalf("log = %q, want %q", output, want)
			}
			for _, secret := range []string{f.token, "pcenr1_", "raw detail", "weird failure", "primary said"} {
				if strings.Contains(output, secret) {
					t.Fatalf("log leaks %q: %q", secret, output)
				}
			}
		})
	}
}

func TestConfirmNilLoggerStillRejectsGenerically(t *testing.T) {
	f := newConfirmFixture(t, true)
	f.exchangeErr = syncpublication.ErrExchangeDenied
	f.withLogger(nil, nil)
	if rec := f.confirm(); rec.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	_ = uuid.Nil
}
