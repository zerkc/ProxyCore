package acme

import (
	"crypto/x509"
	"encoding/pem"
	"testing"
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
