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
)

var (
	ErrSnapshotClientUnauthorized = errors.New("snapshot client unauthorized")
	ErrSnapshotClientGone         = errors.New("snapshot client credential gone")
	ErrSnapshotClientNotFound     = errors.New("snapshot client snapshot not found")
	ErrSnapshotClientUnavailable  = errors.New("snapshot client unavailable")
	ErrSnapshotClientInvalid      = errors.New("snapshot client response invalid")
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
}

type SnapshotClientOptions struct {
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
	return c.fetchWithRetries(request)
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
	return c.fetchWithRetries(request)
}

func (c *SnapshotClient) fetchWithRetries(request *http.Request) ([]byte, error) {
	attempts := c.retryableErrors
	if attempts <= 0 {
		attempts = 1
	}
	for attempt := 1; attempt <= attempts; attempt++ {
		if attempt > 1 && request.Body != nil {
			if request.GetBody == nil {
				return nil, ErrSnapshotClientUnavailable
			}
			body, err := request.GetBody()
			if err != nil {
				return nil, ErrSnapshotClientUnavailable
			}
			request.Body = body
		}
		response, err := c.httpClient.Do(request)
		if err != nil {
			if snapshotTLSFailure(err) {
				return nil, ErrSnapshotClientInvalid
			}
			if attempt == attempts {
				return nil, ErrSnapshotClientUnavailable
			}
			if err := c.wait(request.Context(), attempt); err != nil {
				return nil, ErrSnapshotClientUnavailable
			}
			continue
		}
		body, retry, result := c.handleSnapshotResponse(response)
		if !retry {
			return body, result
		}
		if attempt == attempts {
			return nil, ErrSnapshotClientUnavailable
		}
		if err := c.wait(request.Context(), attempt); err != nil {
			return nil, ErrSnapshotClientUnavailable
		}
	}
	return nil, ErrSnapshotClientUnavailable
}

func (c *SnapshotClient) handleSnapshotResponse(response *http.Response) ([]byte, bool, error) {
	if response == nil {
		return nil, true, ErrSnapshotClientUnavailable
	}
	if response.Body != nil {
		defer response.Body.Close()
	}
	if response.ProtoMajor == 1 || (response.ProtoMajor != 0 && response.ProtoMajor != 2) {
		return nil, false, ErrSnapshotClientInvalid
	}
	switch {
	case response.StatusCode >= 500 && response.StatusCode <= 599:
		return nil, true, ErrSnapshotClientUnavailable
	case response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden:
		return nil, false, ErrSnapshotClientUnauthorized
	case response.StatusCode == http.StatusGone:
		return nil, false, ErrSnapshotClientGone
	case response.StatusCode == http.StatusNotFound:
		return nil, false, ErrSnapshotClientNotFound
	case response.StatusCode == http.StatusNoContent:
		return nil, false, ErrSnapshotClientUnavailable
	case response.StatusCode != http.StatusOK:
		return nil, false, ErrSnapshotClientInvalid
	}
	if response.Body == nil {
		return nil, false, ErrSnapshotClientInvalid
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, c.maxBodyBytes+1))
	if err != nil {
		return nil, true, ErrSnapshotClientUnavailable
	}
	if int64(len(body)) > c.maxBodyBytes || len(body) == 0 {
		return nil, false, ErrSnapshotClientInvalid
	}
	return body, false, nil
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
