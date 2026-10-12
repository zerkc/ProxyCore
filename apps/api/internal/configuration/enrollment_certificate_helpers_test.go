package configuration

import (
	"crypto"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"testing"
	"time"
)

func variantCertificate(t *testing.T, caPEM, caKeyPEM, leafPEM string, expired, malformed bool) string {
	caPair, err := tls.X509KeyPair([]byte(caPEM), []byte(caKeyPEM))
	if err != nil {
		t.Fatalf("parse CA pair: %v", err)
	}
	ca, err := x509.ParseCertificate(caPair.Certificate[0])
	if err != nil {
		t.Fatalf("parse CA: %v", err)
	}
	leaf := parseEnrollmentCertificate(t, leafPEM)
	template := *leaf
	now := time.Now().UTC()
	template.NotBefore, template.NotAfter = now.Add(-2*time.Hour), now.Add(time.Hour)
	template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	if expired {
		template.NotAfter, template.ExtKeyUsage = now.Add(-time.Minute), []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	}
	if malformed {
		template.DNSNames, template.IPAddresses = []string{"192.0.2.10"}, nil
	}
	signer := caPair.PrivateKey.(crypto.Signer)
	der, err := x509.CreateCertificate(rand.Reader, &template, ca, leaf.PublicKey, signer)
	if err != nil {
		t.Fatalf("create variant: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func parseEnrollmentCertificate(t *testing.T, certificatePEM string) *x509.Certificate {
	block, _ := pem.Decode([]byte(certificatePEM))
	if block == nil {
		t.Fatal("certificate PEM decode failed")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse enrollment certificate: %v", err)
	}
	return certificate
}
func enrollmentSPKI(t *testing.T, certificate *x509.Certificate) []byte {
	spki, err := x509.MarshalPKIXPublicKey(certificate.PublicKey)
	if err != nil {
		t.Fatalf("marshal SPKI: %v", err)
	}
	return spki
}

func TestEnrollmentSANMatchingRejectsMalformedAndIncompleteNames(t *testing.T) {
	if sameEnrollmentHostnames(&x509.Certificate{DNSNames: []string{"192.0.2.10"}}, []string{"192.0.2.10"}) {
		t.Fatal("accepted an IP encoded as a DNS SAN")
	}
	if sameEnrollmentHostnames(&x509.Certificate{DNSNames: []string{"enroll.example"}}, []string{"enroll.example", "192.0.2.10"}) {
		t.Fatal("accepted incomplete configured SAN coverage")
	}
}
