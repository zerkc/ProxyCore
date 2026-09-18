package enrollment_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"sync"
	"testing"

	"github.com/zerkc/ProxyCore/apps/api/internal/enrollment"
)

func TestTLSCertificateProviderPublishesCurrentCertificate(t *testing.T) {
	f := newProviderFixture(t)
	provider, err := enrollment.NewTLSCertificateProvider(f.material(f.first, f.ca.CertificatePEM))
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := provider.GetCertificate(nil)
	if err != nil || len(certificate.Certificate) != 2 || certificate.PrivateKey == nil {
		t.Fatalf("published certificate = %#v, err = %v", certificate, err)
	}
	leaf, _ := x509.ParseCertificate(certificate.Certificate[0])
	ca, _ := x509.ParseCertificate(certificate.Certificate[1])
	if leaf == nil || ca == nil || leaf.CheckSignatureFrom(ca) != nil {
		t.Fatal("published chain is not leaf signed by its CA")
	}
	parsed, err := provider.Certificate(context.Background())
	if err != nil || !bytes.Equal(parsed.Raw, leaf.Raw) {
		t.Fatalf("current leaf = %v, want published leaf; err = %v", parsed, err)
	}
	leafPEM, keyPEM, caPEM, err := provider.Material(context.Background())
	if err != nil || leafPEM != f.first.CertificatePEM || keyPEM != f.first.PrivateKeyPEM || caPEM != f.ca.CertificatePEM {
		t.Fatalf("current material mismatch: err=%v", err)
	}
	if _, err := tls.X509KeyPair([]byte(leafPEM), []byte(keyPEM)); err != nil {
		t.Fatalf("published key pair is unusable: %v", err)
	}
}

func TestTLSCertificateProviderUninitializedFailsClosed(t *testing.T) {
	var provider enrollment.TLSCertificateProvider
	if _, err := provider.GetCertificate(nil); !errors.Is(err, enrollment.ErrTLSCertificateUnavailable) {
		t.Fatalf("uninitialized GetCertificate error = %v", err)
	}
	if _, _, _, err := provider.Material(context.Background()); !errors.Is(err, enrollment.ErrTLSCertificateUnavailable) {
		t.Fatalf("uninitialized Material error = %v", err)
	}
	if _, err := enrollment.NewTLSCertificateProvider(enrollment.TLSCertificateMaterial{PrivateKeyPEM: "secret"}); !errors.Is(err, enrollment.ErrInvalidTLSCertificateMaterial) {
		t.Fatalf("invalid constructor error = %v", err)
	}
}

func TestTLSCertificateProviderReplacementIsAtomicForReaders(t *testing.T) {
	f := newProviderFixture(t)
	provider, err := enrollment.NewTLSCertificateProvider(f.material(f.first, f.ca.CertificatePEM))
	if err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, 1)
	var group sync.WaitGroup
	check := func(err error) {
		select {
		case errs <- err:
		default:
		}
	}
	for i := 0; i < 8; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for j := 0; j < 100; j++ {
				certificate, err := provider.GetCertificate(nil)
				if err != nil || certificate.PrivateKey == nil || len(certificate.Certificate) != 2 {
					check(errors.New("reader observed incomplete certificate"))
					return
				}
				leaf, leafErr := x509.ParseCertificate(certificate.Certificate[0])
				ca, caErr := x509.ParseCertificate(certificate.Certificate[1])
				if leafErr != nil || caErr != nil || leaf.CheckSignatureFrom(ca) != nil {
					check(errors.New("reader observed mixed certificate chain"))
					return
				}
			}
		}()
	}
	group.Add(1)
	go func() {
		defer group.Done()
		for i := 0; i < 100; i++ {
			if err := provider.Replace(f.material(f.second, f.ca.CertificatePEM)); err != nil {
				check(err)
			}
			if err := provider.Replace(f.material(f.first, f.ca.CertificatePEM)); err != nil {
				check(err)
			}
		}
	}()
	group.Wait()
	select {
	case err := <-errs:
		t.Fatal(err)
	default:
	}
}

func TestTLSCertificateProviderAcceptsAndPreservesOrderedIntermediateChain(t *testing.T) {
	f := newChainFixture(t)
	chain := f.intermediatePEM + f.rootPEM
	provider, err := enrollment.NewTLSCertificateProvider(enrollment.TLSCertificateMaterial{CertificatePEM: f.leaf.CertificatePEM, PrivateKeyPEM: f.leaf.PrivateKeyPEM, CACertificatePEM: chain})
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := provider.GetCertificate(nil)
	if err != nil || len(certificate.Certificate) != 3 {
		t.Fatalf("ordered chain = %#v, err = %v", certificate, err)
	}
	want := [][]byte{parseCert(t, f.leaf.CertificatePEM).Raw, parseCert(t, f.intermediatePEM).Raw, f.rootCert.Raw}
	for i := range want {
		if !bytes.Equal(certificate.Certificate[i], want[i]) {
			t.Fatalf("chain[%d] is out of order", i)
		}
	}
}

func TestTLSCertificateProviderRejectsAmbiguousChainOrderAndPreservesOld(t *testing.T) {
	f := newChainFixture(t)
	valid := enrollment.TLSCertificateMaterial{CertificatePEM: f.leaf.CertificatePEM, PrivateKeyPEM: f.leaf.PrivateKeyPEM, CACertificatePEM: f.intermediatePEM + f.rootPEM}
	provider, err := enrollment.NewTLSCertificateProvider(valid)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := provider.GetCertificate(nil)
	cases := map[string]string{
		"reversed":                f.rootPEM + f.intermediatePEM,
		"missing intermediate":    f.rootPEM,
		"unrelated intermediate":  f.otherIntermediatePEM + f.intermediatePEM + f.rootPEM,
		"duplicated intermediate": f.intermediatePEM + f.intermediatePEM + f.rootPEM,
		"ambiguous order":         f.rootPEM + f.intermediatePEM + f.rootPEM,
	}
	for name, chain := range cases {
		t.Run(name, func(t *testing.T) {
			if err := provider.Replace(enrollment.TLSCertificateMaterial{CertificatePEM: f.leaf.CertificatePEM, PrivateKeyPEM: f.leaf.PrivateKeyPEM, CACertificatePEM: chain}); err == nil {
				t.Fatal("invalid chain was accepted")
			}
			after, err := provider.GetCertificate(nil)
			if err != nil || len(after.Certificate) != len(before.Certificate) {
				t.Fatalf("previous certificate unavailable: %v", err)
			}
			for i := range before.Certificate {
				if !bytes.Equal(before.Certificate[i], after.Certificate[i]) {
					t.Fatalf("failed replacement changed chain[%d]", i)
				}
			}
		})
	}
}
