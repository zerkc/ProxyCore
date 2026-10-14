package configuration

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/acme"
)

type trustCertOptions struct {
	isCA     bool
	notAfter time.Time
}

func trustTestCert(t *testing.T, o trustCertOptions) (certPEM string, der []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if o.notAfter.IsZero() {
		o.notAfter = time.Now().Add(24 * time.Hour)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: "primary test ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              o.notAfter,
		BasicConstraintsValid: true,
		IsCA:                  o.isCA,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err = x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), der
}

func TestNormalizePrimaryTrustCAAcceptsACAAndFingerprintsItLikeThePrimary(t *testing.T) {
	caPEM, der := trustTestCert(t, trustCertOptions{isCA: true})
	got, fingerprint, err := NormalizePrimaryTrustCA("  \n" + caPEM + "\n ")
	if err != nil {
		t.Fatalf("NormalizePrimaryTrustCA: %v", err)
	}
	if fingerprint != acme.DERHashSHA256(der) {
		t.Fatalf("fingerprint = %q, want the primary's DER SHA-256 %q", fingerprint, acme.DERHashSHA256(der))
	}
	if got != caPEM {
		t.Fatalf("canonical PEM changed:\n%q\nwant\n%q", got, caPEM)
	}
}

func TestNormalizePrimaryTrustCARejectsUnsafeInput(t *testing.T) {
	caPEM, _ := trustTestCert(t, trustCertOptions{isCA: true})
	leafPEM, _ := trustTestCert(t, trustCertOptions{isCA: false})
	expiredPEM, _ := trustTestCert(t, trustCertOptions{isCA: true, notAfter: time.Now().Add(-time.Minute)})
	keyBlock := string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: []byte("secret")}))
	cases := map[string]string{
		"empty":            "",
		"not pem":          "hello",
		"not a cert":       "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n",
		"leaf certificate": leafPEM,
		"expired":          expiredPEM,
		"private key":      caPEM + keyBlock,
		"trailing data":    caPEM + "junk",
		"too many":         strings.Repeat(caPEM, 5),
		"oversize":         caPEM + strings.Repeat("A", 70<<10),
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, err := NormalizePrimaryTrustCA(input); !errors.Is(err, ErrPrimaryTrustInvalid) {
				t.Fatalf("err = %v, want ErrPrimaryTrustInvalid", err)
			}
		})
	}
}

func primaryTrustFixture(t *testing.T) *SQLitePrimaryTrustStore {
	t.Helper()
	db, _, _ := sqliteSettingsFixture(t)
	return NewSQLitePrimaryTrustStore(settingsWriter(db))
}

func TestPrimaryTrustStoreRoundTripsAndReplacesPerOrigin(t *testing.T) {
	store := primaryTrustFixture(t)
	ctx := context.Background()
	origin := "https://192.168.1.2:3443"
	if _, ok, err := store.Get(ctx, origin); err != nil || ok {
		t.Fatalf("Get before Put ok=%v err=%v", ok, err)
	}
	first, _ := trustTestCert(t, trustCertOptions{isCA: true})
	stored, err := store.Put(ctx, origin, first)
	if err != nil || stored.Origin != origin || stored.CAPEM != first || stored.Fingerprint == "" {
		t.Fatalf("Put = %+v err=%v", stored, err)
	}
	got, ok, err := store.Get(ctx, origin)
	if err != nil || !ok || got.CAPEM != first || got.Fingerprint != stored.Fingerprint {
		t.Fatalf("Get = %+v ok=%v err=%v", got, ok, err)
	}

	// Re-enrolling against a rebuilt primary at the same address replaces the CA.
	second, _ := trustTestCert(t, trustCertOptions{isCA: true})
	replaced, err := store.Put(ctx, origin, second)
	if err != nil || replaced.Fingerprint == stored.Fingerprint {
		t.Fatalf("replace = %+v err=%v", replaced, err)
	}
	got, _, _ = store.Get(ctx, origin)
	if got.CAPEM != second {
		t.Fatalf("CA after replace = %q, want the new one", got.CAPEM)
	}

	// Trust never leaks across origins.
	if _, ok, _ := store.Get(ctx, "https://192.168.1.9:3443"); ok {
		t.Fatal("CA for one origin was returned for another")
	}
}

func TestPrimaryTrustStoreRejectsBadInputWithoutTouchingStoredCA(t *testing.T) {
	store := primaryTrustFixture(t)
	ctx := context.Background()
	origin := "https://primary.example:3443"
	good, _ := trustTestCert(t, trustCertOptions{isCA: true})
	if _, err := store.Put(ctx, origin, good); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(ctx, origin, "garbage"); !errors.Is(err, ErrPrimaryTrustInvalid) {
		t.Fatalf("bad CA err = %v, want ErrPrimaryTrustInvalid", err)
	}
	for _, badOrigin := range []string{"", "http://primary.example", "https://", "https://user@primary.example", "https://primary.example/path"} {
		if _, err := store.Put(ctx, badOrigin, good); !errors.Is(err, ErrPrimaryTrustInvalid) {
			t.Fatalf("origin %q err = %v, want ErrPrimaryTrustInvalid", badOrigin, err)
		}
	}
	got, ok, err := store.Get(ctx, origin)
	if err != nil || !ok || got.CAPEM != good {
		t.Fatalf("stored CA changed after rejected writes: %+v ok=%v err=%v", got, ok, err)
	}
}
