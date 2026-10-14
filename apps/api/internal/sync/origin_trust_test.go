package sync

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type trustPKI struct {
	caPEM string
	leaf  tls.Certificate
}

func newTrustPKI(t *testing.T) trustPKI {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "origin trust test ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		BasicConstraintsValid: true, IsCA: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, _ := x509.ParseCertificate(caDER)
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano() + 1), Subject: pkix.Name{CommonName: "primary"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return trustPKI{
		caPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})),
		leaf:  tls.Certificate{Certificate: [][]byte{leafDER}, PrivateKey: leafKey},
	}
}

// startTrustServer serves TLS 1.3 + h2 with a leaf signed by the PKI's CA.
func startTrustServer(t *testing.T, pki trustPKI) (origin string) {
	t.Helper()
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	server.EnableHTTP2 = true
	server.TLS = &tls.Config{Certificates: []tls.Certificate{pki.leaf}, MinVersion: tls.VersionTLS13}
	server.StartTLS()
	t.Cleanup(server.Close)
	return "https://" + strings.ToLower(server.Listener.Addr().String())
}

func trustGet(t *testing.T, rt http.RoundTripper, ctx context.Context, origin string) error {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := rt.RoundTrip(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func TestOriginTrustTransportRejectsAnUntrustedPrimaryByDefault(t *testing.T) {
	origin := startTrustServer(t, newTrustPKI(t))
	rt := NewOriginTrustTransport(nil)
	err := trustGet(t, rt, context.Background(), origin)
	if err == nil || !IsUntrustedCertificate(err) {
		t.Fatalf("err = %v, want an untrusted-certificate failure", err)
	}
}

func TestOriginTrustTransportTrustsTheStoredCAForThatOriginOnly(t *testing.T) {
	pki, other := newTrustPKI(t), newTrustPKI(t)
	origin, otherOrigin := startTrustServer(t, pki), startTrustServer(t, other)
	rt := NewOriginTrustTransport(func(_ context.Context, o string) (string, bool, error) {
		if o == origin {
			return pki.caPEM, true, nil
		}
		return "", false, nil
	})
	if err := trustGet(t, rt, context.Background(), origin); err != nil {
		t.Fatalf("trusted origin: %v", err)
	}
	if err := trustGet(t, rt, context.Background(), otherOrigin); !IsUntrustedCertificate(err) {
		t.Fatalf("CA for one origin must not trust another, err = %v", err)
	}
}

func TestOriginTrustTransportFollowsAReplacedCA(t *testing.T) {
	oldPKI, rebuilt := newTrustPKI(t), newTrustPKI(t)
	// A rebuilt primary is a different server at the same address, so serve the
	// rebuilt PKI and have the resolver first answer with the stale CA.
	origin := startTrustServer(t, rebuilt)
	current := oldPKI.caPEM
	rt := NewOriginTrustTransport(func(context.Context, string) (string, bool, error) { return current, true, nil })
	if err := trustGet(t, rt, context.Background(), origin); !IsUntrustedCertificate(err) {
		t.Fatalf("stale CA err = %v, want untrusted", err)
	}
	current = rebuilt.caPEM // owner loaded the new CA during re-enrollment
	if err := trustGet(t, rt, context.Background(), origin); err != nil {
		t.Fatalf("after replacing the CA: %v", err)
	}
}

func TestOriginTrustTransportOverrideBeatsStoreAndNeedsNoResolver(t *testing.T) {
	pki := newTrustPKI(t)
	origin := startTrustServer(t, pki)
	rt := NewOriginTrustTransport(nil)
	ctx := WithTrustOverride(context.Background(), origin, pki.caPEM)
	if err := trustGet(t, rt, ctx, origin); err != nil {
		t.Fatalf("override: %v", err)
	}
	// An override for another origin does not apply here.
	wrong := WithTrustOverride(context.Background(), "https://127.0.0.1:1", pki.caPEM)
	if err := trustGet(t, rt, wrong, origin); !IsUntrustedCertificate(err) {
		t.Fatalf("override for another origin err = %v, want untrusted", err)
	}
}

func TestOriginTrustTransportFailsClosedOnResolverOrCAErrors(t *testing.T) {
	origin := startTrustServer(t, newTrustPKI(t))
	boom := errors.New("store down")
	if err := trustGet(t, NewOriginTrustTransport(func(context.Context, string) (string, bool, error) { return "", false, boom }), context.Background(), origin); err == nil || IsUntrustedCertificate(err) {
		t.Fatalf("resolver error = %v, want a non-trust failure", err)
	}
	if err := trustGet(t, NewOriginTrustTransport(func(context.Context, string) (string, bool, error) { return "not pem", true, nil }), context.Background(), origin); err == nil {
		t.Fatal("unparseable stored CA must fail the request")
	}
}

func TestSnapshotClientVerifiesThroughTheOriginTrustTransport(t *testing.T) {
	pki := newTrustPKI(t)
	origin := startTrustServer(t, pki)
	client := NewSnapshotClient(SnapshotClientOptions{
		HTTPClient:  &http.Client{Transport: NewOriginTrustTransport(nil)},
		MaxAttempts: 1,
	})
	_, err := client.FetchSnapshotByToken(context.Background(), origin, "pcenr1_aaaa_bbbb")
	if !errors.Is(err, ErrSnapshotClientUntrusted) {
		t.Fatalf("without CA err = %v, want ErrSnapshotClientUntrusted", err)
	}
	// With the CA the TLS layer passes and the test server's 200 body comes back.
	body, err := client.FetchSnapshotByToken(WithTrustOverride(context.Background(), origin, pki.caPEM), origin, "pcenr1_aaaa_bbbb")
	if err != nil || string(body) != "ok" {
		t.Fatalf("with CA body=%q err=%v, want the served body", body, err)
	}
}
