package enrollment_test

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/acme"
	"github.com/zerkc/ProxyCore/apps/api/internal/enrollment"
)

type providerFixture struct {
	ca, first, second acme.Material
}

func newProviderFixture(t *testing.T) providerFixture {
	t.Helper()
	ca, err := acme.CreateInternalCA(30)
	if err != nil {
		t.Fatal(err)
	}
	issue := func() acme.Material {
		leaf, err := acme.IssueSignedByCA([]string{"primary.example"}, 30, ca.CertificatePEM, ca.PrivateKeyPEM)
		if err != nil {
			t.Fatal(err)
		}
		return leaf
	}
	return providerFixture{ca: ca, first: issue(), second: issue()}
}

func (f providerFixture) material(leaf acme.Material, ca string) enrollment.TLSCertificateMaterial {
	return enrollment.TLSCertificateMaterial{CertificatePEM: leaf.CertificatePEM, PrivateKeyPEM: leaf.PrivateKeyPEM, CACertificatePEM: ca}
}

func providerSerial(t *testing.T, provider *enrollment.TLSCertificateProvider) string {
	t.Helper()
	certificate, err := provider.GetCertificate(nil)
	if err != nil || len(certificate.Certificate) == 0 {
		t.Fatalf("get certificate: %v", err)
	}
	leaf, err := x509.ParseCertificate(certificate.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	return leaf.SerialNumber.String()
}

type chainFixture struct {
	rootPEM, intermediatePEM, otherIntermediatePEM string
	rootCert                                       *x509.Certificate
	rootKey                                        *rsa.PrivateKey
	leaf                                           acme.Material
}

func newChainFixture(t *testing.T) chainFixture {
	t.Helper()
	rootPEM, _, rootCert, rootKey := issueRoot(t)
	intermediatePEM, intermediateKeyPEM := issueIntermediate(t, rootCert, rootKey, "Intermediate A")
	otherIntermediatePEM, _ := issueIntermediate(t, rootCert, rootKey, "Intermediate B")
	leaf := issueLeaf(t, parseCert(t, intermediatePEM), parseRSAKey(t, intermediateKeyPEM))
	return chainFixture{rootPEM, intermediatePEM, otherIntermediatePEM, rootCert, rootKey, leaf}
}

func parseCert(t *testing.T, value string) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode([]byte(value))
	if block == nil {
		t.Fatal("certificate PEM did not decode")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return certificate
}

func parseRSAKey(t *testing.T, value string) *rsa.PrivateKey {
	t.Helper()
	block, _ := pem.Decode([]byte(value))
	if block == nil {
		t.Fatal("key PEM did not decode")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		t.Fatal("key is not RSA")
	}
	return key
}

func issueRoot(t *testing.T) (string, string, *x509.Certificate, *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Test Root"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(30 * 24 * time.Hour), KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, BasicConstraintsValid: true, IsCA: true, MaxPathLen: 1}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certificatePEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	return certificatePEM, keyPEM(t, key), parseCert(t, certificatePEM), key
}

func issueIntermediate(t *testing.T, root *x509.Certificate, rootKey *rsa.PrivateKey, name string) (string, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	template := &x509.Certificate{SerialNumber: big.NewInt(int64(len(name))), Subject: pkix.Name{CommonName: name}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(29 * 24 * time.Hour), KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, BasicConstraintsValid: true, IsCA: true, MaxPathLen: 0}
	der, err := x509.CreateCertificate(rand.Reader, template, root, &key.PublicKey, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), keyPEM(t, key)
}

func issueLeaf(t *testing.T, issuer *x509.Certificate, issuerKey crypto.Signer) acme.Material {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	template := &x509.Certificate{SerialNumber: big.NewInt(99), Subject: pkix.Name{CommonName: "primary.example"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(29 * 24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true, DNSNames: []string{"primary.example"}}
	der, err := x509.CreateCertificate(rand.Reader, template, issuer, &key.PublicKey, issuerKey)
	if err != nil {
		t.Fatal(err)
	}
	return acme.Material{CertificatePEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), PrivateKeyPEM: keyPEM(t, key)}
}

func issueECDSALeaf(t *testing.T, issuer *x509.Certificate, issuerKey *rsa.PrivateKey) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	template := &x509.Certificate{SerialNumber: big.NewInt(100), Subject: pkix.Name{CommonName: "primary.example"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(29 * 24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true, DNSNames: []string{"primary.example"}}
	der, err := x509.CreateCertificate(rand.Reader, template, issuer, &key.PublicKey, issuerKey)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), keyPEM(t, key)
}

func keyPEM(t *testing.T, key any) string {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

type providerSnapshot struct {
	chain        [][]byte
	leaf         []byte
	materialHash [3][32]byte
}

func providerSnapshotOf(t *testing.T, provider *enrollment.TLSCertificateProvider) providerSnapshot {
	t.Helper()
	certificate, err := provider.GetCertificate(nil)
	if err != nil || certificate == nil || certificate.PrivateKey == nil {
		t.Fatalf("get certificate: %v", err)
	}
	chain := cloneDER(certificate.Certificate)
	leaf, err := provider.Certificate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	leafPEM, keyPEM, caPEM, err := provider.Material(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return providerSnapshot{chain: chain, leaf: append([]byte(nil), leaf.Raw...), materialHash: [3][32]byte{sha256.Sum256([]byte(leafPEM)), sha256.Sum256([]byte(keyPEM)), sha256.Sum256([]byte(caPEM))}}
}

func sameProviderSnapshot(first, second providerSnapshot) bool {
	if !bytes.Equal(first.leaf, second.leaf) || first.materialHash != second.materialHash || len(first.chain) != len(second.chain) {
		return false
	}
	for i := range first.chain {
		if !bytes.Equal(first.chain[i], second.chain[i]) {
			return false
		}
	}
	return true
}

func cloneDER(chain [][]byte) [][]byte {
	cloned := make([][]byte, len(chain))
	for i := range chain {
		cloned[i] = append([]byte(nil), chain[i]...)
	}
	return cloned
}

func issueECDSARoot(t *testing.T) (string, string, *x509.Certificate, crypto.Signer) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	template := &x509.Certificate{SerialNumber: big.NewInt(201), Subject: pkix.Name{CommonName: "ECDSA Root"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(30 * 24 * time.Hour), KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, BasicConstraintsValid: true, IsCA: true, MaxPathLen: 1}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certificatePEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	return certificatePEM, keyPEM(t, key), parseCert(t, certificatePEM), key
}

func issueECDSAIntermediate(t *testing.T, root *x509.Certificate, rootKey crypto.Signer) (string, string, crypto.Signer) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	template := &x509.Certificate{SerialNumber: big.NewInt(202), Subject: pkix.Name{CommonName: "ECDSA Intermediate"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(29 * 24 * time.Hour), KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, BasicConstraintsValid: true, IsCA: true, MaxPathLen: 0}
	der, err := x509.CreateCertificate(rand.Reader, template, root, &key.PublicKey, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), keyPEM(t, key), key
}
