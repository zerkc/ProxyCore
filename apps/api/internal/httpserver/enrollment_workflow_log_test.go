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
	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
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
				return httpserver.EnrollmentLocalIdentity{InstallationID: f.localInstall, NodeID: f.localNode, Role: f.localRole, ClusterKeyID: f.localKeyID}, nil
			},
			LocalIngress:   func(context.Context) (domain.Ingress, error) { return domain.Ingress{IPv4: "192.0.2.7"}, nil },
			Converter:      fakeConverter{f},
			ExchangeGrant:  f.exchange,
			AcceptGrant:    accept,
			LoadClusterKEK: f.loadKEK,
			Logger:         logger,
			RecordSyncBinding: func(context.Context, configuration.NodeSyncBinding, string) error {
				return f.bindErr
			},
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
			if strings.Contains(output, "cause=") && !strings.Contains(output, " msg=\"") {
				t.Fatalf("rejected line must carry msg=: %q", output)
			}
			for _, secret := range []string{f.token, "pcenr1_"} {
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

func TestConfirmLogsStepReasonStatusAndRedactedCauseForEveryFailure(t *testing.T) {
	key := uuid.NewString()
	cases := []struct {
		name   string
		status int
		want   []string
		setup  func(f *confirmFixture)
	}{
		{"role ineligible", http.StatusConflict, []string{"step=eligibility", "reason=role_ineligible", "role=primary-with-nodes"}, func(f *confirmFixture) {
			f.localRole, f.localKeyID = domain.TopologyRolePrimaryWithNodes, key
		}},
		{"bound node not unlinked", http.StatusConflict, []string{"step=eligibility", "reason=node_not_unlinked"}, func(f *confirmFixture) {
			f.localRole, f.localKeyID = domain.TopologyRoleNode, key
		}},
		{"identity mismatch", http.StatusConflict, []string{"step=identity", "reason=identity_mismatch"}, func(f *confirmFixture) {
			f.localNode = domain.NewNodeID()
		}},
		{"exchange unavailable", http.StatusServiceUnavailable, []string{"step=exchange", "reason=exchange_unavailable", "ErrExchangeUnavailable"}, func(f *confirmFixture) {
			f.exchangeErr = fmt.Errorf("dial %s: %w", f.token, syncpublication.ErrExchangeUnavailable)
		}},
		{"exchange rate limited", http.StatusTooManyRequests, []string{"step=exchange", "reason=exchange_rate_limited"}, func(f *confirmFixture) {
			f.exchangeErr = fmt.Errorf("limit %s: %w", f.token, syncpublication.ErrExchangeRateLimited)
		}},
		{"exchange denied has cause", http.StatusConflict, []string{"step=exchange", "reason=exchange_denied", "status=409", "ErrExchangeDenied", "*fmt.wrapError"}, func(f *confirmFixture) {
			f.exchangeErr = fmt.Errorf("primary said %s: %w", f.token, syncpublication.ErrExchangeDenied)
		}},
		{"accept persistence", http.StatusServiceUnavailable, []string{"step=accept", "reason=accept_persistence_failed", "ErrBootstrapPersistence"}, func(f *confirmFixture) {
			f.acceptErrs = []error{fmt.Errorf("raw detail %s: %w", f.token, enrollment.ErrBootstrapPersistence)}
		}},
		{"accept denied cause", http.StatusConflict, []string{"step=accept", "reason=accept_denied", "ErrBootstrapDenied"}, func(f *confirmFixture) {
			f.acceptErrs = []error{fmt.Errorf("raw detail %s: %w", f.token, enrollment.ErrBootstrapDenied)}
		}},
		{"kek load", http.StatusServiceUnavailable, []string{"step=kek_load", "reason=kek_load_failed", "*errors.errorString"}, func(f *confirmFixture) {
			f.loadErr = errors.New("read-back failed " + f.token)
		}},
		{"record sync", http.StatusServiceUnavailable, []string{"step=record_sync", "reason=record_sync_failed", "*errors.errorString"}, func(f *confirmFixture) {
			f.bindErr = errors.New("disk full " + f.token)
		}},
		{"convert aborted", http.StatusConflict, []string{"step=convert", "reason=convert_failed", "class=aborted", "ErrNodeConversionAborted"}, func(f *confirmFixture) {
			f.convertErr = fmt.Errorf("apply %s: %w", f.token, enrollment.ErrNodeConversionAborted)
		}},
		{"convert denied", http.StatusConflict, []string{"step=convert", "class=denied"}, func(f *confirmFixture) {
			f.convertErr = enrollment.ErrNodeConversionDenied
		}},
		{"convert canceled", http.StatusConflict, []string{"step=convert", "class=canceled", "context.Canceled"}, func(f *confirmFixture) {
			f.convertErr = fmt.Errorf("x %s: %w", f.token, context.Canceled)
		}},
		{"convert deadline", http.StatusConflict, []string{"step=convert", "class=deadline"}, func(f *confirmFixture) {
			f.convertErr = context.DeadlineExceeded
		}},
		{"convert import", http.StatusConflict, []string{"step=convert", "class=import", "ErrNodeConversionImport"}, func(f *confirmFixture) {
			f.convertErr = enrollment.NewNodeConversionImportError(errors.New("import " + f.token))
		}},
		{"convert unknown", http.StatusConflict, []string{"step=convert", "class=other", "*errors.errorString"}, func(f *confirmFixture) {
			f.convertErr = errors.New("weird " + f.token)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newConfirmFixture(t, true)
			var buf bytes.Buffer
			tc.setup(f)
			f.withLogger(log.New(&buf, "", 0), nil)
			rec := f.confirm()
			if rec.Code != tc.status {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			out := buf.String()
			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Fatalf("log missing %q:\n%s", want, out)
				}
			}
			if !strings.Contains(out, fmt.Sprintf("status=%d", tc.status)) {
				t.Fatalf("log missing status:\n%s", out)
			}
			for _, secret := range []string{f.token, "pcenr1_"} {
				if strings.Contains(out, secret) {
					t.Fatalf("log leaks %q:\n%s", secret, out)
				}
			}
		})
	}
}

func TestConfirmLogsSuccessLine(t *testing.T) {
	f := newConfirmFixture(t, true)
	var buf bytes.Buffer
	f.withLogger(log.New(&buf, "", 0), nil)
	if rec := f.confirm(); rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	out := buf.String()
	if !strings.Contains(out, "enrollment confirm committed role=node generation=1") || strings.Contains(out, f.token) {
		t.Fatalf("log = %s", out)
	}
	last := -1
	for _, step := range []string{"draft_lookup", "tls_authority", "identity", "ingress", "binding", "exchange", "accept", "kek_load", "validate", "record_sync", "convert", "response"} {
		for _, suffix := range []string{" start", " ok dur="} {
			want := "enrollment confirm step=" + step + suffix
			idx := strings.Index(out, want)
			if idx < 0 || idx < last {
				t.Fatalf("log missing or out of order %q:\n%s", want, out)
			}
			last = idx
		}
	}
}

func TestConfirmLogsRedactedMessageForLeakyError(t *testing.T) {
	f := newConfirmFixture(t, true)
	var buf bytes.Buffer
	f.convertErr = enrollment.NewNodeConversionImportError(fmt.Errorf("save archive for https://primary.example/x with %s and key=topsecret at 192.0.2.7", f.token))
	f.withLogger(log.New(&buf, "", 0), nil)
	if rec := f.confirm(); rec.Code != http.StatusConflict {
		t.Fatalf("status=%d", rec.Code)
	}
	out := buf.String()
	if !strings.Contains(out, "step=convert start") || !strings.Contains(out, "msg=\"") || !strings.Contains(out, "save archive for <redacted> with <redacted>") {
		t.Fatalf("log = %s", out)
	}
	for _, secret := range []string{f.token, "primary.example", "topsecret", "192.0.2.7"} {
		if strings.Contains(out, secret) {
			t.Fatalf("log leaks %q:\n%s", secret, out)
		}
	}
}

func TestConfirmLogsNonCauseRejections(t *testing.T) {
	f := newConfirmFixture(t, true)
	var buf bytes.Buffer
	f.withLogger(log.New(&buf, "", 0), nil)
	f.cache.DeleteByID(f.draftID)
	if rec := f.confirm(); rec.Code != http.StatusGone {
		t.Fatalf("status=%d", rec.Code)
	}
	if out := buf.String(); !strings.Contains(out, "step=draft_lookup") || !strings.Contains(out, "reason=draft_expired") || !strings.Contains(out, "status=410") {
		t.Fatalf("log = %s", out)
	}
}
