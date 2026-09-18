package enrollment

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"strings"
	"sync/atomic"
	"time"
)

var (
	ErrInvalidTLSCertificateMaterial = errors.New("invalid TLS certificate material")
	ErrTLSCertificateUnavailable     = errors.New("TLS certificate is unavailable")
	ErrUnsupportedTLSCertificateKey  = errors.New("unsupported TLS certificate key")
)

// TLSCertificateMaterial is the leaf, private key, and ordered CA chain used
// by the enrollment HTTPS endpoint. CertificatePEM must contain one leaf;
// CACertificatePEM must contain one or more CA certificates. RSA is the
// only accepted leaf/key algorithm while enrollment proof signing is RSA.
type TLSCertificateMaterial struct {
	CertificatePEM   string
	PrivateKeyPEM    string
	CACertificatePEM string
}

type tlsCertificateSnapshot struct {
	certificate tls.Certificate
	leafDER     []byte
	material    TLSCertificateMaterial
}

// TLSCertificateProvider publishes complete, immutable TLS snapshots. A
// failed replacement is never stored, so concurrent handshakes see either
// the old complete material or the new complete material.
type TLSCertificateProvider struct {
	current atomic.Pointer[tlsCertificateSnapshot]
}

func NewTLSCertificateProvider(material TLSCertificateMaterial) (*TLSCertificateProvider, error) {
	provider := new(TLSCertificateProvider)
	if err := provider.Replace(material); err != nil {
		return nil, err
	}
	return provider, nil
}

func (p *TLSCertificateProvider) Replace(material TLSCertificateMaterial) error {
	if p == nil {
		return ErrTLSCertificateUnavailable
	}
	snapshot, err := buildTLSCertificateSnapshot(material)
	if err != nil {
		return err
	}
	p.current.Store(snapshot)
	return nil
}

// GetCertificate is suitable for tls.Config.GetCertificate.
func (p *TLSCertificateProvider) GetCertificate(_ *tls.ClientHelloInfo) (*tls.Certificate, error) {
	snapshot := p.load()
	if snapshot == nil {
		return nil, ErrTLSCertificateUnavailable
	}
	certificate := snapshot.certificate
	certificate.Certificate = cloneDERChain(snapshot.certificate.Certificate)
	return &certificate, nil
}

// Certificate returns a parsed copy of the current leaf for the proof-only
// handler's hostname/SAN check.
func (p *TLSCertificateProvider) Certificate(context.Context) (*x509.Certificate, error) {
	snapshot := p.load()
	if snapshot == nil {
		return nil, ErrTLSCertificateUnavailable
	}
	certificate, err := x509.ParseCertificate(snapshot.leafDER)
	if err != nil {
		return nil, ErrTLSCertificateUnavailable
	}
	return certificate, nil
}

// Material returns the current material for the identity proof signer.
func (p *TLSCertificateProvider) Material(context.Context) (string, string, string, error) {
	snapshot := p.load()
	if snapshot == nil {
		return "", "", "", ErrTLSCertificateUnavailable
	}
	return snapshot.material.CertificatePEM, snapshot.material.PrivateKeyPEM, snapshot.material.CACertificatePEM, nil
}

func (p *TLSCertificateProvider) load() *tlsCertificateSnapshot {
	if p == nil {
		return nil
	}
	return p.current.Load()
}

func buildTLSCertificateSnapshot(material TLSCertificateMaterial) (*tlsCertificateSnapshot, error) {
	if strings.TrimSpace(material.CertificatePEM) == "" || strings.TrimSpace(material.PrivateKeyPEM) == "" || strings.TrimSpace(material.CACertificatePEM) == "" {
		return nil, ErrInvalidTLSCertificateMaterial
	}
	leafDER, leaf, err := parseSingleTLSCertificate(material.CertificatePEM)
	if err != nil {
		return nil, ErrInvalidTLSCertificateMaterial
	}
	caDER, caCertificates, err := parseTLSCertificateChain(material.CACertificatePEM)
	if err != nil || len(caCertificates) == 0 {
		return nil, ErrInvalidTLSCertificateMaterial
	}
	pair, err := tls.X509KeyPair([]byte(material.CertificatePEM), []byte(material.PrivateKeyPEM))
	if err != nil || len(pair.Certificate) != 1 {
		return nil, ErrInvalidTLSCertificateMaterial
	}
	if _, ok := pair.PrivateKey.(*rsa.PrivateKey); !ok || !isRSACertificate(leaf) {
		return nil, ErrUnsupportedTLSCertificateKey
	}
	for _, certificate := range caCertificates {
		if !isRSACertificate(certificate) {
			return nil, ErrUnsupportedTLSCertificateKey
		}
	}
	if !validTLSServerLeaf(leaf) || !validTLSCertificateChain(leaf, caCertificates) {
		return nil, ErrInvalidTLSCertificateMaterial
	}
	pair.Certificate = append([][]byte{append([]byte(nil), leafDER...)}, caDER...)
	return &tlsCertificateSnapshot{certificate: pair, leafDER: leafDER, material: material}, nil
}

func parseSingleTLSCertificate(value string) ([]byte, *x509.Certificate, error) {
	block, rest := pem.Decode([]byte(value))
	if block == nil || block.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, nil, ErrInvalidTLSCertificateMaterial
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, nil, ErrInvalidTLSCertificateMaterial
	}
	return append([]byte(nil), block.Bytes...), certificate, nil
}

func parseTLSCertificateChain(value string) ([][]byte, []*x509.Certificate, error) {
	remaining := []byte(value)
	var ders [][]byte
	var certificates []*x509.Certificate
	for len(bytes.TrimSpace(remaining)) != 0 {
		block, rest := pem.Decode(remaining)
		if block == nil || block.Type != "CERTIFICATE" {
			return nil, nil, ErrInvalidTLSCertificateMaterial
		}
		certificate, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, nil, ErrInvalidTLSCertificateMaterial
		}
		ders = append(ders, append([]byte(nil), block.Bytes...))
		certificates = append(certificates, certificate)
		remaining = rest
	}
	return ders, certificates, nil
}

func validTLSServerLeaf(certificate *x509.Certificate) bool {
	if certificate == nil || certificate.IsCA || len(certificate.DNSNames)+len(certificate.IPAddresses) == 0 {
		return false
	}
	serverAuth := false
	for _, usage := range certificate.ExtKeyUsage {
		serverAuth = serverAuth || usage == x509.ExtKeyUsageServerAuth
	}
	now := time.Now().UTC()
	return serverAuth && now.After(certificate.NotBefore) && now.Before(certificate.NotAfter)
}

func validTLSCertificateChain(leaf *x509.Certificate, chain []*x509.Certificate) bool {
	if leaf == nil || !isRSACertificate(leaf) || len(chain) == 0 {
		return false
	}
	for i, certificate := range chain {
		if certificate == nil || !isRSACertificate(certificate) || !certificate.IsCA || certificate.KeyUsage&x509.KeyUsageCertSign == 0 {
			return false
		}
		for _, previous := range chain[:i] {
			if bytes.Equal(certificate.Raw, previous.Raw) {
				return false
			}
		}
		child := leaf
		if i > 0 {
			child = chain[i-1]
		}
		if !bytes.Equal(child.RawIssuer, certificate.RawSubject) || child.CheckSignatureFrom(certificate) != nil {
			return false
		}
	}
	root := chain[len(chain)-1]
	if !bytes.Equal(root.RawIssuer, root.RawSubject) || root.CheckSignatureFrom(root) != nil {
		return false
	}
	roots := x509.NewCertPool()
	roots.AddCert(root)
	intermediates := x509.NewCertPool()
	for _, certificate := range chain[:len(chain)-1] {
		intermediates.AddCert(certificate)
	}
	_, err := leaf.Verify(x509.VerifyOptions{
		Roots:         roots,
		Intermediates: intermediates,
		CurrentTime:   time.Now().UTC(),
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	})
	return err == nil
}

func isRSACertificate(certificate *x509.Certificate) bool {
	_, ok := certificate.PublicKey.(*rsa.PublicKey)
	return ok
}

func cloneDERChain(chain [][]byte) [][]byte {
	cloned := make([][]byte, len(chain))
	for i, der := range chain {
		cloned[i] = append([]byte(nil), der...)
	}
	return cloned
}
