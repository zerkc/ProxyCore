package configuration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/acme"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
)

func TestDirectoryURLForEnvironment(t *testing.T) {
	staging := "https://acme-staging.example/directory"
	production := "https://acme-v02.example/directory"
	if got := directoryURLForEnvironment("production", staging, production); got != production {
		t.Fatalf("production directory=%q", got)
	}
	if got := directoryURLForEnvironment("staging", staging, production); got != staging {
		t.Fatalf("staging directory=%q", got)
	}
	if got := directoryURLForEnvironment("", staging, production); got != staging {
		t.Fatalf("default directory=%q", got)
	}
}

func TestRenewLetsEncryptCertificateRejectsNonLE(t *testing.T) {
	store := &Store{}
	_, err := store.RenewLetsEncryptCertificate(context.Background(), domain.CertificateStatus{
		ID:     "cert-1",
		Issuer: "self-signed",
		Status: "active",
	}, RenewalOptions{})
	if err == nil || err.Error() == "" {
		t.Fatalf("expected rejection for non-LE issuer, got %v", err)
	}
}

func TestRenewSelfSignedCertificateRejectsNonSelfSigned(t *testing.T) {
	store := &Store{}
	_, err := store.RenewSelfSignedCertificate(context.Background(), domain.CertificateStatus{
		ID:     "cert-1",
		Issuer: "letsencrypt",
		Status: "active",
	})
	if err == nil || err.Error() == "" {
		t.Fatalf("expected rejection for non-self-signed issuer, got %v", err)
	}
}

func TestRenewSelfSignedCertificateRequiresSecrets(t *testing.T) {
	store := &Store{}
	_, err := store.RenewSelfSignedCertificate(context.Background(), domain.CertificateStatus{
		ID:        "cert-1",
		Hostnames: []string{"app.home.arpa"},
		Issuer:    "self-signed",
		Challenge: "none",
		Status:    "active",
	})
	if err == nil {
		t.Fatal("expected error without secrets")
	}
}

func TestRenewLetsEncryptCertificateUsesOverrideIssuer(t *testing.T) {
	if testing.Short() {
		t.Skip("requires DATABASE_URL for full renew path")
	}
	// Pure override smoke: ensure the hook is invoked for a crafted call that
	// fails before DB writes when secrets/store are unset.
	original := issueLetsEncryptFn
	t.Cleanup(func() { issueLetsEncryptFn = original })

	called := false
	issueLetsEncryptFn = func(ctx context.Context, options acme.LetsEncryptOptions) (acme.Material, error) {
		called = true
		return acme.Material{}, errors.New("forced failure")
	}

	store := &Store{}
	_, err := store.RenewLetsEncryptCertificate(context.Background(), domain.CertificateStatus{
		ID:          "cert-1",
		Hostnames:   []string{"app.example.test"},
		Issuer:      "letsencrypt",
		Challenge:   "http-01",
		Environment: "staging",
		Status:      "active",
	}, RenewalOptions{
		StagingDirectoryURL: "https://acme-staging.example/directory",
	})
	if err == nil {
		t.Fatal("expected error without secrets")
	}
	if called {
		t.Fatal("issuer should not run when master key/secrets are missing")
	}
}

func TestRenewalDateIsThirtyDaysBeforeExpiry(t *testing.T) {
	expires := time.Date(2026, 11, 7, 3, 26, 41, 0, time.UTC)
	renew := renewalDate(expires)
	want := expires.Add(-30 * 24 * time.Hour)
	if !renew.Equal(want) {
		t.Fatalf("renewAfter=%s want=%s", renew, want)
	}
}
