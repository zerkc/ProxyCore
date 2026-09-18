package enrollment_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/zerkc/ProxyCore/apps/api/internal/acme"
	"github.com/zerkc/ProxyCore/apps/api/internal/enrollment"
)

func TestTLSCertificateProviderRejectsAndPreservesInvalidReplacement(t *testing.T) {
	f := newProviderFixture(t)
	otherCA, err := acme.CreateInternalCA(30)
	if err != nil {
		t.Fatal(err)
	}
	provider, err := enrollment.NewTLSCertificateProvider(f.material(f.first, f.ca.CertificatePEM))
	if err != nil {
		t.Fatal(err)
	}
	before := providerSnapshotOf(t, provider)
	cases := []struct {
		name     string
		material enrollment.TLSCertificateMaterial
	}{
		{"empty", enrollment.TLSCertificateMaterial{}},
		{"mismatched key", enrollment.TLSCertificateMaterial{CertificatePEM: f.first.CertificatePEM, PrivateKeyPEM: f.second.PrivateKeyPEM, CACertificatePEM: f.ca.CertificatePEM}},
		{"wrong chain", f.material(f.first, otherCA.CertificatePEM)},
		{"malformed chain", enrollment.TLSCertificateMaterial{CertificatePEM: f.first.CertificatePEM, PrivateKeyPEM: f.first.PrivateKeyPEM, CACertificatePEM: "not-a-certificate"}},
		{"malformed secret", enrollment.TLSCertificateMaterial{CertificatePEM: f.first.CertificatePEM, PrivateKeyPEM: "leak-private-key", CACertificatePEM: f.ca.CertificatePEM}},
		{"extra certificate", enrollment.TLSCertificateMaterial{CertificatePEM: f.first.CertificatePEM + f.ca.CertificatePEM, PrivateKeyPEM: f.first.PrivateKeyPEM, CACertificatePEM: f.ca.CertificatePEM}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			err := provider.Replace(test.material)
			if err == nil || strings.Contains(err.Error(), "leak-private-key") || strings.Contains(err.Error(), "BEGIN") {
				t.Fatalf("replacement error = %v", err)
			}
			if !sameProviderSnapshot(before, providerSnapshotOf(t, provider)) {
				t.Fatal("failed replacement changed the published provider snapshot")
			}
		})
	}
}

func TestTLSCertificateProviderRejectsECDSALeaf(t *testing.T) {
	f := newChainFixture(t)
	leafPEM, keyPEM := issueECDSALeaf(t, f.rootCert, f.rootKey)
	_, err := enrollment.NewTLSCertificateProvider(enrollment.TLSCertificateMaterial{CertificatePEM: leafPEM, PrivateKeyPEM: keyPEM, CACertificatePEM: f.rootPEM})
	if !errors.Is(err, enrollment.ErrUnsupportedTLSCertificateKey) {
		t.Fatalf("ECDSA error = %v", err)
	}
}

func TestTLSCertificateProviderRejectsECDSACertificateAuthoritiesAndPreservesState(t *testing.T) {
	f := newChainFixture(t)
	valid := enrollment.TLSCertificateMaterial{CertificatePEM: f.leaf.CertificatePEM, PrivateKeyPEM: f.leaf.PrivateKeyPEM, CACertificatePEM: f.intermediatePEM + f.rootPEM}
	provider, err := enrollment.NewTLSCertificateProvider(valid)
	if err != nil {
		t.Fatal(err)
	}
	before := providerSnapshotOf(t, provider)
	ecdsaRootPEM, _, ecdsaRoot, ecdsaRootKey := issueECDSARoot(t)
	ecdsaRootLeaf := issueLeaf(t, ecdsaRoot, ecdsaRootKey)
	ecdsaIntermediatePEM, _, ecdsaIntermediateKey := issueECDSAIntermediate(t, f.rootCert, f.rootKey)
	ecdsaIntermediateLeaf := issueLeaf(t, parseCert(t, ecdsaIntermediatePEM), ecdsaIntermediateKey)
	cases := []enrollment.TLSCertificateMaterial{
		{CertificatePEM: ecdsaRootLeaf.CertificatePEM, PrivateKeyPEM: ecdsaRootLeaf.PrivateKeyPEM, CACertificatePEM: ecdsaRootPEM},
		{CertificatePEM: ecdsaIntermediateLeaf.CertificatePEM, PrivateKeyPEM: ecdsaIntermediateLeaf.PrivateKeyPEM, CACertificatePEM: ecdsaIntermediatePEM + f.rootPEM},
	}
	for i, material := range cases {
		t.Run("ecdsa-ca-"+string(rune('a'+i)), func(t *testing.T) {
			if err := provider.Replace(material); !errors.Is(err, enrollment.ErrUnsupportedTLSCertificateKey) {
				t.Fatalf("ECDSA CA error = %v", err)
			}
			if !sameProviderSnapshot(before, providerSnapshotOf(t, provider)) {
				t.Fatal("failed ECDSA replacement changed the published snapshot")
			}
		})
	}
}
