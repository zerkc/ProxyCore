package httpserver

import (
	"net/http"
	"slices"
	"strings"

	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
)

// certRawInput holds the loosely-typed certificate fields from JSON or
// multipart bodies before validation.
type certRawInput struct {
	hostnames          any
	issuer             any
	challenge          any
	environment        any
	email              any
	keyType            any
	propagationSeconds any
	certificatePem     any
	privateKeyPem      any
	cfAPIToken         any
	cfZoneID           any
	cfZoneName         any
}

// buildCertificateInput validates raw certificate fields and produces an issue
// input, mirroring the Node buildInput in certificates/route.ts.
func buildCertificateInput(raw certRawInput, acmeDirectoryURL, acmeProductionURL string) (configuration.CertificateIssueInput, error) {
	hostnames, err := parseCertHostnames(raw.hostnames)
	if err != nil {
		return configuration.CertificateIssueInput{}, err
	}

	issuer, _ := raw.issuer.(string)
	if issuer != "self-signed" && issuer != "uploaded" && issuer != "letsencrypt" {
		return configuration.CertificateIssueInput{}, &httpError{status: http.StatusBadRequest, message: "Invalid certificate source"}
	}
	challenge, _ := raw.challenge.(string)
	if challenge != "none" && challenge != "http-01" && challenge != "dns-01" {
		return configuration.CertificateIssueInput{}, &httpError{status: http.StatusBadRequest, message: "Invalid ACME challenge"}
	}

	environment := ""
	if value, ok := raw.environment.(string); ok && strings.TrimSpace(value) != "" {
		environment = strings.TrimSpace(value)
	} else if issuer == "letsencrypt" {
		environment = "staging"
	} else {
		environment = "local"
	}
	if !slices.Contains([]string{"local", "staging", "production"}, environment) {
		return configuration.CertificateIssueInput{}, &httpError{status: http.StatusBadRequest, message: "Invalid certificate environment"}
	}

	if issuer != "letsencrypt" && challenge != "none" {
		return configuration.CertificateIssueInput{}, &httpError{status: http.StatusBadRequest, message: "Self-signed and uploaded certificates use no challenge"}
	}
	if issuer == "letsencrypt" && challenge == "none" {
		return configuration.CertificateIssueInput{}, &httpError{status: http.StatusBadRequest, message: "Let's Encrypt requires HTTP-01 or DNS-01"}
	}

	keyType := "rsa"
	if value, ok := raw.keyType.(string); ok && value != "" {
		keyType = value
	}
	if keyType != "rsa" && keyType != "ecdsa" {
		return configuration.CertificateIssueInput{}, &httpError{status: http.StatusBadRequest, message: "Invalid certificate key type"}
	}

	var propagationSeconds *int
	if challenge == "dns-01" {
		value := 30
		if !isEmptyValue(raw.propagationSeconds) {
			value = int(jsNumber(raw.propagationSeconds))
		}
		if value < 0 || value > 600 || !isIntegerValue(raw.propagationSeconds) {
			return configuration.CertificateIssueInput{}, &httpError{status: http.StatusBadRequest, message: "Propagation seconds must be between 0 and 600"}
		}
		propagationSeconds = &value
	}

	certificatePem := stringValue(raw.certificatePem)
	privateKeyPem := stringValue(raw.privateKeyPem)
	if issuer == "uploaded" && (certificatePem == "" || privateKeyPem == "") {
		return configuration.CertificateIssueInput{}, &httpError{status: http.StatusBadRequest, message: "Upload both the certificate PEM and the private key PEM"}
	}

	input := configuration.CertificateIssueInput{
		Hostnames:          hostnames,
		Issuer:             issuer,
		Challenge:          challenge,
		Environment:        environment,
		Email:              stringValue(raw.email),
		KeyType:            keyType,
		PropagationSeconds: propagationSeconds,
		CertificatePEM:     certificatePem,
		PrivateKeyPEM:      privateKeyPem,
	}
	if issuer == "letsencrypt" {
		if environment == "production" {
			input.DirectoryURL = acmeProductionURL
		} else {
			input.DirectoryURL = acmeDirectoryURL
		}
	}
	if issuer == "letsencrypt" && challenge == "dns-01" {
		input.Cloudflare = &configuration.CloudflareInput{
			APIToken: stringValue(raw.cfAPIToken),
			ZoneID:   stringValue(raw.cfZoneID),
			ZoneName: stringValue(raw.cfZoneName),
		}
	}
	return input, nil
}

func parseCertHostnames(value any) ([]string, error) {
	invalid := &httpError{status: http.StatusBadRequest, message: "Enter at least one valid certificate hostname"}
	items, ok := value.([]any)
	if !ok {
		return nil, invalid
	}
	if len(items) == 0 || len(items) > 100 {
		return nil, invalid
	}
	hostnames := make([]string, 0, len(items))
	for _, item := range items {
		hostname, ok := item.(string)
		if !ok || strings.TrimSpace(hostname) == "" {
			return nil, invalid
		}
		hostnames = append(hostnames, strings.TrimSpace(hostname))
	}
	return hostnames, nil
}

func stringValue(value any) string {
	if s, ok := value.(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

func isEmptyValue(value any) bool {
	switch v := value.(type) {
	case nil:
		return true
	case string:
		return v == ""
	default:
		return false
	}
}

func isIntegerValue(value any) bool {
	switch v := value.(type) {
	case nil:
		return true // unset defaults to 30, which is an integer
	case string:
		if v == "" {
			return true
		}
		parsed := jsNumber(v)
		return parsed == float64(int(parsed))
	case float64:
		return v == float64(int(v))
	default:
		return false
	}
}
