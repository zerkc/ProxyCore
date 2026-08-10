package httpserver

import (
	"net/http"
	"testing"
)

const (
	testStagingURL    = "https://acme-staging.example/directory"
	testProductionURL = "https://acme-prod.example/directory"
)

func TestBuildCertificateInputSelfSigned(t *testing.T) {
	input, err := buildCertificateInput(certRawInput{
		hostnames: []any{"example.test"},
		issuer:    "self-signed",
		challenge: "none",
	}, testStagingURL, testProductionURL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if input.Issuer != "self-signed" || input.Challenge != "none" {
		t.Fatalf("input=%+v", input)
	}
	if input.Environment != "local" {
		t.Fatalf("environment=%q", input.Environment)
	}
	if input.KeyType != "rsa" {
		t.Fatalf("keyType=%q", input.KeyType)
	}
	if input.DirectoryURL != "" {
		t.Fatalf("directoryURL should be empty for self-signed, got %q", input.DirectoryURL)
	}
}

func TestBuildCertificateInputLetsEncryptDNS(t *testing.T) {
	input, err := buildCertificateInput(certRawInput{
		hostnames:          []any{"example.test", "www.example.test"},
		issuer:             "letsencrypt",
		challenge:          "dns-01",
		environment:        "production",
		email:              "ops@example.test",
		keyType:            "ecdsa",
		propagationSeconds: float64(45),
		cfAPIToken:         "cf-token",
		cfZoneID:           "zone-123",
		cfZoneName:         "example.test",
	}, testStagingURL, testProductionURL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if input.DirectoryURL != testProductionURL {
		t.Fatalf("directoryURL=%q", input.DirectoryURL)
	}
	if input.PropagationSeconds == nil || *input.PropagationSeconds != 45 {
		t.Fatalf("propagationSeconds=%v", input.PropagationSeconds)
	}
	if input.Cloudflare == nil || input.Cloudflare.APIToken != "cf-token" {
		t.Fatalf("cloudflare=%+v", input.Cloudflare)
	}
	if input.KeyType != "ecdsa" {
		t.Fatalf("keyType=%q", input.KeyType)
	}
}

func TestBuildCertificateInputDefaultsDNSPropagation(t *testing.T) {
	input, err := buildCertificateInput(certRawInput{
		hostnames:  []any{"example.test"},
		issuer:     "letsencrypt",
		challenge:  "dns-01",
		cfAPIToken: "cf-token",
	}, testStagingURL, testProductionURL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if input.Environment != "staging" || input.DirectoryURL != testStagingURL {
		t.Fatalf("environment=%q directoryURL=%q", input.Environment, input.DirectoryURL)
	}
	if input.PropagationSeconds == nil || *input.PropagationSeconds != 60 {
		t.Fatalf("propagationSeconds default should be 60, got %v", input.PropagationSeconds)
	}
}

func TestBuildCertificateInputValidationErrors(t *testing.T) {
	cases := []struct {
		name string
		raw  certRawInput
	}{
		{"no hostnames", certRawInput{hostnames: []any{}, issuer: "self-signed", challenge: "none"}},
		{"blank hostname", certRawInput{hostnames: []any{"   "}, issuer: "self-signed", challenge: "none"}},
		{"invalid issuer", certRawInput{hostnames: []any{"a.test"}, issuer: "bogus", challenge: "none"}},
		{"invalid challenge", certRawInput{hostnames: []any{"a.test"}, issuer: "letsencrypt", challenge: "bogus"}},
		{"self-signed with challenge", certRawInput{hostnames: []any{"a.test"}, issuer: "self-signed", challenge: "http-01"}},
		{"letsencrypt none challenge", certRawInput{hostnames: []any{"a.test"}, issuer: "letsencrypt", challenge: "none"}},
		{"invalid key type", certRawInput{hostnames: []any{"a.test"}, issuer: "self-signed", challenge: "none", keyType: "dsa"}},
		{"bad environment", certRawInput{hostnames: []any{"a.test"}, issuer: "self-signed", challenge: "none", environment: "prod"}},
		{"propagation too high", certRawInput{hostnames: []any{"a.test"}, issuer: "letsencrypt", challenge: "dns-01", propagationSeconds: float64(1000)}},
		{"uploaded missing pem", certRawInput{hostnames: []any{"a.test"}, issuer: "uploaded", challenge: "none"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := buildCertificateInput(tc.raw, testStagingURL, testProductionURL)
			assertHTTPStatus(t, err, http.StatusBadRequest)
		})
	}
}
