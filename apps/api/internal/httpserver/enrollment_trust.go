package httpserver

import (
	"io"
	"net/http"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
)

const (
	// EnrollmentTrustPath is the Owner-only topology settings metadata route.
	EnrollmentTrustPath = "/api/topology/settings/enrollment-trust"
	// EnrollmentTrustCAPEMPath is the Owner-only public CA PEM download route.
	EnrollmentTrustCAPEMPath = EnrollmentTrustPath + "/ca"

	enrollmentTrustSettingsAliasPath  = "/api/settings/enrollment-trust"
	enrollmentTrustSettingsAliasCAPEM = enrollmentTrustSettingsAliasPath + "/ca"
	enrollmentTrustUnavailableMessage = "enrollment trust unavailable"
	enrollmentTrustNotReadyMessage    = "enrollment trust is not ready"
)

type enrollmentTrustResponse struct {
	Status           string     `json:"status"`
	Configured       bool       `json:"configured"`
	Ready            bool       `json:"ready"`
	CertificatePEM   string     `json:"certificatePem,omitempty"`
	CACertificatePEM string     `json:"caCertificatePem,omitempty"`
	CADERHashSHA256  string     `json:"caDerSha256,omitempty"`
	ExpiresAt        *time.Time `json:"expiresAt,omitempty"`
}

func (s *Server) handleGetEnrollmentTrust(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.requireEnrollmentTrustUser(w, r) {
		return
	}
	store, ok := s.configStore(w)
	if !ok {
		return
	}
	trust, err := store.LoadEnrollmentTLSTrust(r.Context())
	if err != nil {
		writeConfigError(w, &httpError{status: http.StatusServiceUnavailable, message: enrollmentTrustUnavailableMessage, code: "ENROLLMENT_TRUST_UNAVAILABLE"})
		return
	}
	writeJSON(w, http.StatusOK, newEnrollmentTrustResponse(trust))
}

func (s *Server) handleDownloadEnrollmentCA(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.requireEnrollmentTrustUser(w, r) {
		return
	}
	store, ok := s.configStore(w)
	if !ok {
		return
	}
	trust, err := store.LoadEnrollmentTLSTrust(r.Context())
	if err != nil {
		writeConfigError(w, &httpError{status: http.StatusServiceUnavailable, message: enrollmentTrustUnavailableMessage, code: "ENROLLMENT_TRUST_UNAVAILABLE"})
		return
	}
	if !trust.Configured {
		writeConfigError(w, &httpError{status: http.StatusConflict, message: "enrollment trust is unconfigured", code: "ENROLLMENT_TRUST_UNCONFIGURED"})
		return
	}
	if !trust.Ready {
		writeConfigError(w, &httpError{status: http.StatusConflict, message: enrollmentTrustNotReadyMessage, code: "ENROLLMENT_TRUST_NOT_READY"})
		return
	}
	w.Header().Set("Content-Type", "application/x-pem-file")
	w.Header().Set("Content-Disposition", `attachment; filename="proxycore-enrollment-ca.pem"`)
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, trust.Material.CACertificatePEM)
}

func newEnrollmentTrustResponse(trust configuration.EnrollmentTLSTrust) enrollmentTrustResponse {
	status := "not-ready"
	if !trust.Configured {
		status = "unconfigured"
	}
	response := enrollmentTrustResponse{
		Status:     status,
		Configured: trust.Configured,
		Ready:      trust.Ready,
	}
	if !trust.Ready {
		return response
	}
	expiresAt := trust.Material.ExpiresAt
	response.Status = "ready"
	response.CertificatePEM = trust.Material.CertificatePEM
	response.CACertificatePEM = trust.Material.CACertificatePEM
	response.CADERHashSHA256 = trust.Material.CADERHashSHA256
	response.ExpiresAt = &expiresAt
	return response
}
