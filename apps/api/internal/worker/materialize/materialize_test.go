package materialize

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
)

// mapStore is an in-memory SecretReader; a missing id yields "" like the SQLite store.
type mapStore map[string]string

func (m mapStore) Get(_ context.Context, id string) (string, error) { return m[id], nil }

type failingStore struct{}

func (failingStore) Get(context.Context, string) (string, error) {
	return "", errors.New("store unavailable")
}

func proxied(proxy map[string]any) domain.DNSRecord {
	return domain.DNSRecord{ID: "r", Name: "a", Type: "A", Value: "1.1.1.1", TTL: 1, Enabled: true, Proxied: true, Proxy: proxy}
}

func tlsRecord(certID string) domain.DNSRecord {
	return proxied(map[string]any{"tlsEnabled": true, "certificateId": certID})
}

func TestFormatBasicAuthFileLine(t *testing.T) {
	got, err := FormatBasicAuthFileLine("a.b@c+d=e-1", "{SHA}abc")
	if err != nil || got != "a.b@c+d=e-1:{SHA}abc\n" {
		t.Fatalf("got (%q, %v)", got, err)
	}
}

func TestFormatBasicAuthFileLineRejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name, user, hash, want string
	}{
		{"empty username", "", "{SHA}abc", "Basic Auth username is invalid"},
		{"username with colon", "a:b", "{SHA}abc", "Basic Auth username is invalid"},
		{"username with newline", "ab\n", "{SHA}abc", "Basic Auth username is invalid"},
		{"username too long", strings.Repeat("a", 65), "{SHA}abc", "Basic Auth username is invalid"},
		{"hash without scheme", "op", "abc", "Basic Auth password hash is invalid"},
		{"hash too short", "op", "{SHA}", "Basic Auth password hash is invalid"},
		{"hash one char", "op", "{SHA}a", "Basic Auth password hash is invalid"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := FormatBasicAuthFileLine(tt.user, tt.hash)
			if err == nil || err.Error() != tt.want {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
	// "{SHA}" plus one character is length 6, which TS rejects (> 6 required).
	if _, err := FormatBasicAuthFileLine(strings.Repeat("a", 64), "{SHA}ab"); err != nil {
		t.Fatalf("64-char username and 7-char hash should be valid: %v", err)
	}
}

func TestBasicAuthFiles(t *testing.T) {
	store := mapStore{"s1": "{SHA}abc"}
	records := []domain.DNSRecord{
		proxied(map[string]any{"basicAuth": map[string]any{"username": "op", "passwordSecretId": "s1"}}),
		{},
		proxied(map[string]any{"tlsEnabled": true}),
	}
	got, err := BasicAuthFiles(context.Background(), records, store)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"basic-auth/s1": "op:{SHA}abc\n"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestBasicAuthFilesErrors(t *testing.T) {
	rec := proxied(map[string]any{"basicAuth": map[string]any{"username": "op", "passwordSecretId": "gone"}})
	if _, err := BasicAuthFiles(context.Background(), []domain.DNSRecord{rec}, mapStore{}); err == nil || err.Error() != "Basic Auth secret not found: gone" {
		t.Errorf("missing secret: err = %v", err)
	}
	if _, err := BasicAuthFiles(context.Background(), []domain.DNSRecord{rec}, failingStore{}); err == nil || err.Error() != "store unavailable" {
		t.Errorf("store error not propagated: %v", err)
	}
	bad := proxied(map[string]any{"basicAuth": map[string]any{"username": "no good", "passwordSecretId": "s1"}})
	_, err := BasicAuthFiles(context.Background(), []domain.DNSRecord{bad}, mapStore{"s1": "{SHA}abc"})
	if err == nil || err.Error() != "Basic Auth username is invalid" {
		t.Errorf("invalid username: err = %v", err)
	}
}

func TestCertificateFiles(t *testing.T) {
	store := mapStore{"k1": "KEY", "k2": "KEY2\n"}
	certs := []domain.CertificateStatus{
		{ID: "c1", CertificatePEM: "CERT", SecretID: "k1"},
		{ID: "c2", CertificatePEM: "CERT2\n", SecretID: "k2"},
	}
	disabled := tlsRecord("c9")
	disabled.Enabled = false
	notProxied := tlsRecord("c9")
	notProxied.Proxied = false
	noTLS := proxied(map[string]any{"tlsEnabled": false, "certificateId": "c9"})
	noCertID := proxied(map[string]any{"tlsEnabled": true})
	records := []domain.DNSRecord{tlsRecord("c1"), tlsRecord("c2"), tlsRecord("c1"), disabled, notProxied, noTLS, noCertID, {}}
	got, err := CertificateFiles(context.Background(), records, certs, store)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"certs/c1.crt": "CERT\n", "certs/c1.key": "KEY\n",
		"certs/c2.crt": "CERT2\n", "certs/c2.key": "KEY2\n",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestCertificateFilesErrors(t *testing.T) {
	records := []domain.DNSRecord{tlsRecord("c1")}
	tests := []struct {
		name  string
		certs []domain.CertificateStatus
		store SecretReader
		want  string
	}{
		{"unknown certificate", nil, mapStore{}, "Certificate not found: c1"},
		{"missing pem", []domain.CertificateStatus{{ID: "c1", SecretID: "k1"}}, mapStore{"k1": "KEY"}, "Certificate PEM is missing: c1"},
		{"missing secret id", []domain.CertificateStatus{{ID: "c1", CertificatePEM: "C"}}, mapStore{}, "Certificate private key is missing: c1"},
		{"secret absent", []domain.CertificateStatus{{ID: "c1", CertificatePEM: "C", SecretID: "k1"}}, mapStore{}, "Certificate key secret not found: k1"},
		{"store failure", []domain.CertificateStatus{{ID: "c1", CertificatePEM: "C", SecretID: "k1"}}, failingStore{}, "store unavailable"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := CertificateFiles(context.Background(), records, tt.certs, tt.store)
			if err == nil || err.Error() != tt.want {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
			if got != nil {
				t.Errorf("files must be nil on error, got %v", got)
			}
		})
	}
}

func TestErrorsDoNotLeakKeyMaterial(t *testing.T) {
	store := mapStore{"k1": "SUPER-SECRET-KEY"}
	certs := []domain.CertificateStatus{{ID: "c1", SecretID: "k1"}}
	_, err := CertificateFiles(context.Background(), []domain.DNSRecord{tlsRecord("c1")}, certs, store)
	if err == nil || strings.Contains(err.Error(), "SUPER-SECRET-KEY") {
		t.Fatalf("err = %v", err)
	}
}
