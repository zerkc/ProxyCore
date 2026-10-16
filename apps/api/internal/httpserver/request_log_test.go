package httpserver_test

import (
	"bufio"
	"bytes"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zerkc/ProxyCore/apps/api/internal/config"
	"github.com/zerkc/ProxyCore/apps/api/internal/httpserver"
)

func runRequestLog(t *testing.T, method, target string, handler http.HandlerFunc) (string, *httptest.ResponseRecorder) {
	t.Helper()
	var buf bytes.Buffer
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/items/{id}", handler)
	mux.HandleFunc("POST /api/things", handler)
	mux.HandleFunc("/", handler)
	h := httpserver.RequestLogMiddleware(log.New(&buf, "", 0), mux)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, target, strings.NewReader("")))
	return buf.String(), rec
}

func TestRequestLogLinesRedactAndCaptureErrors(t *testing.T) {
	secretToken := "pcenr2_AbCdEfGhIjKlMnOpQrStUvWxYz0123456789"
	cases := []struct {
		name    string
		method  string
		target  string
		handler http.HandlerFunc
		want    []string
		absent  []string
	}{
		{"ok uses route pattern", "GET", "/api/items/7f0c1c2e-1111-2222-3333-444455556666?token=" + secretToken, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true,"secret":"BODYSECRET"}`))
		}, []string{"http GET /api/items/{id} status=200", "dur=", "bytes=33"}, []string{"?", "token=", "7f0c1c2e", "BODYSECRET", secretToken}},
		{"4xx captures error field", "POST", "/api/things", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":"name taken"}` + "\n"))
		}, []string{"http POST /api/things status=409", `err="name taken"`}, nil},
		{"error text is redacted", "POST", "/api/things", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"bad ` + secretToken + ` and -----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE----- tail"}`))
		}, []string{"status=400", "err="}, []string{secretToken, "MIIB", "BEGIN CERTIFICATE"}},
		{"error text truncated", "POST", "/api/things", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"` + strings.Repeat("a ", 400) + `"}`))
		}, []string{"status=400", "err="}, []string{strings.Repeat("a ", 150)}},
		{"unmatched path is redacted", "GET", "/unknown/" + secretToken + "/7f0c1c2e-1111-2222-3333-444455556666/12345678?x=1", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}, []string{"status=404", ":token", ":id"}, []string{secretToken, "7f0c1c2e", "x=1"}},
		{"static asset ok is skipped", "GET", "/assets/app.js", func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("js"))
		}, nil, []string{"http "}},
		{"static asset error is logged", "GET", "/assets/app.js", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}, []string{"status=500"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, _ := runRequestLog(t, tc.method, tc.target, tc.handler)
			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Fatalf("log missing %q:\n%s", want, out)
				}
			}
			for _, absent := range tc.absent {
				if strings.Contains(out, absent) {
					t.Fatalf("log must not contain %q:\n%s", absent, out)
				}
			}
		})
	}
}

func TestRequestLogSkipsHealthUnlessFailing(t *testing.T) {
	var buf bytes.Buffer
	status := http.StatusOK
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) })
	h := httpserver.RequestLogMiddleware(log.New(&buf, "", 0), mux)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/health", nil))
	if buf.Len() != 0 {
		t.Fatalf("healthy probe logged: %s", buf.String())
	}
	status = http.StatusServiceUnavailable
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/health", nil))
	if !strings.Contains(buf.String(), "status=503") {
		t.Fatalf("failing probe not logged: %q", buf.String())
	}
}

func TestRequestLogDoesNotChangeResponse(t *testing.T) {
	_, rec := runRequestLog(t, "POST", "/api/things", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Test", "1")
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte(`{"error":"short"}`))
	})
	if rec.Code != http.StatusTeapot || rec.Header().Get("X-Test") != "1" || rec.Body.String() != `{"error":"short"}` {
		t.Fatalf("response changed: %d %v %q", rec.Code, rec.Header(), rec.Body.String())
	}
}

type hijackRecorder struct {
	*httptest.ResponseRecorder
	hijacked bool
}

func (h *hijackRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h.hijacked = true
	return nil, nil, nil
}

func TestRequestLogPassesFlusherAndHijackerThrough(t *testing.T) {
	var buf bytes.Buffer
	var flusher, hijacker bool
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
			flusher = true
		}
		if hj, ok := w.(http.Hijacker); ok {
			_, _, _ = hj.Hijack()
			hijacker = true
		}
	})
	h := httpserver.RequestLogMiddleware(log.New(&buf, "", 0), handler)
	rec := &hijackRecorder{ResponseRecorder: httptest.NewRecorder()}
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/x", nil))
	if !flusher || !rec.Flushed || !hijacker || !rec.hijacked {
		t.Fatalf("passthrough failed flusher=%t flushed=%t hijacker=%t hijacked=%t", flusher, rec.Flushed, hijacker, rec.hijacked)
	}
}

func TestRequestLogRecoversPanicWithTypeOnly(t *testing.T) {
	var buf bytes.Buffer
	handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom pcenr2_SECRETSECRETSECRETSECRET") })
	h := httpserver.RequestLogMiddleware(log.New(&buf, "", 0), handler)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/x?q=1", nil))
	out := buf.String()
	if rec.Code != http.StatusInternalServerError || !strings.Contains(out, "http panic GET /api/x type=string") || strings.Contains(out, "boom") || strings.Contains(out, "q=1") {
		t.Fatalf("code=%d log=%s", rec.Code, out)
	}
}

func TestRealRouterLogsNonEnrollment4xxWithError(t *testing.T) {
	var buf bytes.Buffer
	srv := httpserver.New(config.Config{UIDist: t.TempDir()}, log.New(&buf, "", 0))
	req := httptest.NewRequest(http.MethodGet, "/api/users?secret=abc", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code < 400 {
		t.Fatalf("expected an error status, got %d", rec.Code)
	}
	out := buf.String()
	if !strings.Contains(out, "http GET /api/users status=") || !strings.Contains(out, "err=") || strings.Contains(out, "secret=abc") {
		t.Fatalf("log = %q (body %s)", out, rec.Body.String())
	}
}
