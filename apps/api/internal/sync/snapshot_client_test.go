package sync

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSnapshotClientRejectsCanonicalizationFailure(t *testing.T) {
	client := NewSnapshotClient(SnapshotClientOptions{
		HTTPClient: &http.Client{Transport: snapshotRoundTripper(func(*http.Request) (*http.Response, error) {
			t.Fatal("transport should not be called")
			return nil, nil
		})},
	})
	client.canonicalURL = func(string) (string, error) { return "", errors.New("canonicalization failed") }
	_, err := client.FetchSnapshot(context.Background(), "https://primary.example:3443/", []byte("credential"))
	if !errors.Is(err, ErrSnapshotClientInvalid) {
		t.Fatalf("FetchSnapshot error=%v, want ErrSnapshotClientInvalid", err)
	}
}

func TestSnapshotClientSendsCanonicalBearer(t *testing.T) {
	credential, err := NewNodeCredential("11112222-3333-4444-8999-aabbccddeeff", bytes.Repeat([]byte{0x19}, NodeCredentialSecretBytes))
	if err != nil {
		t.Fatal(err)
	}
	defer credential.Destroy()
	var authorization string
	client := NewSnapshotClient(SnapshotClientOptions{
		HTTPClient: &http.Client{Transport: snapshotRoundTripper(func(request *http.Request) (*http.Response, error) {
			authorization = request.Header.Get("Authorization")
			return snapshotResponse(http.StatusOK, []byte("snapshot")), nil
		})},
	})
	if _, err := client.FetchSnapshot(context.Background(), "https://primary.example:3443/", []byte(credential.BearerCopy())); err != nil {
		t.Fatalf("FetchSnapshot: %v", err)
	}
	if authorization != "Bearer "+credential.BearerCopy() {
		t.Fatalf("authorization=%q", redactAuthorization(authorization))
	}
}

func TestSnapshotClientRetriesServerErrors(t *testing.T) {
	var calls atomic.Int32
	client := NewSnapshotClient(SnapshotClientOptions{
		HTTPClient: &http.Client{Transport: snapshotRoundTripper(func(*http.Request) (*http.Response, error) {
			if calls.Add(1) == 1 {
				return snapshotResponse(http.StatusBadGateway, nil), nil
			}
			return snapshotResponse(http.StatusOK, []byte("snapshot")), nil
		})},
		MaxAttempts:    3,
		InitialBackoff: time.Nanosecond,
	})
	client.sleep = func(context.Context, time.Duration) error { return nil }
	body, err := client.FetchSnapshot(context.Background(), "https://primary.example:3443/", bytes.Repeat([]byte{0x42}, NodeCredentialSecretBytes))
	if err != nil || string(body) != "snapshot" || calls.Load() != 2 {
		t.Fatalf("body=%q err=%v calls=%d", body, err, calls.Load())
	}
}

func TestSnapshotClientDoesNotRetryForbidden(t *testing.T) {
	var calls atomic.Int32
	client := NewSnapshotClient(SnapshotClientOptions{
		HTTPClient: &http.Client{Transport: snapshotRoundTripper(func(*http.Request) (*http.Response, error) {
			calls.Add(1)
			return snapshotResponse(http.StatusForbidden, []byte("credential must not escape")), nil
		})},
		MaxAttempts:    3,
		InitialBackoff: time.Nanosecond,
	})
	_, err := client.FetchSnapshot(context.Background(), "https://primary.example:3443/", bytes.Repeat([]byte{0x42}, NodeCredentialSecretBytes))
	if !errors.Is(err, ErrSnapshotClientUnauthorized) || calls.Load() != 1 {
		t.Fatalf("err=%v calls=%d", err, calls.Load())
	}
}

func TestSnapshotClientCapsBodyAndRedactsCredentialErrors(t *testing.T) {
	secret := []byte("credential-secret-canary")
	client := NewSnapshotClient(SnapshotClientOptions{
		HTTPClient: &http.Client{Transport: snapshotRoundTripper(func(*http.Request) (*http.Response, error) {
			return snapshotResponse(http.StatusOK, []byte("0123456789")), nil
		})},
		MaxBodyBytes: 4,
	})
	_, err := client.FetchSnapshot(context.Background(), "https://primary.example:3443/", secret)
	if !errors.Is(err, ErrSnapshotClientInvalid) {
		t.Fatalf("body cap error=%v, want ErrSnapshotClientInvalid", err)
	}
	_, err = client.FetchSnapshot(context.Background(), "https://primary.example:3443/", secret)
	if strings.Contains(err.Error(), string(secret)) {
		t.Fatalf("credential appeared in error: %v", err)
	}
}

func TestSnapshotClientExhaustedServerErrorsAreUnavailable(t *testing.T) {
	var calls atomic.Int32
	client := NewSnapshotClient(SnapshotClientOptions{
		HTTPClient: &http.Client{Transport: snapshotRoundTripper(func(*http.Request) (*http.Response, error) {
			calls.Add(1)
			return snapshotResponse(http.StatusInternalServerError, nil), nil
		})},
		MaxAttempts:    2,
		InitialBackoff: time.Nanosecond,
	})
	client.sleep = func(context.Context, time.Duration) error { return nil }
	_, err := client.FetchSnapshot(context.Background(), "https://primary.example:3443/", bytes.Repeat([]byte{0x42}, NodeCredentialSecretBytes))
	if !errors.Is(err, ErrSnapshotClientUnavailable) || calls.Load() != 2 {
		t.Fatalf("err=%v calls=%d", err, calls.Load())
	}
}

func TestSnapshotClientRejectsTLS12(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS12, MaxVersion: tls.VersionTLS12}
	server.StartTLS()
	defer server.Close()
	client := NewSnapshotClient(SnapshotClientOptions{HTTPClient: server.Client(), MaxAttempts: 1})
	_, err := client.FetchSnapshot(context.Background(), server.URL+"/", bytes.Repeat([]byte{0x42}, NodeCredentialSecretBytes))
	if !errors.Is(err, ErrSnapshotClientInvalid) {
		t.Fatalf("TLS downgrade error=%v, want ErrSnapshotClientInvalid", err)
	}
}

func TestSnapshotClientFetchSnapshotByTokenPostsBoundedJSON(t *testing.T) {
	token := "pcenr1_selector_secret"
	var method, path, contentType string
	var body []byte
	client := NewSnapshotClient(SnapshotClientOptions{
		HTTPClient: &http.Client{Transport: snapshotRoundTripper(func(request *http.Request) (*http.Response, error) {
			method, path, contentType = request.Method, request.URL.Path, request.Header.Get("Content-Type")
			body, _ = io.ReadAll(request.Body)
			return snapshotResponse(http.StatusOK, []byte("snapshot")), nil
		})},
	})
	got, err := client.FetchSnapshotByToken(context.Background(), "https://PRIMARY.example:3443/", token)
	if err != nil || string(got) != "snapshot" {
		t.Fatalf("body=%q err=%v", got, err)
	}
	if method != http.MethodPost || path != snapshotByTokenHTTPPath || contentType != "application/json" ||
		string(body) != `{"token":"`+token+`"}` {
		t.Fatalf("request method=%q path=%q content-type=%q bodyLen=%d bodyHash=%x", method, path, contentType, len(body), sha256.Sum256(body))
	}
}

func TestSnapshotClientFetchSnapshotByTokenRejectsCanonicalURLAndRequestBodyOverflow(t *testing.T) {
	var calls atomic.Int32
	client := NewSnapshotClient(SnapshotClientOptions{
		HTTPClient: &http.Client{Transport: snapshotRoundTripper(func(*http.Request) (*http.Response, error) {
			calls.Add(1)
			return snapshotResponse(http.StatusOK, []byte("unexpected")), nil
		})},
	})
	client.canonicalURL = func(string) (string, error) { return "", errors.New("canonicalization failed") }
	if _, err := client.FetchSnapshotByToken(context.Background(), "https://primary.example:3443/", "token"); !errors.Is(err, ErrSnapshotClientInvalid) {
		t.Fatalf("canonical URL error=%v", err)
	}
	client.canonicalURL = canonicalizeSnapshotPrimaryURL
	secret := strings.Repeat("token", maxSnapshotByTokenRequestBytes)
	if _, err := client.FetchSnapshotByToken(context.Background(), "https://primary.example:3443/", secret); !errors.Is(err, ErrSnapshotClientInvalid) {
		t.Fatalf("body overflow error=%v", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("transport calls=%d, want 0", calls.Load())
	}
}

func TestSnapshotClientFetchSnapshotByTokenRetriesWithFreshBody(t *testing.T) {
	var calls atomic.Int32
	var bodies [][]byte
	token := "pcenr1_selector_secret"
	client := NewSnapshotClient(SnapshotClientOptions{
		HTTPClient: &http.Client{Transport: snapshotRoundTripper(func(request *http.Request) (*http.Response, error) {
			body, _ := io.ReadAll(request.Body)
			bodies = append(bodies, body)
			if calls.Add(1) == 1 {
				return snapshotResponse(http.StatusBadGateway, nil), nil
			}
			return snapshotResponse(http.StatusOK, []byte("snapshot")), nil
		})},
		MaxAttempts: 3, InitialBackoff: time.Nanosecond,
	})
	client.sleep = func(context.Context, time.Duration) error { return nil }
	if _, err := client.FetchSnapshotByToken(context.Background(), "https://primary.example:3443/", token); err != nil {
		t.Fatalf("FetchSnapshotByToken: %v", err)
	}
	if calls.Load() != 2 || len(bodies) != 2 || !bytes.Equal(bodies[0], bodies[1]) {
		t.Fatalf("calls=%d bodyCount=%d bodyLens=%v firstHash=%x secondHash=%x", calls.Load(), len(bodies), []int{len(bodies[0]), len(bodies[1])}, sha256.Sum256(bodies[0]), sha256.Sum256(bodies[1]))
	}
}

func TestSnapshotClientFetchSnapshotByTokenMapsTerminalStatusesWithoutRetry(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		want   error
	}{
		{name: "no content", status: http.StatusNoContent, want: ErrSnapshotClientUnavailable},
		{name: "forbidden", status: http.StatusForbidden, want: ErrSnapshotClientUnauthorized},
		{name: "gone", status: http.StatusGone, want: ErrSnapshotClientGone},
		{name: "method", status: http.StatusMethodNotAllowed, want: ErrSnapshotClientInvalid},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			client := NewSnapshotClient(SnapshotClientOptions{
				HTTPClient: &http.Client{Transport: snapshotRoundTripper(func(*http.Request) (*http.Response, error) {
					calls.Add(1)
					return snapshotResponse(test.status, nil), nil
				})},
				MaxAttempts: 3, InitialBackoff: time.Nanosecond,
			})
			if _, err := client.FetchSnapshotByToken(context.Background(), "https://primary.example:3443/", "token"); !errors.Is(err, test.want) || calls.Load() != 1 {
				t.Fatalf("error=%v calls=%d want=%v/1", err, calls.Load(), test.want)
			}
		})
	}
}

func TestSnapshotClientFetchSnapshotByTokenExhaustsServerErrorsWithoutTokenLeak(t *testing.T) {
	token := "pcenr1_secret-canary"
	var calls atomic.Int32
	client := NewSnapshotClient(SnapshotClientOptions{
		HTTPClient: &http.Client{Transport: snapshotRoundTripper(func(*http.Request) (*http.Response, error) {
			calls.Add(1)
			return snapshotResponse(http.StatusInternalServerError, []byte(token)), nil
		})},
		MaxAttempts: 2, InitialBackoff: time.Nanosecond,
	})
	client.sleep = func(context.Context, time.Duration) error { return nil }
	_, err := client.FetchSnapshotByToken(context.Background(), "https://primary.example:3443/", token)
	if !errors.Is(err, ErrSnapshotClientUnavailable) || calls.Load() != 2 || strings.Contains(err.Error(), token) {
		t.Fatalf("error=%v calls=%d", err, calls.Load())
	}
}

type snapshotRoundTripper func(*http.Request) (*http.Response, error)

func (f snapshotRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func redactAuthorization(value string) string {
	const prefix = "Bearer "
	if !strings.HasPrefix(value, prefix) {
		return "<no-bearer>"
	}
	secret := strings.TrimPrefix(value, prefix)
	if len(secret) <= 6 {
		return prefix + "<redacted>"
	}
	return prefix + secret[:3] + "..." + secret[len(secret)-3:]
}

func snapshotResponse(status int, body []byte) *http.Response {
	return &http.Response{
		StatusCode: status, ProtoMajor: 2, ProtoMinor: 0,
		Body: io.NopCloser(bytes.NewReader(body)), Header: make(http.Header),
	}
}
