package sync

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"sync"
)

const maxOriginTrustTransports = 8

// OriginCAResolver returns the PEM CA the NODE trusts for a primary origin
// ("https://host:port", lowercase). ok=false means no CA is stored for it.
type OriginCAResolver func(ctx context.Context, origin string) (caPEM string, ok bool, err error)

type trustOverrideKey struct{}

type trustOverride struct{ origin, caPEM string }

// WithTrustOverride makes requests to origin verify against caPEM for this
// context only. It carries a CA the owner has loaded but not yet confirmed, so
// nothing is persisted before enrollment is confirmed.
func WithTrustOverride(ctx context.Context, origin, caPEM string) context.Context {
	return context.WithValue(ctx, trustOverrideKey{}, trustOverride{origin: strings.ToLower(origin), caPEM: caPEM})
}

// IsUntrustedCertificate reports whether err is the peer's certificate being
// rejected (unknown authority, bad hostname, invalid chain), as opposed to an
// unreachable host or a protocol failure.
func IsUntrustedCertificate(err error) bool {
	var verification *tls.CertificateVerificationError
	var unknown x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	return errors.As(err, &verification) || errors.As(err, &unknown) ||
		errors.As(err, &hostname) || errors.As(err, &invalid)
}

// originTrustTransport verifies each primary against the CA stored for its
// origin (or a per-request override) and against the system roots when none is
// set. A CA never applies to another origin. Transports are TLS 1.3 / h2 only,
// like every other node->primary client.
type originTrustTransport struct {
	resolve OriginCAResolver
	system  http.RoundTripper

	mu      sync.Mutex
	pinned  map[string]http.RoundTripper
	closers []http.RoundTripper
}

// NewOriginTrustTransport builds the trust-aware transport. resolve may be nil.
func NewOriginTrustTransport(resolve OriginCAResolver) http.RoundTripper {
	return &originTrustTransport{
		resolve: resolve,
		system:  snapshotTLS13Transport(nil),
		pinned:  map[string]http.RoundTripper{},
	}
}

func (t *originTrustTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	origin := "https://" + strings.ToLower(request.URL.Host)
	caPEM, err := t.caFor(request.Context(), origin)
	if err != nil {
		return nil, err
	}
	if caPEM == "" {
		return t.system.RoundTrip(request)
	}
	transport, err := t.transportFor(origin, caPEM)
	if err != nil {
		return nil, err
	}
	return transport.RoundTrip(request)
}

func (t *originTrustTransport) caFor(ctx context.Context, origin string) (string, error) {
	if override, ok := ctx.Value(trustOverrideKey{}).(trustOverride); ok && override.origin == origin {
		return override.caPEM, nil
	}
	if t.resolve == nil {
		return "", nil
	}
	caPEM, ok, err := t.resolve(ctx, origin)
	if err != nil {
		return "", errors.New("primary trust lookup failed")
	}
	if !ok {
		return "", nil
	}
	return caPEM, nil
}

func (t *originTrustTransport) transportFor(origin, caPEM string) (http.RoundTripper, error) {
	sum := sha256.Sum256([]byte(caPEM))
	key := origin + "|" + hex.EncodeToString(sum[:])
	t.mu.Lock()
	defer t.mu.Unlock()
	if transport, ok := t.pinned[key]; ok {
		return transport, nil
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(caPEM)) {
		return nil, errors.New("primary trust anchor unusable")
	}
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.TLSClientConfig = &tls.Config{RootCAs: pool}
	transport := snapshotTLS13Transport(base)
	if len(t.pinned) >= maxOriginTrustTransports {
		for _, old := range t.pinned {
			if closer, ok := old.(interface{ CloseIdleConnections() }); ok {
				closer.CloseIdleConnections()
			}
		}
		t.pinned = map[string]http.RoundTripper{}
	}
	t.pinned[key] = transport
	return transport, nil
}

// CloseIdleConnections lets http.Client release pooled connections.
func (t *originTrustTransport) CloseIdleConnections() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, transport := range append([]http.RoundTripper{t.system}, t.pinnedList()...) {
		if closer, ok := transport.(interface{ CloseIdleConnections() }); ok {
			closer.CloseIdleConnections()
		}
	}
}

func (t *originTrustTransport) pinnedList() []http.RoundTripper {
	list := make([]http.RoundTripper, 0, len(t.pinned))
	for _, transport := range t.pinned {
		list = append(list, transport)
	}
	return list
}
