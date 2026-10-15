package sync

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
)

const (
	snapshotPublicationHTTPPath    = "/api/topology/sync/snapshot"
	snapshotByTokenHTTPPath        = "/api/topology/sync/snapshot-by-token"
	maxSnapshotByTokenRequestBytes = 8 << 10

	snapshotAcknowledgementHTTPPath = "/api/topology/sync/acknowledge"
	snapshotAfterQueryKey           = "after"

	// NodeUnlinkedSignalCode is the machine-readable code in the primary's
	// 410 body when the node was unlinked (httpserver.NodeUnlinkedCode).
	NodeUnlinkedSignalCode = "NODE_UNLINKED"
	// maxUnlinkedBodyBytes bounds the 410 body that is inspected.
	maxUnlinkedBodyBytes = 1 << 10

	// SnapshotContentHashHeader and SnapshotAppliedAtHeader carry the primary's
	// metadata for the latest snapshot on publication responses (200 and 304).
	SnapshotContentHashHeader = "X-ProxyCore-Snapshot-Hash"
	SnapshotAppliedAtHeader   = "X-ProxyCore-Snapshot-Applied-At"
)

var (
	ErrSnapshotClientUnauthorized = errors.New("snapshot client unauthorized")
	ErrSnapshotClientGone         = errors.New("snapshot client credential gone")
	// ErrSnapshotClientUnlinked means the primary explicitly answered 410 with
	// the NODE_UNLINKED code: the owner unlinked this node. It is the only
	// authoritative unlink signal; a bare 410 stays ErrSnapshotClientGone.
	ErrSnapshotClientUnlinked    = errors.New("snapshot client node unlinked")
	ErrSnapshotClientNotFound    = errors.New("snapshot client snapshot not found")
	ErrSnapshotClientUnavailable = errors.New("snapshot client unavailable")
	ErrSnapshotClientInvalid     = errors.New("snapshot client response invalid")
	// ErrSnapshotClientUntrusted means the primary's certificate was rejected:
	// the node has no (or the wrong) CA for that primary.
	ErrSnapshotClientUntrusted = errors.New("snapshot client primary certificate untrusted")
)

// SnapshotClient is the NODE-side HTTPS adapter for the canonical PRIMARY
// snapshot endpoint. It owns no durable credential or response state.
type SnapshotClient struct {
	httpClient      *http.Client
	canonicalURL    func(string) (string, error)
	maxBodyBytes    int64
	retryableErrors int
	backoff         time.Duration
	now             func() time.Time
	sleep           func(context.Context, time.Duration) error
	ingress         func() (string, string)
}

func (c *SnapshotClient) setIngressHeaders(request *http.Request) {
	if c == nil || c.ingress == nil {
		return
	}
	v4, v6 := c.ingress()
	if v4 != "" && len(v4) <= 15 {
		request.Header.Set(IngressIPv4Header, v4)
	}
	if v6 != "" && len(v6) <= 45 {
		request.Header.Set(IngressIPv6Header, v6)
	}
}

// Request headers a NODE uses to report its own gateway (ingress) address on
// authenticated pulls and acknowledgements. The values are advisory display
// data; the primary validates them and never uses them for authentication.
const (
	IngressIPv4Header = "X-ProxyCore-Ingress-IPv4"
	IngressIPv6Header = "X-ProxyCore-Ingress-IPv6"
)

type SnapshotClientOptions struct {
	// Ingress returns the node's configured gateway addresses; empty values
	// are not sent. Nil disables reporting.
	Ingress         func() (ipv4, ipv6 string)
	HTTPClient      *http.Client
	MaxBodyBytes    int64
	MaxAttempts     int
	InitialBackoff  time.Duration
	Now             func() time.Time
	CanonicalizeURL func(string) (string, error)
}

// NewSnapshotClient constructs a TLS 1.3-only snapshot transport. A supplied
// HTTP client is retained as a test/trust seam, while concrete HTTP transports
// are cloned and constrained to TLS 1.3 and HTTP/2.
func NewSnapshotClient(opts SnapshotClientOptions) *SnapshotClient {
	maxBodyBytes := opts.MaxBodyBytes
	if maxBodyBytes <= 0 {
		maxBodyBytes = configuration.SnapshotPublicationMaxBytes
	}
	attempts := opts.MaxAttempts
	if attempts <= 0 {
		attempts = 3
	}
	backoff := opts.InitialBackoff
	if backoff < 0 {
		backoff = 0
	}
	if backoff == 0 && opts.InitialBackoff == 0 {
		backoff = 50 * time.Millisecond
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	canonicalURL := opts.CanonicalizeURL
	if canonicalURL == nil {
		canonicalURL = canonicalizeSnapshotPrimaryURL
	}
	return &SnapshotClient{
		httpClient:      snapshotHTTPClient(opts.HTTPClient),
		canonicalURL:    canonicalURL,
		maxBodyBytes:    maxBodyBytes,
		retryableErrors: attempts,
		backoff:         backoff,
		now:             now,
		sleep:           snapshotClientWait,
		ingress:         opts.Ingress,
	}
}

// FetchSnapshot retrieves one complete canonical snapshot body from the
// supplied PRIMARY origin. The credential may be a complete pcnode1 bearer;
// a raw 32-byte secret is also accepted for transport-bound callers and is
// encoded as its canonical raw-url base64 representation.
func (c *SnapshotClient) FetchSnapshot(ctx context.Context, primaryURL string, credential []byte) ([]byte, error) {
	if c == nil || c.httpClient == nil || c.canonicalURL == nil || ctx == nil || len(credential) == 0 {
		return nil, ErrSnapshotClientInvalid
	}
	if err := ctx.Err(); err != nil {
		return nil, ErrSnapshotClientUnavailable
	}
	normalized, err := c.canonicalURL(primaryURL)
	if err != nil || !validSnapshotClientOrigin(normalized) {
		return nil, ErrSnapshotClientInvalid
	}
	bearer, ok := snapshotBearer(credential)
	if !ok {
		return nil, ErrSnapshotClientInvalid
	}
	endpoint := strings.TrimRight(normalized, "/") + snapshotPublicationHTTPPath
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, ErrSnapshotClientInvalid
	}
	request.Header.Set("Authorization", "Bearer "+bearer)
	c.setIngressHeaders(request)
	result, err := c.fetchWithRetries(request, false)
	return result.Body, err
}

// SnapshotFetchResult is one conditional publication read. NotModified means
// the primary's latest snapshot is exactly the hash the caller already holds;
// Body is then empty. ContentHash and AppliedAt are the primary's own metadata
// for the latest snapshot and are what an acknowledgement must echo back.
type SnapshotFetchResult struct {
	NotModified bool
	Body        []byte
	ContentHash string
	AppliedAt   time.Time
}

// FetchSnapshotIfChanged is FetchSnapshot with the publication endpoint's
// AfterContentHash short-circuit: when after equals the primary's latest hash
// the primary answers 304 and no body is transferred.
func (c *SnapshotClient) FetchSnapshotIfChanged(ctx context.Context, primaryURL string, credential []byte, after string) (SnapshotFetchResult, error) {
	if after != "" && !validSnapshotHash(after) {
		return SnapshotFetchResult{}, ErrSnapshotClientInvalid
	}
	if c == nil || c.httpClient == nil || c.canonicalURL == nil || ctx == nil || len(credential) == 0 {
		return SnapshotFetchResult{}, ErrSnapshotClientInvalid
	}
	if err := ctx.Err(); err != nil {
		return SnapshotFetchResult{}, ErrSnapshotClientUnavailable
	}
	normalized, err := c.canonicalURL(primaryURL)
	if err != nil || !validSnapshotClientOrigin(normalized) {
		return SnapshotFetchResult{}, ErrSnapshotClientInvalid
	}
	bearer, ok := snapshotBearer(credential)
	if !ok {
		return SnapshotFetchResult{}, ErrSnapshotClientInvalid
	}
	endpoint := strings.TrimRight(normalized, "/") + snapshotPublicationHTTPPath
	if after != "" {
		endpoint += "?" + snapshotAfterQueryKey + "=" + after
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return SnapshotFetchResult{}, ErrSnapshotClientInvalid
	}
	request.Header.Set("Authorization", "Bearer "+bearer)
	c.setIngressHeaders(request)
	return c.fetchWithRetries(request, after != "")
}

type snapshotAcknowledgementBody struct {
	NodeID      string `json:"nodeId"`
	PrimaryURL  string `json:"primaryUrl"`
	ContentHash string `json:"contentHash"`
	AppliedAt   string `json:"appliedAt"`
}

// AcknowledgeSnapshot tells the primary which of its snapshots this node has
// applied. appliedAt is the PRIMARY's applied_at for that snapshot as reported
// by the publication response, not a node clock.
func (c *SnapshotClient) AcknowledgeSnapshot(ctx context.Context, primaryURL string, credential []byte, nodeID, contentHash string, appliedAt time.Time) error {
	if c == nil || c.httpClient == nil || c.canonicalURL == nil || ctx == nil || len(credential) == 0 ||
		nodeID == "" || !validSnapshotHash(contentHash) || appliedAt.IsZero() {
		return ErrSnapshotClientInvalid
	}
	if err := ctx.Err(); err != nil {
		return ErrSnapshotClientUnavailable
	}
	normalized, err := c.canonicalURL(primaryURL)
	if err != nil || !validSnapshotClientOrigin(normalized) {
		return ErrSnapshotClientInvalid
	}
	bearer, ok := snapshotBearer(credential)
	if !ok {
		return ErrSnapshotClientInvalid
	}
	payload, err := json.Marshal(snapshotAcknowledgementBody{
		NodeID: nodeID, PrimaryURL: normalized, ContentHash: contentHash,
		AppliedAt: appliedAt.UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		return ErrSnapshotClientInvalid
	}
	endpoint := strings.TrimRight(normalized, "/") + snapshotAcknowledgementHTTPPath
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return ErrSnapshotClientInvalid
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+bearer)
	c.setIngressHeaders(request)
	response, err := c.httpClient.Do(request)
	if err != nil {
		if snapshotTLSFailure(err) {
			return ErrSnapshotClientInvalid
		}
		return ErrSnapshotClientUnavailable
	}
	defer response.Body.Close()
	if response.ProtoMajor != 2 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
		return ErrSnapshotClientInvalid
	}
	if response.StatusCode == http.StatusGone {
		return gonePrimaryError(response.Body)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
	switch {
	case response.StatusCode == http.StatusNoContent || response.StatusCode == http.StatusOK:
		return nil
	case response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden:
		return ErrSnapshotClientUnauthorized
	case response.StatusCode >= 500 && response.StatusCode <= 599:
		return ErrSnapshotClientUnavailable
	default:
		return ErrSnapshotClientInvalid
	}
}

// FetchSnapshotByToken retrieves one canonical snapshot using a pcenr1 token
// in a bounded JSON POST body. The token is never placed in the URL, a log, or
// an error value.
func (c *SnapshotClient) FetchSnapshotByToken(ctx context.Context, primaryURL string, token string) ([]byte, error) {
	if c == nil || c.httpClient == nil || c.canonicalURL == nil || ctx == nil || token == "" ||
		len(token) > maxSnapshotByTokenRequestBytes {
		return nil, ErrSnapshotClientInvalid
	}
	if err := ctx.Err(); err != nil {
		return nil, ErrSnapshotClientUnavailable
	}
	normalized, err := c.canonicalURL(primaryURL)
	if err != nil || !validSnapshotClientOrigin(normalized) {
		return nil, ErrSnapshotClientInvalid
	}
	payload, err := json.Marshal(struct {
		Token string `json:"token"`
	}{Token: token})
	if err != nil || len(payload) > maxSnapshotByTokenRequestBytes {
		return nil, ErrSnapshotClientInvalid
	}
	endpoint := strings.TrimRight(normalized, "/") + snapshotByTokenHTTPPath
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, ErrSnapshotClientInvalid
	}
	request.Header.Set("Content-Type", "application/json")
	result, err := c.fetchWithRetries(request, false)
	return result.Body, err
}

func (c *SnapshotClient) fetchWithRetries(request *http.Request, allowNotModified bool) (SnapshotFetchResult, error) {
	attempts := c.retryableErrors
	if attempts <= 0 {
		attempts = 1
	}
	for attempt := 1; attempt <= attempts; attempt++ {
		if attempt > 1 && request.Body != nil {
			if request.GetBody == nil {
				return SnapshotFetchResult{}, ErrSnapshotClientUnavailable
			}
			body, err := request.GetBody()
			if err != nil {
				return SnapshotFetchResult{}, ErrSnapshotClientUnavailable
			}
			request.Body = body
		}
		response, err := c.httpClient.Do(request)
		if err != nil {
			if IsUntrustedCertificate(err) {
				return SnapshotFetchResult{}, ErrSnapshotClientUntrusted
			}
			if snapshotTLSFailure(err) {
				return SnapshotFetchResult{}, ErrSnapshotClientInvalid
			}
			if attempt == attempts {
				return SnapshotFetchResult{}, ErrSnapshotClientUnavailable
			}
			if err := c.wait(request.Context(), attempt); err != nil {
				return SnapshotFetchResult{}, ErrSnapshotClientUnavailable
			}
			continue
		}
		fetched, retry, result := c.handleSnapshotResponse(response, allowNotModified)
		if !retry {
			return fetched, result
		}
		if attempt == attempts {
			return SnapshotFetchResult{}, ErrSnapshotClientUnavailable
		}
		if err := c.wait(request.Context(), attempt); err != nil {
			return SnapshotFetchResult{}, ErrSnapshotClientUnavailable
		}
	}
	return SnapshotFetchResult{}, ErrSnapshotClientUnavailable
}

func (c *SnapshotClient) handleSnapshotResponse(response *http.Response, allowNotModified bool) (SnapshotFetchResult, bool, error) {
	if response == nil {
		return SnapshotFetchResult{}, true, ErrSnapshotClientUnavailable
	}
	if response.Body != nil {
		defer response.Body.Close()
	}
	if response.ProtoMajor == 1 || (response.ProtoMajor != 0 && response.ProtoMajor != 2) {
		return SnapshotFetchResult{}, false, ErrSnapshotClientInvalid
	}
	switch {
	case response.StatusCode >= 500 && response.StatusCode <= 599:
		return SnapshotFetchResult{}, true, ErrSnapshotClientUnavailable
	case response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden:
		return SnapshotFetchResult{}, false, ErrSnapshotClientUnauthorized
	case response.StatusCode == http.StatusGone:
		return SnapshotFetchResult{}, false, gonePrimaryError(response.Body)
	case response.StatusCode == http.StatusNotFound:
		return SnapshotFetchResult{}, false, ErrSnapshotClientNotFound
	case response.StatusCode == http.StatusNoContent:
		return SnapshotFetchResult{}, false, ErrSnapshotClientUnavailable
	case response.StatusCode == http.StatusNotModified && allowNotModified:
		meta, ok := snapshotResponseMetadata(response.Header)
		if !ok {
			return SnapshotFetchResult{}, false, ErrSnapshotClientInvalid
		}
		meta.NotModified = true
		return meta, false, nil
	case response.StatusCode != http.StatusOK:
		return SnapshotFetchResult{}, false, ErrSnapshotClientInvalid
	}
	if response.Body == nil {
		return SnapshotFetchResult{}, false, ErrSnapshotClientInvalid
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, c.maxBodyBytes+1))
	if err != nil {
		return SnapshotFetchResult{}, true, ErrSnapshotClientUnavailable
	}
	if int64(len(body)) > c.maxBodyBytes || len(body) == 0 {
		return SnapshotFetchResult{}, false, ErrSnapshotClientInvalid
	}
	result := SnapshotFetchResult{Body: body}
	// Metadata headers are optional on a full response (enrollment-time
	// fetches predate them) but must be well formed when present.
	if response.Header.Get(SnapshotContentHashHeader) != "" || response.Header.Get(SnapshotAppliedAtHeader) != "" {
		meta, ok := snapshotResponseMetadata(response.Header)
		if !ok {
			return SnapshotFetchResult{}, false, ErrSnapshotClientInvalid
		}
		result.ContentHash, result.AppliedAt = meta.ContentHash, meta.AppliedAt
	}
	return result, false, nil
}

// gonePrimaryError classifies a 410: ErrSnapshotClientUnlinked only when the
// bounded body is exactly the primary's JSON error carrying code NODE_UNLINKED;
// any other body (empty, oversized, malformed, another code) is a plain
// ErrSnapshotClientGone, so a bare revocation never reads as an unlink.
func gonePrimaryError(body io.Reader) error {
	if body == nil {
		return ErrSnapshotClientGone
	}
	raw, err := io.ReadAll(io.LimitReader(body, maxUnlinkedBodyBytes+1))
	if err != nil || len(raw) == 0 || len(raw) > maxUnlinkedBodyBytes {
		return ErrSnapshotClientGone
	}
	var parsed struct {
		Code string `json:"code"`
	}
	if json.Unmarshal(raw, &parsed) != nil || parsed.Code != NodeUnlinkedSignalCode {
		return ErrSnapshotClientGone
	}
	return ErrSnapshotClientUnlinked
}

func snapshotResponseMetadata(header http.Header) (SnapshotFetchResult, bool) {
	hash := header.Get(SnapshotContentHashHeader)
	appliedAt, err := time.Parse(time.RFC3339Nano, header.Get(SnapshotAppliedAtHeader))
	if !validSnapshotHash(hash) || err != nil || appliedAt.IsZero() {
		return SnapshotFetchResult{}, false
	}
	return SnapshotFetchResult{ContentHash: hash, AppliedAt: appliedAt.UTC()}, true
}

func (c *SnapshotClient) wait(ctx context.Context, attempt int) error {
	if c.backoff <= 0 {
		return nil
	}
	if c.sleep == nil {
		c.sleep = snapshotClientWait
	}
	return c.sleep(ctx, snapshotBackoff(c.backoff, attempt))
}

func snapshotBackoff(initial time.Duration, attempt int) time.Duration {
	if attempt <= 1 {
		return initial
	}
	result := initial
	for i := 1; i < attempt; i++ {
		if result > time.Duration(1<<63-1)/2 {
			return time.Duration(1<<63 - 1)
		}
		result *= 2
	}
	return result
}

func snapshotClientWait(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func snapshotHTTPClient(input *http.Client) *http.Client {
	if input == nil {
		return &http.Client{
			Transport:     snapshotTLS13Transport(nil),
			CheckRedirect: refuseSnapshotRedirect,
			Timeout:       15 * time.Second,
		}
	}
	client := *input
	client.Jar = nil
	client.CheckRedirect = refuseSnapshotRedirect
	client.Transport = snapshotTLS13Transport(input.Transport)
	return &client
}

func snapshotTLS13Transport(input http.RoundTripper) http.RoundTripper {
	transport, ok := input.(*http.Transport)
	if !ok {
		if input != nil {
			return input
		}
		transport = http.DefaultTransport.(*http.Transport)
	}
	clone := transport.Clone()
	config := clone.TLSClientConfig
	if config == nil {
		config = &tls.Config{}
	} else {
		config = config.Clone()
	}
	config.MinVersion = tls.VersionTLS13
	config.MaxVersion = tls.VersionTLS13
	config.NextProtos = []string{"h2"}
	clone.TLSClientConfig = config
	clone.ForceAttemptHTTP2 = true
	return clone
}

func refuseSnapshotRedirect(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

func snapshotBearer(value []byte) (string, bool) {
	candidate := string(value)
	credential, err := ParseNodeCredential(candidate)
	if err == nil {
		bearer := credential.BearerCopy()
		credential.Destroy()
		return bearer, bearer != ""
	}
	if len(value) != NodeCredentialSecretBytes {
		return "", false
	}
	return base64.RawURLEncoding.EncodeToString(value), true
}

func canonicalizeSnapshotPrimaryURL(raw string) (string, error) {
	if raw == "" || len(raw) > 2048 || strings.TrimSpace(raw) != raw {
		return "", ErrSnapshotClientInvalid
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed == nil || !strings.EqualFold(parsed.Scheme, "https") || parsed.Opaque != "" || parsed.Host == "" || parsed.User != nil {
		return "", ErrSnapshotClientInvalid
	}
	if parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.RawFragment != "" || strings.Contains(raw, "#") {
		return "", ErrSnapshotClientInvalid
	}
	if (parsed.Path != "" && parsed.Path != "/") || parsed.RawPath != "" {
		return "", ErrSnapshotClientInvalid
	}
	host, portText, err := net.SplitHostPort(parsed.Host)
	if err != nil || host == "" || portText == "" || (strings.HasPrefix(parsed.Host, "[") && !strings.Contains(host, ":")) {
		return "", ErrSnapshotClientInvalid
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil || port == 0 {
		return "", ErrSnapshotClientInvalid
	}
	canonicalHost, err := canonicalizeSnapshotHost(host)
	if err != nil {
		return "", ErrSnapshotClientInvalid
	}
	return (&url.URL{Scheme: "https", Host: net.JoinHostPort(canonicalHost, strconv.Itoa(int(port))), Path: "/"}).String(), nil
}

func canonicalizeSnapshotHost(raw string) (string, error) {
	if raw == "" || strings.TrimSpace(raw) != raw || strings.Contains(raw, "%") {
		return "", ErrSnapshotClientInvalid
	}
	if ip := net.ParseIP(raw); ip != nil {
		if strings.Contains(raw, ":") && ip.To4() != nil {
			return "", ErrSnapshotClientInvalid
		}
		return ip.String(), nil
	}
	canonical := strings.TrimSuffix(strings.ToLower(raw), ".")
	if canonical == "" || len(canonical) > 253 {
		return "", ErrSnapshotClientInvalid
	}
	labels := strings.Split(canonical, ".")
	if len(labels) == 4 && snapshotAllDecimalLabels(labels) {
		return "", ErrSnapshotClientInvalid
	}
	for _, label := range labels {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", ErrSnapshotClientInvalid
		}
		for i := 0; i < len(label); i++ {
			if (label[i] < 'a' || label[i] > 'z') && (label[i] < '0' || label[i] > '9') && label[i] != '-' {
				return "", ErrSnapshotClientInvalid
			}
		}
	}
	return canonical, nil
}

func snapshotAllDecimalLabels(labels []string) bool {
	for _, label := range labels {
		for i := 0; i < len(label); i++ {
			if label[i] < '0' || label[i] > '9' {
				return false
			}
		}
	}
	return true
}

func validSnapshotClientOrigin(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && parsed != nil && strings.EqualFold(parsed.Scheme, "https") &&
		parsed.Host != "" && parsed.User == nil && parsed.RawQuery == "" &&
		!parsed.ForceQuery && parsed.Fragment == "" && parsed.RawFragment == "" &&
		(parsed.Path == "" || parsed.Path == "/") && parsed.RawPath == ""
}

func snapshotTLSFailure(err error) bool {
	if err == nil {
		return false
	}
	var alert tls.AlertError
	var record tls.RecordHeaderError
	var certificate x509.CertificateInvalidError
	if errors.As(err, &alert) || errors.As(err, &record) || errors.As(err, &certificate) {
		return true
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "tls") ||
		strings.Contains(message, "protocol version") ||
		strings.Contains(message, "first record does not look like a tls handshake")
}
