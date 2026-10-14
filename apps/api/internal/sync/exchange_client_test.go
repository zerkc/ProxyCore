package sync

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const exchangeTestToken = "pcenr1_selectorselector_secretsecretsecretsecretsecretsecret"

func exchangeTestRequest() ExchangeRequest {
	return ExchangeRequest{
		AttemptID:             "attempt-1",
		InstallationID:        "11111111-1111-4111-8111-111111111111",
		NodeID:                "22222222-2222-4222-8222-222222222222",
		RecipientPublicKey:    json.RawMessage(`{"version":1,"key":"AAAA"}`),
		PreviewDigest:         strings.Repeat("ab", 32),
		PrimaryInstallationID: "33333333-3333-4333-8333-333333333333",
		LeadershipGeneration:  7,
	}
}

func startExchangeServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, string) {
	t.Helper()
	server := httptest.NewUnstartedServer(handler)
	server.EnableHTTP2 = true
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS13}
	server.StartTLS()
	t.Cleanup(server.Close)
	leaf, err := x509.ParseCertificate(server.Certificate().Raw)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
	return server, hex.EncodeToString(sum[:])
}

func TestExchangeClientSuccess(t *testing.T) {
	expires := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	var gotAuth, gotPath, gotMethod, gotContentType string
	var gotBody ExchangeRequest
	server, pin := startExchangeServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath, gotMethod, gotContentType = r.URL.RequestURI(), r.Method, r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"sealedBootstrapPayload":"c2VhbGVk","payloadHash":"abc","expiresAt":"` + expires.Format(time.RFC3339) + `"}`))
	})
	client := NewExchangeClient(ExchangeClientOptions{HTTPClient: server.Client(), PinnedSPKISHA256: pin})
	got, err := client.Exchange(context.Background(), server.URL, exchangeTestToken, exchangeTestRequest())
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if string(got.SealedPayload) != "sealed" || got.PayloadHash != "abc" || !got.ExpiresAt.Equal(expires) {
		t.Fatalf("result = %+v", got)
	}
	if gotAuth != "Enrollment "+exchangeTestToken || gotPath != "/api/topology/enroll" || gotMethod != http.MethodPost || gotContentType != "application/json" {
		t.Fatalf("request auth/path/method/type = %q %q %q %q", "<redacted>", gotPath, gotMethod, gotContentType)
	}
	if gotBody.AttemptID != "attempt-1" || gotBody.LeadershipGeneration != 7 || gotBody.PreviewDigest != exchangeTestRequest().PreviewDigest {
		t.Fatalf("body = %+v", gotBody)
	}
}

func TestExchangeClientDeniedUniformly(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusGone, http.StatusNotFound} {
		server, pin := startExchangeServer(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":"ENROLLMENT_DENIED","detail":"secret-server-detail"}`))
		})
		client := NewExchangeClient(ExchangeClientOptions{HTTPClient: server.Client(), PinnedSPKISHA256: pin})
		_, err := client.Exchange(context.Background(), server.URL, exchangeTestToken, exchangeTestRequest())
		if !errors.Is(err, ErrExchangeDenied) {
			t.Fatalf("status %d: err = %v, want ErrExchangeDenied", status, err)
		}
		if strings.Contains(err.Error(), "secret-server-detail") || strings.Contains(err.Error(), exchangeTestToken) {
			t.Fatalf("error leaks data: %v", err)
		}
	}
}

func TestExchangeClientRateLimitedParsesAndBoundsRetryAfter(t *testing.T) {
	cases := map[string]time.Duration{"7": 7 * time.Second, "999999": MaxExchangeRetryAfter, "": 0, "garbage": 0, "-3": 0}
	for header, want := range cases {
		server, pin := startExchangeServer(t, func(w http.ResponseWriter, r *http.Request) {
			if header != "" {
				w.Header().Set("Retry-After", header)
			}
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"ENROLLMENT_RATE_LIMITED"}`))
		})
		client := NewExchangeClient(ExchangeClientOptions{HTTPClient: server.Client(), PinnedSPKISHA256: pin})
		_, err := client.Exchange(context.Background(), server.URL, exchangeTestToken, exchangeTestRequest())
		var limited *ExchangeRateLimitedError
		if !errors.Is(err, ErrExchangeRateLimited) || !errors.As(err, &limited) {
			t.Fatalf("Retry-After %q: err = %v", header, err)
		}
		if limited.RetryAfter != want {
			t.Fatalf("Retry-After %q: got %v want %v", header, limited.RetryAfter, want)
		}
	}
}

func TestExchangeClientRejectsOversizeResponse(t *testing.T) {
	server, pin := startExchangeServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(strings.Repeat("x", 4096)))
	})
	client := NewExchangeClient(ExchangeClientOptions{HTTPClient: server.Client(), PinnedSPKISHA256: pin, MaxBodyBytes: 1024})
	_, err := client.Exchange(context.Background(), server.URL, exchangeTestToken, exchangeTestRequest())
	if !errors.Is(err, ErrExchangeInvalid) {
		t.Fatalf("err = %v, want ErrExchangeInvalid", err)
	}
}

func TestExchangeClientRefusesRedirect(t *testing.T) {
	var followed bool
	server, pin := startExchangeServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/elsewhere" {
			followed = true
			return
		}
		http.Redirect(w, r, "/elsewhere", http.StatusTemporaryRedirect)
	})
	client := NewExchangeClient(ExchangeClientOptions{HTTPClient: server.Client(), PinnedSPKISHA256: pin})
	_, err := client.Exchange(context.Background(), server.URL, exchangeTestToken, exchangeTestRequest())
	if err == nil || followed {
		t.Fatalf("redirect followed=%v err=%v", followed, err)
	}
	if errors.Is(err, ErrExchangeDenied) || errors.Is(err, ErrExchangeRateLimited) {
		t.Fatalf("redirect must be invalid, got %v", err)
	}
}

func TestExchangeClientRejectsWrongSPKIPin(t *testing.T) {
	var served bool
	server, _ := startExchangeServer(t, func(w http.ResponseWriter, r *http.Request) { served = true })
	client := NewExchangeClient(ExchangeClientOptions{HTTPClient: server.Client(), PinnedSPKISHA256: strings.Repeat("0", 64)})
	_, err := client.Exchange(context.Background(), server.URL, exchangeTestToken, exchangeTestRequest())
	if err == nil || served {
		t.Fatalf("pin mismatch served=%v err=%v", served, err)
	}
	if strings.Contains(err.Error(), exchangeTestToken) {
		t.Fatalf("error leaks token: %v", err)
	}
}

func TestExchangeClientRejectsUntrustedCertificate(t *testing.T) {
	var served bool
	server, pin := startExchangeServer(t, func(w http.ResponseWriter, r *http.Request) { served = true })
	// A default client does not trust the httptest CA even with a matching pin.
	client := NewExchangeClient(ExchangeClientOptions{PinnedSPKISHA256: pin})
	_, err := client.Exchange(context.Background(), server.URL, exchangeTestToken, exchangeTestRequest())
	if !errors.Is(err, ErrExchangeUnavailable) || served {
		t.Fatalf("served=%v err=%v", served, err)
	}
}

func TestExchangeClientInvalidInputsNeverReachTransport(t *testing.T) {
	client := NewExchangeClient(ExchangeClientOptions{
		HTTPClient: &http.Client{Transport: snapshotRoundTripper(func(*http.Request) (*http.Response, error) {
			t.Fatal("transport must not be called")
			return nil, nil
		})},
	})
	cases := map[string]func() error{
		"empty token": func() error {
			_, err := client.Exchange(context.Background(), "https://primary.example:3443/", "", exchangeTestRequest())
			return err
		},
		"http url": func() error {
			_, err := client.Exchange(context.Background(), "http://primary.example:3443/", exchangeTestToken, exchangeTestRequest())
			return err
		},
		"header injection token": func() error {
			_, err := client.Exchange(context.Background(), "https://primary.example:3443/", "tok\r\nX: y", exchangeTestRequest())
			return err
		},
	}
	for name, run := range cases {
		if err := run(); !errors.Is(err, ErrExchangeInvalid) {
			t.Fatalf("%s: err = %v, want ErrExchangeInvalid", name, err)
		}
	}
}

func TestExchangeClientServerErrorIsUnavailableWithoutRetry(t *testing.T) {
	var calls int
	server, pin := startExchangeServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadGateway)
	})
	client := NewExchangeClient(ExchangeClientOptions{HTTPClient: server.Client(), PinnedSPKISHA256: pin})
	_, err := client.Exchange(context.Background(), server.URL, exchangeTestToken, exchangeTestRequest())
	if !errors.Is(err, ErrExchangeUnavailable) || calls != 1 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}

func TestExchangeClientHonoursContextDeadline(t *testing.T) {
	release := make(chan struct{})
	server, pin := startExchangeServer(t, func(w http.ResponseWriter, r *http.Request) { <-release })
	defer close(release)
	client := NewExchangeClient(ExchangeClientOptions{HTTPClient: server.Client(), PinnedSPKISHA256: pin})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := client.Exchange(ctx, server.URL, exchangeTestToken, exchangeTestRequest())
	if !errors.Is(err, ErrExchangeUnavailable) {
		t.Fatalf("err = %v, want ErrExchangeUnavailable", err)
	}
}

func TestExchangeClientPinWithoutHTTPTransportFailsClosed(t *testing.T) {
	client := NewExchangeClient(ExchangeClientOptions{
		HTTPClient: &http.Client{Transport: snapshotRoundTripper(func(*http.Request) (*http.Response, error) {
			t.Fatal("transport must not be called when the pin cannot be enforced")
			return nil, nil
		})},
		PinnedSPKISHA256: strings.Repeat("a", 64),
	})
	_, err := client.Exchange(context.Background(), "https://primary.example:3443/", exchangeTestToken, exchangeTestRequest())
	if !errors.Is(err, ErrExchangeInvalid) {
		t.Fatalf("err = %v, want ErrExchangeInvalid", err)
	}
}

// A v2 enrollment token (URL + CA) is several KiB and is deliberately never
// sent in the Authorization header: the node downgrades it to the 83-byte v1
// spelling of selector+secret before any request to the primary.
func TestValidExchangeTokenBounds(t *testing.T) {
	v1 := "pcenr1_" + strings.Repeat("ab", 16) + "_" + strings.Repeat("A", 43)
	for name, tc := range map[string]struct {
		token string
		want  bool
	}{
		"v1 token":         {v1, true},
		"at the bound":     {strings.Repeat("a", exchangeMaxTokenBytes), true},
		"one over":         {strings.Repeat("a", exchangeMaxTokenBytes+1), false},
		"v2 sized token":   {"pcenr2_" + strings.Repeat("A", 4000), false},
		"short":            {"pcenr1_ab", true},
		"embedded newline": {"pcenr1_a\nb", false},
		"empty":            {"", false},
	} {
		if got := validExchangeToken(tc.token); got != tc.want {
			t.Fatalf("%s: validExchangeToken = %v, want %v", name, got, tc.want)
		}
	}
}
