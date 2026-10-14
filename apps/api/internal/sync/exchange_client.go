package sync

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	// ExchangeHTTPPath is the primary's token-authenticated grant exchange route.
	ExchangeHTTPPath = "/api/topology/enroll"
	// MaxExchangeRetryAfter bounds a server-supplied Retry-After so a hostile
	// primary cannot park a node indefinitely.
	MaxExchangeRetryAfter = 15 * time.Minute

	exchangeAuthorizationScheme = "Enrollment"
	exchangeMaxTokenBytes       = 512
	exchangeMaxRequestBytes     = 8 << 10
	exchangeDefaultMaxBody      = 64 << 10
)

var (
	// ErrExchangeDenied is the uniform outcome for every primary-side denial.
	ErrExchangeDenied = errors.New("enrollment exchange denied")
	// ErrExchangeRateLimited is wrapped by *ExchangeRateLimitedError.
	ErrExchangeRateLimited = errors.New("enrollment exchange rate limited")
	// ErrExchangeUnavailable covers transport, TLS, pin, timeout and 5xx failures.
	ErrExchangeUnavailable = errors.New("enrollment exchange unavailable")
	// ErrExchangeInvalid covers bad local input and malformed or oversized responses.
	ErrExchangeInvalid = errors.New("enrollment exchange invalid")
)

// ExchangeRateLimitedError carries the bounded Retry-After of a 429.
type ExchangeRateLimitedError struct {
	RetryAfter time.Duration
}

func (e *ExchangeRateLimitedError) Error() string { return ErrExchangeRateLimited.Error() }

func (e *ExchangeRateLimitedError) Is(target error) bool { return target == ErrExchangeRateLimited }

// ExchangeRequest is the JSON body of POST /api/topology/enroll.
// RecipientPublicKey is the already-marshalled bootstrap recipient public key
// (enrollment.BootstrapRecipientPublicKey.Wire()), embedded verbatim.
type ExchangeRequest struct {
	AttemptID             string          `json:"attemptId"`
	InstallationID        string          `json:"installationId"`
	NodeID                string          `json:"nodeId"`
	RecipientPublicKey    json.RawMessage `json:"recipientPublicKey"`
	PreviewDigest         string          `json:"previewDigest"`
	PrimaryInstallationID string          `json:"primaryInstallationId"`
	LeadershipGeneration  uint64          `json:"leadershipGeneration"`
}

// ExchangeResult is the sealed grant returned by the primary. SealedPayload is
// ciphertext addressed to the node's ephemeral recipient key.
type ExchangeResult struct {
	SealedPayload []byte
	PayloadHash   string
	ExpiresAt     time.Time
}

type exchangeResponseWire struct {
	SealedBootstrapPayload []byte    `json:"sealedBootstrapPayload"`
	PayloadHash            string    `json:"payloadHash"`
	ExpiresAt              time.Time `json:"expiresAt"`
}

// ExchangeClient is the NODE-side adapter for the primary's grant exchange. It
// reuses SnapshotClient's TLS 1.3 / h2-only transport constraints and URL
// canonicalization, performs exactly one attempt (the single-use token must not
// be replayed blindly) and exposes only denied / rate-limited / unavailable /
// invalid outcomes with no server-supplied text.
type ExchangeClient struct {
	httpClient   *http.Client
	maxBodyBytes int64
}

type ExchangeClientOptions struct {
	// HTTPClient is the trust seam (RootCAs for the installation CA). Its
	// transport is cloned and constrained exactly like SnapshotClient's.
	HTTPClient *http.Client
	// PinnedSPKISHA256 is the lowercase hex SHA-256 of the primary leaf's
	// SubjectPublicKeyInfo confirmed during preview. It is checked in addition
	// to normal chain/hostname verification, never instead of it.
	PinnedSPKISHA256 string
	MaxBodyBytes     int64
}

func NewExchangeClient(opts ExchangeClientOptions) *ExchangeClient {
	max := opts.MaxBodyBytes
	if max <= 0 {
		max = exchangeDefaultMaxBody
	}
	client := snapshotHTTPClient(opts.HTTPClient)
	if opts.PinnedSPKISHA256 != "" {
		client = pinExchangeClient(client, opts.PinnedSPKISHA256)
	}
	return &ExchangeClient{httpClient: client, maxBodyBytes: max}
}

// pinExchangeClient adds the SPKI pin to the (already cloned) transport. A pin
// that cannot be enforced fails closed: the returned client has no transport.
func pinExchangeClient(client *http.Client, pin string) *http.Client {
	want, err := hex.DecodeString(pin)
	transport, ok := client.Transport.(*http.Transport)
	if err != nil || len(want) != sha256.Size || !ok || transport.TLSClientConfig == nil {
		return nil
	}
	config := transport.TLSClientConfig.Clone()
	config.VerifyConnection = func(state tls.ConnectionState) error {
		if len(state.PeerCertificates) == 0 {
			return errors.New("tls: missing peer certificate")
		}
		sum := sha256.Sum256(state.PeerCertificates[0].RawSubjectPublicKeyInfo)
		if subtle.ConstantTimeCompare(sum[:], want) != 1 {
			return errors.New("tls: pinned key mismatch")
		}
		return nil
	}
	transport.TLSClientConfig = config
	return client
}

// Exchange POSTs the request with the enrollment token in the Authorization
// header only. The token is never logged, placed in a URL or echoed in errors.
func (c *ExchangeClient) Exchange(ctx context.Context, primaryURL, token string, request ExchangeRequest) (ExchangeResult, error) {
	if c == nil || c.httpClient == nil || ctx == nil || !validExchangeToken(token) {
		return ExchangeResult{}, ErrExchangeInvalid
	}
	if err := ctx.Err(); err != nil {
		return ExchangeResult{}, ErrExchangeUnavailable
	}
	normalized, err := canonicalizeSnapshotPrimaryURL(primaryURL)
	if err != nil || !validSnapshotClientOrigin(normalized) {
		return ExchangeResult{}, ErrExchangeInvalid
	}
	payload, err := json.Marshal(request)
	if err != nil || len(payload) > exchangeMaxRequestBytes || len(request.RecipientPublicKey) == 0 {
		return ExchangeResult{}, ErrExchangeInvalid
	}
	endpoint := strings.TrimRight(normalized, "/") + ExchangeHTTPPath
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return ExchangeResult{}, ErrExchangeInvalid
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Authorization", exchangeAuthorizationScheme+" "+token)
	response, err := c.httpClient.Do(httpRequest)
	if err != nil {
		// url.Error embeds the URL, never the header, but is discarded anyway.
		return ExchangeResult{}, ErrExchangeUnavailable
	}
	defer response.Body.Close()
	if response.ProtoMajor != 2 {
		return ExchangeResult{}, ErrExchangeInvalid
	}
	switch {
	case response.StatusCode == http.StatusTooManyRequests:
		return ExchangeResult{}, &ExchangeRateLimitedError{RetryAfter: parseExchangeRetryAfter(response.Header.Get("Retry-After"))}
	case response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden ||
		response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusGone:
		return ExchangeResult{}, ErrExchangeDenied
	case response.StatusCode >= 500 && response.StatusCode <= 599:
		return ExchangeResult{}, ErrExchangeUnavailable
	case response.StatusCode != http.StatusCreated:
		return ExchangeResult{}, ErrExchangeInvalid
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, c.maxBodyBytes+1))
	if err != nil {
		return ExchangeResult{}, ErrExchangeUnavailable
	}
	if int64(len(body)) > c.maxBodyBytes || len(body) == 0 {
		return ExchangeResult{}, ErrExchangeInvalid
	}
	var wire exchangeResponseWire
	if err := json.Unmarshal(body, &wire); err != nil || len(wire.SealedBootstrapPayload) == 0 ||
		wire.PayloadHash == "" || wire.ExpiresAt.IsZero() {
		return ExchangeResult{}, ErrExchangeInvalid
	}
	return ExchangeResult{
		SealedPayload: wire.SealedBootstrapPayload,
		PayloadHash:   wire.PayloadHash,
		ExpiresAt:     wire.ExpiresAt.UTC(),
	}, nil
}

func validExchangeToken(token string) bool {
	if token == "" || len(token) > exchangeMaxTokenBytes {
		return false
	}
	for i := 0; i < len(token); i++ {
		if token[i] <= 0x20 || token[i] >= 0x7f {
			return false
		}
	}
	return true
}

func parseExchangeRetryAfter(value string) time.Duration {
	seconds, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || seconds <= 0 {
		return 0
	}
	if seconds > int64(MaxExchangeRetryAfter/time.Second) {
		return MaxExchangeRetryAfter
	}
	return time.Duration(seconds) * time.Second
}
