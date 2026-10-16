package httpserver

import (
	"context"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/enrollment"
)

const (
	// EnrollmentGrantExchangePath is the token-authenticated primary exchange
	// served only on the dedicated TLS enrollment listener.
	EnrollmentGrantExchangePath = "/api/topology/enroll"

	maxGrantExchangeBodyBytes  = 8 << 10
	grantExchangeReadTimeout   = 10 * time.Second
	grantExchangeScheme        = "Enrollment"
	defaultGrantExchangeWindow = time.Minute
	defaultGrantExchangeIPMax  = 20
	defaultGrantExchangeSelMax = 10
	maxGrantExchangeLimiterKey = 4096

	grantExchangeDenied      = "ENROLLMENT_DENIED"
	grantExchangeRateLimited = "ENROLLMENT_RATE_LIMITED"
)

// EnrollmentGrantIssuer is the primary-side grant exchange seam. It is
// satisfied by *enrollment.PrimaryService.
type EnrollmentGrantIssuer interface {
	IssueGrant(context.Context, enrollment.PrimaryGrantRequest) (enrollment.PrimaryGrantResponse, error)
}

type EnrollmentGrantExchangeHandlerOptions struct {
	Issuer      EnrollmentGrantIssuer
	Certificate IdentityProofCertificateProvider
	Now         func() time.Time
	// Window, IPLimit and SelectorLimit bound attempts per remote IP and per
	// token selector. Zero values select the defaults.
	Window        time.Duration
	IPLimit       int
	SelectorLimit int
}

type enrollmentGrantExchangeRequest struct {
	AttemptID             string                                 `json:"attemptId"`
	InstallationID        string                                 `json:"installationId"`
	NodeID                string                                 `json:"nodeId"`
	RecipientPublicKey    enrollment.BootstrapRecipientPublicKey `json:"recipientPublicKey"`
	PreviewDigest         string                                 `json:"previewDigest"`
	PrimaryInstallationID string                                 `json:"primaryInstallationId"`
	LeadershipGeneration  uint64                                 `json:"leadershipGeneration"`
}

type enrollmentGrantExchangeResponse struct {
	SealedBootstrapPayload []byte    `json:"sealedBootstrapPayload"`
	PayloadHash            string    `json:"payloadHash"`
	ExpiresAt              time.Time `json:"expiresAt"`
}

// EnrollmentGrantExchangeHandler serves POST /api/topology/enroll.
type EnrollmentGrantExchangeHandler struct {
	issuer      EnrollmentGrantIssuer
	certificate IdentityProofCertificateProvider
	now         func() time.Time
	ip          *attemptLimiter
	selector    *attemptLimiter
}

func NewEnrollmentGrantExchangeHandler(opts EnrollmentGrantExchangeHandlerOptions) *EnrollmentGrantExchangeHandler {
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	window := opts.Window
	if window <= 0 {
		window = defaultGrantExchangeWindow
	}
	ipMax, selMax := opts.IPLimit, opts.SelectorLimit
	if ipMax <= 0 {
		ipMax = defaultGrantExchangeIPMax
	}
	if selMax <= 0 {
		selMax = defaultGrantExchangeSelMax
	}
	return &EnrollmentGrantExchangeHandler{
		issuer: opts.Issuer, certificate: opts.Certificate, now: now,
		ip:       newAttemptLimiter(ipMax, window, maxGrantExchangeLimiterKey),
		selector: newAttemptLimiter(selMax, window, maxGrantExchangeLimiterKey),
	}
}

// NewEnrollmentGrantExchangeMux places the exchange route in front of next.
func NewEnrollmentGrantExchangeMux(handler *EnrollmentGrantExchangeHandler, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r != nil && r.URL != nil && r.URL.Path == EnrollmentGrantExchangePath {
			if handler == nil {
				writeGrantExchangeStatus(w, http.StatusUnauthorized, grantExchangeDenied)
				return
			}
			handler.ServeHTTP(w, r)
			return
		}
		if next == nil {
			setSnapshotByTokenHeaders(w)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (h *EnrollmentGrantExchangeHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setSnapshotByTokenHeaders(w)
	if r == nil || r.URL == nil || r.URL.Path != EnrollmentGrantExchangePath ||
		r.URL.RawPath != "" || r.URL.RawQuery != "" || r.URL.ForceQuery {
		writeGrantExchangeStatus(w, http.StatusNotFound, "NOT_FOUND")
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeGrantExchangeStatus(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED")
		return
	}
	if h == nil || h.issuer == nil || !h.validTLSAuthority(r) {
		writeGrantExchangeStatus(w, http.StatusUnauthorized, grantExchangeDenied)
		return
	}
	now := h.now()
	if !h.ip.allow(remoteHost(r), now) {
		writeGrantExchangeRateLimited(w)
		return
	}
	token, ok := enrollmentSchemeToken(r.Header)
	if !ok {
		writeGrantExchangeStatus(w, http.StatusUnauthorized, grantExchangeDenied)
		return
	}
	selector, secret, parsed := parseHTTPEnrollmentToken(token)
	zeroBytes(secret)
	if !parsed {
		writeGrantExchangeStatus(w, http.StatusUnauthorized, grantExchangeDenied)
		return
	}
	if !h.selector.allow(selector, now) {
		writeGrantExchangeRateLimited(w)
		return
	}
	request, ok := decodeGrantExchangeRequest(w, r, token)
	if !ok {
		writeGrantExchangeStatus(w, http.StatusUnauthorized, grantExchangeDenied)
		return
	}
	response, err := h.issuer.IssueGrant(r.Context(), request)
	if err != nil || len(response.SealedBootstrapPayload) == 0 {
		noteCause(r.Context(), "issue_grant", err)
		writeGrantExchangeStatus(w, http.StatusUnauthorized, grantExchangeDenied)
		return
	}
	body, err := json.Marshal(enrollmentGrantExchangeResponse{
		SealedBootstrapPayload: response.SealedBootstrapPayload,
		PayloadHash:            response.PayloadHash,
		ExpiresAt:              response.ExpiresAt.UTC(),
	})
	if err != nil {
		writeGrantExchangeStatus(w, http.StatusUnauthorized, grantExchangeDenied)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write(body)
}

func (h *EnrollmentGrantExchangeHandler) validTLSAuthority(r *http.Request) bool {
	if r.TLS == nil || !r.TLS.HandshakeComplete || r.TLS.Version < tls.VersionTLS13 {
		return false
	}
	host, port, err := net.SplitHostPort(r.Host)
	if err != nil || host == "" || !validSnapshotPublicationPort(port) ||
		!identityProofSNIMatches(host, r.TLS.ServerName) || h.certificate == nil {
		return false
	}
	certificate, err := h.certificate(r.Context())
	return err == nil && certificate != nil && certificate.VerifyHostname(host) == nil
}

// enrollmentSchemeToken accepts exactly one Authorization value of the form
// "Enrollment <token>"; any other scheme or shape is rejected.
func enrollmentSchemeToken(header http.Header) (string, bool) {
	values := header.Values("Authorization")
	if len(values) != 1 {
		return "", false
	}
	scheme, token, found := strings.Cut(values[0], " ")
	if !found || scheme != grantExchangeScheme || token == "" || strings.ContainsAny(token, " \t\r\n") {
		return "", false
	}
	return token, true
}

func decodeGrantExchangeRequest(w http.ResponseWriter, r *http.Request, token string) (enrollment.PrimaryGrantRequest, bool) {
	if r.Body == nil || r.ContentLength > maxGrantExchangeBodyBytes {
		return enrollment.PrimaryGrantRequest{}, false
	}
	// Best effort: servers that cannot set a read deadline still have the
	// header timeout and the byte cap.
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(grantExchangeReadTimeout))
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxGrantExchangeBodyBytes))
	decoder.DisallowUnknownFields()
	var body enrollmentGrantExchangeRequest
	if err := decoder.Decode(&body); err != nil {
		return enrollment.PrimaryGrantRequest{}, false
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return enrollment.PrimaryGrantRequest{}, false
	}
	digest, err := hex.DecodeString(body.PreviewDigest)
	if err != nil || hex.EncodeToString(digest) != body.PreviewDigest {
		return enrollment.PrimaryGrantRequest{}, false
	}
	return enrollment.PrimaryGrantRequest{
		EnrollmentToken:               token,
		AttemptID:                     body.AttemptID,
		TargetInstallationID:          body.InstallationID,
		TargetNodeID:                  body.NodeID,
		RecipientPublicKey:            body.RecipientPublicKey,
		VerifiedPreviewDigest:         digest,
		ExpectedPrimaryInstallationID: body.PrimaryInstallationID,
		ExpectedLeadershipGeneration:  body.LeadershipGeneration,
	}, true
}

func remoteHost(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func writeGrantExchangeStatus(w http.ResponseWriter, status int, code string) {
	setSnapshotByTokenHeaders(w)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"error":"` + code + `"}`))
}

func writeGrantExchangeRateLimited(w http.ResponseWriter) {
	w.Header().Set("Retry-After", strconv.Itoa(int(defaultGrantExchangeWindow/time.Second)))
	writeGrantExchangeStatus(w, http.StatusTooManyRequests, grantExchangeRateLimited)
}

// attemptLimiter is a fixed-window counter with a bounded key set. When the
// set is full of live windows, unseen keys are denied (fail closed).
type attemptLimiter struct {
	mu      sync.Mutex
	max     int
	window  time.Duration
	maxKeys int
	entries map[string]*attemptWindow
}

type attemptWindow struct {
	start time.Time
	count int
}

func newAttemptLimiter(max int, window time.Duration, maxKeys int) *attemptLimiter {
	return &attemptLimiter{max: max, window: window, maxKeys: maxKeys, entries: make(map[string]*attemptWindow)}
}

func (l *attemptLimiter) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if entry, ok := l.entries[key]; ok {
		if now.Sub(entry.start) >= l.window {
			entry.start, entry.count = now, 0
		}
		entry.count++
		return entry.count <= l.max
	}
	if len(l.entries) >= l.maxKeys {
		for k, entry := range l.entries {
			if now.Sub(entry.start) >= l.window {
				delete(l.entries, k)
			}
		}
		if len(l.entries) >= l.maxKeys {
			return false
		}
	}
	l.entries[key] = &attemptWindow{start: now, count: 1}
	return true
}
