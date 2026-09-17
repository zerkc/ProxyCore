package httpserver

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/zerkc/ProxyCore/apps/api/internal/enrollment"
)

const (
	EnrollmentIdentityProofPath = "/api/topology/enrollment/identity-proof"
	maxIdentityProofBodyBytes   = 8 << 10
)

// IdentityProofCertificateProvider returns the current leaf certificate without its private key.
type IdentityProofCertificateProvider func(context.Context) (*x509.Certificate, error)

type EnrollmentIdentityHandlerOptions struct {
	Signer      *enrollment.IdentityProofSigner
	Certificate IdentityProofCertificateProvider
}

type enrollmentIdentityHandler struct {
	signer      *enrollment.IdentityProofSigner
	certificate IdentityProofCertificateProvider
}

// NewEnrollmentIdentityMux returns a dedicated handler for the proof endpoint only.
func NewEnrollmentIdentityMux(opts EnrollmentIdentityHandlerOptions) http.Handler {
	return &enrollmentIdentityHandler{signer: opts.Signer, certificate: opts.Certificate}
}

func (h *enrollmentIdentityHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL == nil || r.URL.Path != EnrollmentIdentityProofPath || r.URL.RawPath != "" || r.URL.RawQuery != "" || r.URL.ForceQuery {
		writeIdentityProofError(w, http.StatusNotFound, "not found")
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeIdentityProofError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if r.TLS == nil || !r.TLS.HandshakeComplete || r.TLS.Version < tls.VersionTLS13 {
		writeIdentityProofError(w, http.StatusBadRequest, "secure transport required")
		return
	}

	request, ok := decodeIdentityProofRequest(w, r)
	if !ok || request.Validate() != nil {
		writeIdentityProofError(w, http.StatusBadRequest, "invalid identity proof request")
		return
	}
	primary, _ := url.Parse(request.PrimaryURL)
	if r.Host != primary.Host || !identityProofSNIMatches(primary.Hostname(), r.TLS.ServerName) {
		writeIdentityProofError(w, http.StatusMisdirectedRequest, "invalid enrollment authority")
		return
	}
	if h.certificate == nil {
		writeIdentityProofError(w, http.StatusServiceUnavailable, "identity proof unavailable")
		return
	}
	certificate, err := h.certificate(r.Context())
	if err != nil || certificate == nil || certificate.VerifyHostname(primary.Hostname()) != nil {
		writeIdentityProofError(w, http.StatusMisdirectedRequest, "invalid enrollment authority")
		return
	}
	if h.signer == nil {
		writeIdentityProofError(w, http.StatusServiceUnavailable, "identity proof unavailable")
		return
	}
	proof, err := h.signer.Sign(r.Context(), request)
	if err != nil {
		writeIdentityProofError(w, http.StatusServiceUnavailable, "identity proof unavailable")
		return
	}

	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(proof)
}

func decodeIdentityProofRequest(w http.ResponseWriter, r *http.Request) (enrollment.IdentityProofRequest, bool) {
	if r.ContentLength > maxIdentityProofBodyBytes {
		return enrollment.IdentityProofRequest{}, false
	}
	body := http.MaxBytesReader(w, r.Body, maxIdentityProofBodyBytes)
	decoder := json.NewDecoder(body)
	decoder.DisallowUnknownFields()
	var request enrollment.IdentityProofRequest
	if err := decoder.Decode(&request); err != nil {
		return enrollment.IdentityProofRequest{}, false
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return enrollment.IdentityProofRequest{}, false
	}
	return request, true
}

func identityProofSNIMatches(host, serverName string) bool {
	ip := net.ParseIP(host)
	if ip != nil {
		if serverName == "" {
			return true
		}
		candidate := net.ParseIP(serverName)
		return candidate != nil && candidate.Equal(ip)
	}
	return serverName != "" && strings.EqualFold(serverName, host)
}

func writeIdentityProofError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(struct {
		Error string `json:"error"`
	}{Error: message})
}
