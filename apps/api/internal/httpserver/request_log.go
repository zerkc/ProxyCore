package httpserver

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/enrollment"
)

const (
	requestLogCaptureBytes = 512
	requestLogMaxErrorLen  = 200
)

var (
	requestLogPEM      = regexp.MustCompile(`(?s)-----BEGIN[^-]*-----.*?(-----END[^-]*-----|$)`)
	requestLogToken    = regexp.MustCompile(`pcenr\d_[A-Za-z0-9_\-]+`)
	requestLogLongRun  = regexp.MustCompile(`[A-Za-z0-9+/_=\-]{32,}`)
	requestLogUUID     = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	requestLogErrField = regexp.MustCompile(`"error"\s*:\s*"((?:[^"\\]|\\.)*)`)
)

// RequestLogMiddleware logs one redacted line per completed request:
//
//	http <METHOD> <route> status=<code> dur=<ms> bytes=<n> [err="<message>"]
//
// It never logs query strings, headers, cookies or bodies; only the "error"
// field of a JSON error body is captured, truncated and secret-redacted. Static
// assets and health probes are logged only on failure. A panic is recovered,
// logged by type only and answered with a generic 500.
func RequestLogMiddleware(logger *log.Logger, next http.Handler) http.Handler {
	if logger == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, original *http.Request) {
		start := time.Now()
		var requestID string
		var idBytes [16]byte
		if _, err := rand.Read(idBytes[:]); err == nil {
			requestID = hex.EncodeToString(idBytes[:])
			w.Header().Set("X-Request-ID", requestID)
		}
		notes := &causeNotes{}
		// r is the request the mux sees, so r.Pattern is readable afterwards.
		r := original.WithContext(context.WithValue(original.Context(), causeNotesKey{}, notes))
		rec := &requestLogWriter{ResponseWriter: w}
		defer func() {
			if recovered := recover(); recovered != nil {
				if recovered == http.ErrAbortHandler {
					panic(recovered)
				}
				logger.Printf("http panic %s %s type=%T", r.Method, requestLogRoute(r), recovered)
				if !rec.wroteHeader {
					writeJSON(rec, http.StatusInternalServerError, map[string]any{"error": "internal server error"})
				}
			}
			status := rec.status
			if status == 0 {
				status = http.StatusOK
			}
			route := requestLogRoute(r)
			if status < http.StatusBadRequest && requestLogQuiet(route) {
				return
			}
			line := fmt.Sprintf("http %s %s status=%d dur=%dms bytes=%d", r.Method, route, status, time.Since(start).Milliseconds(), rec.bytes)
			if requestID != "" {
				line += " request_id=" + requestID
			}
			if status >= http.StatusBadRequest {
				if message := requestLogError(rec.captured); message != "" {
					line += fmt.Sprintf(" err=%q", message)
				}
			}
			if causes := notes.render(); causes != "" {
				line += " " + causes
			}
			logger.Print(line)
		}()
		next.ServeHTTP(rec, r)
	})
}

func requestLogQuiet(route string) bool {
	return !strings.HasPrefix(route, "/api/") || route == "/api/health" || route == "/api/ready"
}

// requestLogRoute prefers the matched route pattern; otherwise it redacts the
// raw path segment by segment. The query string is never used.
func requestLogRoute(r *http.Request) string {
	if pattern := r.Pattern; pattern != "" {
		if _, path, ok := strings.Cut(pattern, " "); ok {
			pattern = path
		}
		if pattern != "/" && pattern != "" {
			return pattern
		}
	}
	return redactRequestPath(r.URL.Path)
}

func redactRequestPath(path string) string {
	segments := strings.Split(path, "/")
	for index, segment := range segments {
		switch {
		case segment == "":
		case requestLogToken.MatchString(segment) || requestLogLongRun.MatchString(segment):
			segments[index] = ":token"
		case requestLogUUID.MatchString(segment) || isLongDigits(segment):
			segments[index] = ":id"
		}
	}
	return strings.Join(segments, "/")
}

func isLongDigits(s string) bool {
	if len(s) < 6 {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func requestLogError(captured []byte) string {
	match := requestLogErrField.FindSubmatch(captured)
	if match == nil {
		return ""
	}
	return redactLogText(string(match[1]))
}

// redactLogText removes secret-looking material and truncates to 200 runes.
func redactLogText(text string) string {
	text = requestLogPEM.ReplaceAllString(text, "[pem]")
	text = requestLogToken.ReplaceAllString(text, "[token]")
	text = requestLogLongRun.ReplaceAllString(text, "[redacted]")
	if runes := []rune(text); len(runes) > requestLogMaxErrorLen {
		text = string(runes[:requestLogMaxErrorLen])
	}
	return text
}

// requestLogWriter records status, size and the first bytes of an error body
// without altering what the client receives.
type requestLogWriter struct {
	http.ResponseWriter
	status      int
	bytes       int
	wroteHeader bool
	captured    []byte
}

func (w *requestLogWriter) WriteHeader(status int) {
	if !w.wroteHeader {
		w.status, w.wroteHeader = status, true
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *requestLogWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.status, w.wroteHeader = http.StatusOK, true
	}
	if w.status >= http.StatusBadRequest && len(w.captured) < requestLogCaptureBytes {
		room := requestLogCaptureBytes - len(w.captured)
		if room > len(p) {
			room = len(p)
		}
		w.captured = append(w.captured, p[:room]...)
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytes += n
	return n, err
}

func (w *requestLogWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *requestLogWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *requestLogWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := w.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, http.ErrNotSupported
}

func (w *requestLogWriter) Push(target string, opts *http.PushOptions) error {
	if p, ok := w.ResponseWriter.(http.Pusher); ok {
		return p.Push(target, opts)
	}
	return http.ErrNotSupported
}

type causeNotesKey struct{}

type causeNotes struct {
	mu    sync.Mutex
	items []string
	job   string
}

func (n *causeNotes) render() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	parts := make([]string, 0, 2)
	if len(n.items) > 0 {
		parts = append(parts, "cause="+strings.Join(n.items, ";"))
	}
	if n.job != "" {
		parts = append(parts, n.job)
	}
	return strings.Join(parts, " ")
}

// noteJob links a server-generated request ID to existing durable job IDs.
// It never changes the job's persisted correlation ID or response payload.
func noteJob(ctx context.Context, jobID, revisionID, correlationID string) {
	if ctx == nil || !requestLogUUID.MatchString(jobID) || !requestLogUUID.MatchString(revisionID) || !requestLogUUID.MatchString(correlationID) {
		return
	}
	if notes, ok := ctx.Value(causeNotesKey{}).(*causeNotes); ok {
		notes.mu.Lock()
		notes.job = "job=" + jobID + " revision=" + revisionID + " correlation=" + correlationID
		notes.mu.Unlock()
	}
}

// noteCause attaches a redacted cause (sentinel names plus Go type chain, never
// the error text) to the request line logged by RequestLogMiddleware. It is a
// no-op outside the middleware, so handlers can call it unconditionally where
// they map an internal error to a generic response.
func noteCause(ctx context.Context, step string, err error) {
	if err == nil || ctx == nil {
		return
	}
	notes, ok := ctx.Value(causeNotesKey{}).(*causeNotes)
	if !ok {
		return
	}
	notes.mu.Lock()
	defer notes.mu.Unlock()
	if len(notes.items) < 4 {
		notes.items = append(notes.items, "["+step+" "+enrollment.RedactedCause(err, workflowSentinels...)+"]")
	}
}
