package acme

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"
)

func TestInternalCASignsLeafAndVerifies(t *testing.T) {
	ca, err := CreateInternalCA(3650)
	if err != nil {
		t.Fatalf("CreateInternalCA: %v", err)
	}
	leaf, err := IssueSignedByCA([]string{"app.home.arpa", "*.home.arpa"}, 365, ca.CertificatePEM, ca.PrivateKeyPEM)
	if err != nil {
		t.Fatalf("IssueSignedByCA: %v", err)
	}

	caBlock, _ := pem.Decode([]byte(ca.CertificatePEM))
	if caBlock == nil {
		t.Fatal("ca pem decode failed")
	}
	caCert, err := x509.ParseCertificate(caBlock.Bytes)
	if err != nil {
		t.Fatalf("parse ca: %v", err)
	}
	leafBlock, _ := pem.Decode([]byte(leaf.CertificatePEM))
	if leafBlock == nil {
		t.Fatal("leaf pem decode failed")
	}
	leafCert, err := x509.ParseCertificate(leafBlock.Bytes)
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}

	roots := x509.NewCertPool()
	roots.AddCert(caCert)
	if _, err := leafCert.Verify(x509.VerifyOptions{
		DNSName: "app.home.arpa",
		Roots:   roots,
	}); err != nil {
		t.Fatalf("leaf should verify against CA: %v", err)
	}
}

func TestIssueSignedByCARejectsBadCA(t *testing.T) {
	_, err := IssueSignedByCA([]string{"app.home.arpa"}, 365, "not-a-cert", "not-a-key")
	if err == nil {
		t.Fatal("expected error for invalid CA material")
	}
}

func TestDERHashSHA256IsDeterministicAndPEMRoundTrips(t *testing.T) {
	der := []byte{0x30, 0x03, 0x02, 0x01, 0x01}
	const want = "1b65f68a522c858715f5dd951cd0402dc16691778814bf0759822b7a257421d0"
	if got := DERHashSHA256(der); got != want {
		t.Fatalf("DERHashSHA256=%q, want %q", got, want)
	}
	ca, err := CreateInternalCA(3650)
	if err != nil {
		t.Fatalf("CreateInternalCA: %v", err)
	}
	got, err := CertificateDERHashSHA256(ca.CertificatePEM)
	if err != nil {
		t.Fatalf("CertificateDERHashSHA256: %v", err)
	}
	block, _ := pem.Decode([]byte(ca.CertificatePEM))
	if block == nil {
		t.Fatal("ca pem decode failed")
	}
	if got != DERHashSHA256(block.Bytes) {
		t.Fatalf("PEM fingerprint=%q, DER fingerprint=%q", got, DERHashSHA256(block.Bytes))
	}
	if got != strings.ToLower(got) {
		t.Fatalf("fingerprint is not lowercase: %q", got)
	}
	if _, err := CertificateDERHashSHA256(ca.CertificatePEM + ca.PrivateKeyPEM); err == nil {
		t.Fatal("fingerprinted PEM containing private-key material")
	}
}

func TestValidateInternalCAMaterialRejectsNonCanonicalPEMAndMislabeledDER(t *testing.T) {
	ca, err := CreateInternalCA(3650)
	if err != nil {
		t.Fatalf("CreateInternalCA: %v", err)
	}
	certBlock, _ := pem.Decode([]byte(ca.CertificatePEM))
	keyBlock, _ := pem.Decode([]byte(ca.PrivateKeyPEM))
	if certBlock == nil || keyBlock == nil {
		t.Fatal("canonical CA material did not decode")
	}
	cases := []struct {
		name string
		cert string
		key  string
	}{
		{"leading certificate junk", "junk" + ca.CertificatePEM, ca.PrivateKeyPEM},
		{"leading certificate whitespace", "\n" + ca.CertificatePEM, ca.PrivateKeyPEM},
		{"leading key junk", ca.CertificatePEM, "junk" + ca.PrivateKeyPEM},
		{"leading key whitespace", ca.CertificatePEM, "\n" + ca.PrivateKeyPEM},
		{"private DER relabeled certificate", string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: keyBlock.Bytes})), ca.PrivateKeyPEM},
		{"PKCS8 DER relabeled EC key", ca.CertificatePEM, string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBlock.Bytes}))},
		{"PKCS8 DER relabeled unknown key", ca.CertificatePEM, string(pem.EncodeToMemory(&pem.Block{Type: "NOT A KEY", Bytes: keyBlock.Bytes}))},
		{"PKCS8 DER relabeled certificate key", ca.CertificatePEM, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: keyBlock.Bytes}))},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if err := ValidateInternalCAMaterial(tt.cert, tt.key); err == nil {
				t.Fatal("accepted non-canonical or mislabeled CA material")
			}
		})
	}
}

func TestValidateInternalCAMaterialAcceptsCanonicalPersistedKeyTypes(t *testing.T) {
	ca, err := CreateInternalCA(3650)
	if err != nil {
		t.Fatalf("CreateInternalCA: %v", err)
	}
	if err := ValidateInternalCAMaterial(ca.CertificatePEM, ca.PrivateKeyPEM); err != nil {
		t.Fatalf("canonical PKCS8 material rejected: %v", err)
	}
	keyBlock, _ := pem.Decode([]byte(ca.PrivateKeyPEM))
	parsed, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	if err != nil {
		t.Fatalf("parse canonical PKCS8 key: %v", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		t.Fatal("canonical CA key is not RSA")
	}
	rsaPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := ValidateInternalCAMaterial(ca.CertificatePEM, string(rsaPEM)); err != nil {
		t.Fatalf("canonical RSA PKCS1 material rejected: %v", err)
	}
}

func TestValidateInternalCAMaterialRejectsNonRSAPKCS8Keys(t *testing.T) {
	for _, kind := range []string{"ECDSA", "Ed25519"} {
		t.Run(kind, func(t *testing.T) {
			certPEM, keyPEM := nonRSACAMaterial(t, kind)
			if err := ValidateInternalCAMaterial(certPEM, keyPEM); err == nil {
				t.Fatal("accepted a non-RSA PKCS8 CA key")
			}
		})
	}
}

func nonRSACAMaterial(t *testing.T, kind string) (string, string) {
	t.Helper()
	var public any
	var signer crypto.Signer
	switch kind {
	case "ECDSA":
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		public, signer = &key.PublicKey, key
	case "Ed25519":
		publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		public, signer = publicKey, privateKey
	default:
		t.Fatalf("unsupported test key kind %q", kind)
	}
	now := time.Now().UTC()
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: kind + " CA"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, template, template, public, signer)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(signer)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
}

func TestValidateInternalCAMaterialRejectsTamperedSignature(t *testing.T) {
	ca, err := CreateInternalCA(3650)
	if err != nil {
		t.Fatalf("CreateInternalCA: %v", err)
	}
	block, _ := pem.Decode([]byte(ca.CertificatePEM))
	der := append([]byte(nil), block.Bytes...)
	der[len(der)-1] ^= 1
	if _, err := x509.ParseCertificate(der); err != nil {
		t.Fatalf("tampered certificate became syntactically invalid: %v", err)
	}
	tampered := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := ValidateInternalCAMaterial(string(tampered), ca.PrivateKeyPEM); err == nil {
		t.Fatal("accepted an internal CA with a tampered signature")
	}
	if err := ValidateInternalCAMaterial(ca.CertificatePEM+ca.PrivateKeyPEM, ca.PrivateKeyPEM); err == nil {
		t.Fatal("accepted an internal CA PEM containing private-key material")
	}
	if err := ValidateInternalCAMaterial(ca.CertificatePEM, ca.PrivateKeyPEM+ca.PrivateKeyPEM); err == nil {
		t.Fatal("accepted an internal CA key PEM containing extra private-key material")
	}
}
