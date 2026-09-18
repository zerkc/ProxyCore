package enrollment_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/acme"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/enrollment"
	"github.com/zerkc/ProxyCore/apps/api/internal/httpserver"
	"github.com/zerkc/ProxyCore/apps/api/internal/identity"
)

func TestEnrollmentTLSServerRealTLS13ProofHandshake(t *testing.T) {
	h := newTLSHandshakeHarness(t, "primary.example")
	response, err := h.client(t, "primary.example", tls.VersionTLS13).Post(h.proofEndpoint, "application/json", bytes.NewReader(h.fixture.body()))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.TLS == nil || response.TLS.Version != tls.VersionTLS13 {
		t.Fatalf("proof status/TLS = %d/%#v", response.StatusCode, response.TLS)
	}
	if h.observed.calls != 1 || h.observed.tls == nil || !h.observed.tls.HandshakeComplete || h.observed.tls.Version != tls.VersionTLS13 {
		t.Fatalf("handler observation = %#v calls=%d", h.observed.tls, h.observed.calls)
	}
	if h.observed.host != h.fixture.host || h.observed.tls.ServerName != "primary.example" {
		t.Fatalf("handler authority = host %q SNI %q", h.observed.host, h.observed.tls.ServerName)
	}
	var proof enrollment.IdentityProof
	if err := json.NewDecoder(response.Body).Decode(&proof); err != nil {
		t.Fatal(err)
	}
	if err := enrollment.VerifyIdentityProof(proof, enrollment.IdentityProofVerifierOptions{
		Request:                      h.fixture.request,
		ExpectedInstallationID:       h.fixture.id.InstallationID,
		ExpectedNodeID:               h.fixture.id.NodeID,
		ExpectedLeadershipGeneration: h.fixture.id.LeadershipGeneration,
		LeafCertificatePEM:           h.fixture.leaf.CertificatePEM,
		CACertificatePEM:             h.fixture.ca.CertificatePEM,
		Now:                          h.fixture.now,
	}); err != nil {
		t.Fatalf("proof verification: %v", err)
	}
}

func TestEnrollmentTLSServerRejectsWrongHostnameAndTLS12BeforeHandler(t *testing.T) {
	h := newTLSHandshakeHarness(t, "primary.example")
	wrongURL := "https://other.example:" + h.port + httpserver.EnrollmentIdentityProofPath
	response, err := h.client(t, "other.example", tls.VersionTLS13).Post(wrongURL, "application/json", bytes.NewReader(h.fixture.body()))
	if err == nil {
		response.Body.Close()
		t.Fatal("wrong hostname completed a TLS handshake")
	}
	if h.observed.calls != 0 {
		t.Fatalf("wrong hostname reached handler %d times", h.observed.calls)
	}

	response, err = h.client(t, "primary.example", tls.VersionTLS12).Post(h.proofEndpoint, "application/json", bytes.NewReader(h.fixture.body()))
	if err == nil {
		response.Body.Close()
		t.Fatal("TLS 1.2 completed a TLS handshake")
	}
	if h.observed.calls != 0 {
		t.Fatalf("TLS 1.2 reached handler %d times", h.observed.calls)
	}
}

func TestEnrollmentTLSServerPlaintextAndUnknownPathNeverRedirect(t *testing.T) {
	h := newTLSHandshakeHarness(t, "primary.example")
	conn, err := net.DialTimeout("tcp", h.address, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	if _, err := io.WriteString(conn, "GET /api/health HTTP/1.1\r\nHost: "+h.fixture.host+"\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	plaintext, _ := io.ReadAll(conn)
	conn.Close()
	lower := bytes.ToLower(plaintext)
	if bytes.Contains(lower, []byte("location:")) || bytes.Contains(plaintext, []byte("200 OK")) || bytes.Contains(lower, []byte("identity proof")) {
		t.Fatalf("plaintext received redirect/useful response: %q", plaintext)
	}

	unknownURL := strings.TrimSuffix(h.fixture.request.PrimaryURL, "/") + "/api/health"
	response, err := h.client(t, "primary.example", tls.VersionTLS13).Get(unknownURL)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNotFound || response.Header.Get("Location") != "" {
		t.Fatalf("unknown TLS path = %d Location=%q", response.StatusCode, response.Header.Get("Location"))
	}
}

func TestEnrollmentTLSServerProviderRotationUsesCurrentValidCertificate(t *testing.T) {
	h := newTLSHandshakeHarness(t, "primary.example")
	initial := h.presentedSerial(t, "primary.example")
	next, err := acme.IssueSignedByCA([]string{"primary.example"}, 30, h.fixture.ca.CertificatePEM, h.fixture.ca.PrivateKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	nextMaterial := enrollment.TLSCertificateMaterial{
		CertificatePEM:   next.CertificatePEM,
		PrivateKeyPEM:    next.PrivateKeyPEM,
		CACertificatePEM: h.fixture.ca.CertificatePEM,
	}
	if err := h.provider.Replace(nextMaterial); err != nil {
		t.Fatal(err)
	}
	want := parseCert(t, next.CertificatePEM).SerialNumber.String()
	if got := h.presentedSerial(t, "primary.example"); got == initial || got != want {
		t.Fatalf("rotated serial = %s, initial=%s want=%s", got, initial, want)
	}
	bad := nextMaterial
	bad.PrivateKeyPEM = "invalid private key"
	if err := h.provider.Replace(bad); err == nil {
		t.Fatal("invalid replacement was accepted")
	}
	if got := h.presentedSerial(t, "primary.example"); got != want {
		t.Fatalf("serial after invalid replacement = %s, want prior %s", got, want)
	}
}

func TestEnrollmentTLSServerIPCertificateWorksWithoutSNI(t *testing.T) {
	h := newTLSHandshakeHarness(t, "127.0.0.1")
	response, err := h.client(t, "127.0.0.1", tls.VersionTLS13).Post(h.proofEndpoint, "application/json", bytes.NewReader(h.fixture.body()))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || h.observed.tls == nil || h.observed.tls.ServerName != "" {
		t.Fatalf("IP proof status=%d handler SNI=%q", response.StatusCode, h.observed.tls.ServerName)
	}
}

func TestEnrollmentTLSServerShutdownClosesAcceptAndIdleConnectionsIdempotently(t *testing.T) {
	h := newTLSHandshakeHarness(t, "primary.example")
	conn := h.tlsConn(t, "primary.example")
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	if _, err := fmt.Fprintf(conn, "GET /unknown HTTP/1.1\r\nHost: %s\r\nConnection: keep-alive\r\n\r\n", h.fixture.host); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	response.Body.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := h.server.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if err := h.server.Shutdown(ctx); err != nil {
		t.Fatalf("repeated shutdown: %v", err)
	}
	select {
	case err := <-h.serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			t.Fatalf("serve error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Serve did not stop after Shutdown")
	}
	h.stopped = true
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	var one [1]byte
	if n, err := conn.Read(one[:]); err == nil || n != 0 {
		t.Fatalf("idle connection remained open: n=%d err=%v", n, err)
	}
	if probe, err := net.DialTimeout("tcp", h.address, 200*time.Millisecond); err == nil {
		probe.Close()
		t.Fatal("listener accepted a connection after Shutdown")
	}
}

type handshakeFixture struct {
	request enrollment.IdentityProofRequest
	id      identity.Identity
	now     time.Time
	host    string
	ca      acme.Material
	leaf    acme.Material
	handler http.Handler
}

func newHandshakeFixture(t *testing.T, primaryURL string) *handshakeFixture {
	parsed, err := url.Parse(primaryURL)
	if err != nil {
		t.Fatal(err)
	}
	materials := newProviderFixture(t)
	leaf, err := acme.IssueSignedByCA([]string{parsed.Hostname()}, 30, materials.ca.CertificatePEM, materials.ca.PrivateKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	request, err := enrollment.NewIdentityProofRequest(primaryURL)
	if err != nil {
		t.Fatal(err)
	}
	fixture := &handshakeFixture{
		request: request,
		id: identity.Identity{
			InstallationID:        domain.NewInstallationID(),
			NodeID:                domain.NewNodeID(),
			Role:                  domain.TopologyRolePrimary,
			LeadershipGeneration:  4,
			LatestKnownGeneration: 4,
		},
		now:  time.Now().UTC().Truncate(time.Second),
		host: parsed.Host,
		ca:   materials.ca,
		leaf: leaf,
	}
	signer, err := enrollment.NewIdentityProofSigner(enrollment.IdentityProofSignerOptions{
		Identity: func(context.Context) (identity.Identity, bool, error) {
			return fixture.id, true, nil
		},
		Material: func(context.Context) (string, string, string, error) {
			return fixture.leaf.CertificatePEM, fixture.leaf.PrivateKeyPEM, fixture.ca.CertificatePEM, nil
		},
		Now: func() time.Time { return fixture.now },
	})
	if err != nil {
		t.Fatal(err)
	}
	certificate := parseCert(t, leaf.CertificatePEM)
	fixture.handler = httpserver.NewEnrollmentIdentityMux(httpserver.EnrollmentIdentityHandlerOptions{
		Signer: signer,
		Certificate: func(context.Context) (*x509.Certificate, error) {
			return certificate, nil
		},
	})
	return fixture
}

func (f *handshakeFixture) body() []byte {
	body, _ := json.Marshal(f.request)
	return body
}

type handshakeObservation struct {
	calls int
	host  string
	tls   *tls.ConnectionState
}

type tlsHandshakeHarness struct {
	server        *enrollment.EnrollmentTLSServer
	provider      *enrollment.TLSCertificateProvider
	fixture       *handshakeFixture
	observed      handshakeObservation
	address       string
	port          string
	proofEndpoint string
	serveErr      chan error
	stopped       bool
}

func newTLSHandshakeHarness(t *testing.T, hostname string) *tlsHandshakeHarness {
	listener := mustListen(t)
	address := listener.Addr().String()
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatal(err)
	}
	fixture := newHandshakeFixture(t, "https://"+net.JoinHostPort(hostname, port)+"/")
	provider, err := enrollment.NewTLSCertificateProvider(enrollment.TLSCertificateMaterial{
		CertificatePEM:   fixture.leaf.CertificatePEM,
		PrivateKeyPEM:    fixture.leaf.PrivateKeyPEM,
		CACertificatePEM: fixture.ca.CertificatePEM,
	})
	if err != nil {
		t.Fatal(err)
	}
	h := &tlsHandshakeHarness{
		provider:      provider,
		fixture:       fixture,
		address:       address,
		port:          port,
		proofEndpoint: strings.TrimSuffix(fixture.request.PrimaryURL, "/") + httpserver.EnrollmentIdentityProofPath,
		serveErr:      make(chan error, 1),
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.observed.calls++
		h.observed.host = r.Host
		if r.TLS != nil {
			state := *r.TLS
			h.observed.tls = &state
		}
		fixture.handler.ServeHTTP(w, r)
	})
	h.server, err = enrollment.NewEnrollmentTLSServer(listener, handler, provider)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		h.serveErr <- h.server.Serve()
	}()
	t.Cleanup(func() { h.stop(t) })
	return h
}

func (h *tlsHandshakeHarness) stop(t *testing.T) {
	if h.stopped {
		return
	}
	h.stopped = true
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := h.server.Shutdown(ctx); err != nil {
		t.Errorf("cleanup shutdown: %v", err)
	}
	select {
	case err := <-h.serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("cleanup serve error: %v", err)
		}
	case <-time.After(time.Second):
		t.Error("cleanup Serve did not stop")
	}
}

func (h *tlsHandshakeHarness) roots(t *testing.T) *x509.CertPool {
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(h.fixture.ca.CertificatePEM)) {
		t.Fatal("append test CA")
	}
	return roots
}

func (h *tlsHandshakeHarness) client(t *testing.T, serverName string, version uint16) *http.Client {
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{
			RootCAs:    h.roots(t),
			ServerName: serverName,
			MinVersion: version,
			MaxVersion: version,
		},
		TLSHandshakeTimeout:   time.Second,
		ResponseHeaderTimeout: time.Second,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "tcp", h.address)
		},
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   2 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	t.Cleanup(client.CloseIdleConnections)
	return client
}

func (h *tlsHandshakeHarness) tlsConn(t *testing.T, serverName string) *tls.Conn {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	dialer := &tls.Dialer{
		NetDialer: &net.Dialer{Timeout: time.Second},
		Config: &tls.Config{
			RootCAs:    h.roots(t),
			ServerName: serverName,
			MinVersion: tls.VersionTLS13,
			MaxVersion: tls.VersionTLS13,
		},
	}
	conn, err := dialer.DialContext(ctx, "tcp", h.address)
	if err != nil {
		t.Fatal(err)
	}
	return conn.(*tls.Conn)
}

func (h *tlsHandshakeHarness) presentedSerial(t *testing.T, serverName string) string {
	conn := h.tlsConn(t, serverName)
	defer conn.Close()
	state := conn.ConnectionState()
	if len(state.PeerCertificates) == 0 {
		t.Fatal("server did not present a certificate")
	}
	return state.PeerCertificates[0].SerialNumber.String()
}
