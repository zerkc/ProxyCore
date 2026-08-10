package httpserver

import (
	"io"
	"net/http"
	"strings"

	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
)

func (s *Server) handleListCertificates(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireUser(w, r); !ok {
		return
	}
	store, ok := s.configStore(w)
	if !ok {
		return
	}
	certificates, err := store.ListCertificates(r.Context())
	if err != nil {
		writeConfigError(w, err)
		return
	}
	public := make([]map[string]any, 0, len(certificates))
	for _, cert := range certificates {
		public = append(public, cert.PublicCertificate())
	}
	writeJSON(w, http.StatusOK, map[string]any{"certificates": public})
}

func (s *Server) handleIssueCertificate(w http.ResponseWriter, r *http.Request) {
	user, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	store, ok := s.configStore(w)
	if !ok {
		return
	}
	input, err := s.parseCertificateRequest(w, r)
	if err != nil {
		writeConfigError(w, err)
		return
	}
	cert, err := store.IssueCertificate(r.Context(), input, user.ID)
	if err != nil {
		writeConfigError(w, err)
		return
	}
	public := cert.PublicCertificate()
	if cert.Status == "failed" {
		reason := cert.FailureReason
		if reason == "" {
			reason = "Certificate issuance failed"
		}
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"certificate": public, "error": reason})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"certificate": public})
}

func (s *Server) parseCertificateRequest(w http.ResponseWriter, r *http.Request) (configuration.CertificateIssueInput, error) {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		return s.parseCertificateMultipart(r)
	}
	body, err := readJSONObject(w, r)
	if err != nil {
		return configuration.CertificateIssueInput{}, err
	}
	raw := certRawInput{
		hostnames:          body["hostnames"],
		issuer:             body["issuer"],
		challenge:          body["challenge"],
		environment:        body["environment"],
		email:              body["email"],
		keyType:            body["keyType"],
		propagationSeconds: body["propagationSeconds"],
		certificatePem:     body["certificatePem"],
		privateKeyPem:      body["privateKeyPem"],
	}
	if cloudflare, ok := body["cloudflare"].(map[string]any); ok {
		raw.cfAPIToken = cloudflare["apiToken"]
		raw.cfZoneID = cloudflare["zoneId"]
		raw.cfZoneName = cloudflare["zoneName"]
	}
	return buildCertificateInput(raw, s.cfg.ACMEDirectoryURL, s.cfg.ACMEProductionDirectoryURL)
}

func (s *Server) parseCertificateMultipart(r *http.Request) (configuration.CertificateIssueInput, error) {
	if err := r.ParseMultipartForm(4 << 20); err != nil {
		return configuration.CertificateIssueInput{}, &httpError{status: http.StatusBadRequest, message: "multipart form is invalid"}
	}
	hostnames, err := parseHostnamesField(r.FormValue("hostnames"))
	if err != nil {
		return configuration.CertificateIssueInput{}, err
	}
	certificatePem, err := multipartFileField(r, "certificate")
	if err != nil {
		return configuration.CertificateIssueInput{}, err
	}
	privateKeyPem, err := multipartFileField(r, "privateKey")
	if err != nil {
		return configuration.CertificateIssueInput{}, err
	}
	rawHostnames := make([]any, len(hostnames))
	for i, hostname := range hostnames {
		rawHostnames[i] = hostname
	}
	raw := certRawInput{
		hostnames:          rawHostnames,
		issuer:             multipartStringField(r, "issuer"),
		challenge:          multipartStringField(r, "challenge"),
		environment:        multipartStringField(r, "environment"),
		email:              multipartStringField(r, "email"),
		keyType:            multipartStringField(r, "keyType"),
		propagationSeconds: multipartStringField(r, "propagationSeconds"),
		certificatePem:     certificatePem,
		privateKeyPem:      privateKeyPem,
		cfAPIToken:         multipartStringField(r, "cloudflareApiToken"),
		cfZoneID:           multipartStringField(r, "cloudflareZoneId"),
		cfZoneName:         multipartStringField(r, "cloudflareZoneName"),
	}
	return buildCertificateInput(raw, s.cfg.ACMEDirectoryURL, s.cfg.ACMEProductionDirectoryURL)
}

func parseHostnamesField(value string) ([]string, error) {
	hostnames := []string{}
	for _, part := range strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == '\n' }) {
		trimmed := strings.TrimSpace(part)
		if trimmed != "" {
			hostnames = append(hostnames, trimmed)
		}
	}
	return hostnames, nil
}

func multipartStringField(r *http.Request, name string) any {
	value := strings.TrimSpace(r.FormValue(name))
	if value == "" {
		return nil
	}
	return value
}

func multipartFileField(r *http.Request, name string) (any, error) {
	file, header, err := r.FormFile(name)
	if err != nil {
		if value := r.FormValue(name); value != "" {
			return value, nil
		}
		return nil, nil
	}
	defer file.Close()
	if header.Size > 2*1024*1024 {
		return nil, &httpError{status: http.StatusRequestEntityTooLarge, message: name + " is larger than 2 MiB"}
	}
	contents, err := io.ReadAll(file)
	if err != nil {
		return nil, err
	}
	return string(contents), nil
}
