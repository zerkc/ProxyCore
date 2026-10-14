package httpserver_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/acme"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/enrollment"
	"github.com/zerkc/ProxyCore/apps/api/internal/httpserver"
	replicationsnapshot "github.com/zerkc/ProxyCore/apps/api/internal/snapshot"
	syncpublication "github.com/zerkc/ProxyCore/apps/api/internal/sync"
)

const trustTestOrigin = "https://primary.example:3443"

func trustTestCA(t *testing.T) (caPEM, fingerprint string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "workflow trust ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		BasicConstraintsValid: true, IsCA: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), acme.DERHashSHA256(der)
}

type trustDraftFixture struct {
	handler http.Handler
	fetched int
	gotCA   string
	hadCA   bool
	fetch   func(ctx context.Context) (*replicationsnapshot.Envelope, error)
}

func newTrustDraftFixture(t *testing.T) *trustDraftFixture {
	t.Helper()
	f := &trustDraftFixture{}
	envelope := workflowTestEnvelope(t)
	f.fetch = func(context.Context) (*replicationsnapshot.Envelope, error) { return envelope, nil }
	f.handler = httpserver.NewEnrollmentIdentityMux(httpserver.EnrollmentIdentityHandlerOptions{
		Certificate: func(context.Context) (*x509.Certificate, error) {
			return &x509.Certificate{DNSNames: []string{"primary.example"}}, nil
		},
		Workflow: &httpserver.EnrollmentWorkflowHandlerOptions{
			Cache:       enrollment.NewDraftCache(enrollment.DraftCacheOptions{}),
			VerifyToken: func(context.Context, string) error { return nil },
			FetchSnapshot: func(ctx context.Context, _, _ string) (*replicationsnapshot.Envelope, error) {
				f.fetched++
				f.gotCA, f.hadCA = syncpublication.TrustOverrideFor(ctx, trustTestOrigin)
				return f.fetch(ctx)
			},
			Validator: snapshotValidatorFunc(func(context.Context, replicationsnapshot.Envelope) replicationsnapshot.ValidationResult {
				return replicationsnapshot.ValidationResult{OK: true}
			}),
			Identity: func(context.Context) (httpserver.EnrollmentLocalIdentity, error) {
				return httpserver.EnrollmentLocalIdentity{InstallationID: domain.NewInstallationID(), NodeID: domain.NewNodeID()}, nil
			},
			LocalIngress: func(context.Context) (domain.Ingress, error) { return domain.Ingress{IPv4: "192.0.2.7"}, nil },
		},
	})
	return f
}

func (f *trustDraftFixture) draft(caPEM string) *httptest.ResponseRecorder {
	payload := map[string]string{"token": workflowTestToken(), "primaryURL": trustTestOrigin + "/"}
	if caPEM != "" {
		payload["caPem"] = caPEM
	}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, httpserver.EnrollmentDraftPath, bytes.NewReader(body))
	req.Host = "primary.example:3443"
	req.TLS = &tls.ConnectionState{Version: tls.VersionTLS13, HandshakeComplete: true, ServerName: "primary.example"}
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

func TestDraftWithCAUsesItForTheFetchAndReturnsItsFingerprint(t *testing.T) {
	f := newTrustDraftFixture(t)
	caPEM, fingerprint := trustTestCA(t)
	rec := f.draft(caPEM)
	if rec.Code != http.StatusOK || f.fetched != 1 || !f.hadCA || f.gotCA != caPEM {
		t.Fatalf("status=%d fetched=%d hadCA=%v body=%s", rec.Code, f.fetched, f.hadCA, rec.Body.String())
	}
	var response struct {
		PrimaryCAFingerprint string `json:"primaryCaFingerprint"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil || response.PrimaryCAFingerprint != fingerprint {
		t.Fatalf("fingerprint = %q (err %v), want %q", response.PrimaryCAFingerprint, err, fingerprint)
	}
}

func TestDraftWithoutCAKeepsTheDefaultTrust(t *testing.T) {
	f := newTrustDraftFixture(t)
	rec := f.draft("")
	if rec.Code != http.StatusOK || f.fetched != 1 || f.hadCA {
		t.Fatalf("status=%d fetched=%d hadCA=%v body=%s", rec.Code, f.fetched, f.hadCA, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "primaryCaFingerprint") {
		t.Fatalf("a draft without a CA must not report a CA fingerprint: %s", rec.Body.String())
	}
}

func TestDraftRejectsAnInvalidCAWithoutContactingThePrimary(t *testing.T) {
	f := newTrustDraftFixture(t)
	rec := f.draft("-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n")
	if rec.Code != http.StatusBadRequest || f.fetched != 0 || !strings.Contains(rec.Body.String(), "invalid CA certificate") {
		t.Fatalf("status=%d fetched=%d body=%s", rec.Code, f.fetched, rec.Body.String())
	}
}

func TestDraftExplainsAnUntrustedPrimaryCertificate(t *testing.T) {
	f := newTrustDraftFixture(t)
	f.fetch = func(context.Context) (*replicationsnapshot.Envelope, error) {
		return nil, syncpublication.ErrSnapshotClientUntrusted
	}
	rec := f.draft("")
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "primary certificate not trusted") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	// Other failures keep the generic message.
	f.fetch = func(context.Context) (*replicationsnapshot.Envelope, error) { return nil, errors.New("down") }
	rec = f.draft("")
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "primary snapshot unavailable") {
		t.Fatalf("generic status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestConfirmWithADraftCAUsesItForTheExchangeAndPersistsItBeforeBinding(t *testing.T) {
	f := newConfirmFixture(t, true)
	caPEM, _ := trustTestCA(t)
	if !f.cache.AttachPrimaryCA(f.draftID, caPEM) {
		t.Fatal("AttachPrimaryCA refused a live draft")
	}
	rec := f.confirm()
	if rec.Code != http.StatusOK {
		t.Fatalf("confirm status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(f.exchangeCAs) != 1 || f.exchangeCAs[0] != caPEM {
		t.Fatalf("exchange saw CA %q, want the draft's", f.exchangeCAs)
	}
	if len(f.trusts) != 1 || f.trusts[0].origin != trustTestOrigin || f.trusts[0].caPEM != caPEM {
		t.Fatalf("persisted trust = %+v", f.trusts)
	}
	if strings.Join(f.order, ",") != "trust,bind" {
		t.Fatalf("order = %v, want the CA persisted before the sync binding", f.order)
	}
}

func TestConfirmWithoutADraftCAPersistsNoTrust(t *testing.T) {
	f := newConfirmFixture(t, true)
	rec := f.confirm()
	if rec.Code != http.StatusOK || len(f.trusts) != 0 || f.exchangeCAs[0] != "" {
		t.Fatalf("status=%d trusts=%+v exchangeCAs=%q", rec.Code, f.trusts, f.exchangeCAs)
	}
}

func TestConfirmFailsClosedWhenTheCACannotBePersisted(t *testing.T) {
	f := newConfirmFixture(t, true)
	caPEM, _ := trustTestCA(t)
	f.cache.AttachPrimaryCA(f.draftID, caPEM)
	f.trustErr = errors.New("disk full")
	rec := f.confirm()
	if rec.Code != http.StatusServiceUnavailable || f.convertCalls != 0 || len(f.bindings) != 0 {
		t.Fatalf("status=%d converts=%d bindings=%d body=%s", rec.Code, f.convertCalls, len(f.bindings), rec.Body.String())
	}
}
