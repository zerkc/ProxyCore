package configuration

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net"
	"strings"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/acme"
)

const internalCAID = "default"

const enrollmentStateID = "default"

const EnrollmentLeafKeyPurpose = "internal-ca-enrollment-leaf-private-key"

const enrollmentLeafValidityDays = 365

const enrollmentLeafRenewalWindow = 30 * 24 * time.Hour

type EnrollmentTLSMaterial struct {
	CertificatePEM   string    `json:"certificatePem"`
	PrivateKeyPEM    string    `json:"-"`
	CACertificatePEM string    `json:"caCertificatePem"`
	CADERHashSHA256  string    `json:"caDerSha256"`
	ExpiresAt        time.Time `json:"expiresAt"`
}

// EnrollmentTLSPublicMaterial is the redacted trust projection safe for Owner-facing serialization.
type EnrollmentTLSPublicMaterial struct {
	CertificatePEM   string    `json:"certificatePem"`
	CACertificatePEM string    `json:"caCertificatePem"`
	CADERHashSHA256  string    `json:"caDerSha256"`
	ExpiresAt        time.Time `json:"expiresAt"`
}

// EnrollmentTLSTrust describes the read-only, established enrollment trust state.
type EnrollmentTLSTrust struct {
	Configured bool
	Ready      bool
	Material   EnrollmentTLSPublicMaterial
}

// Public returns enrollment certificate and CA trust material without private or secret data.
func (m EnrollmentTLSMaterial) Public() EnrollmentTLSPublicMaterial {
	return EnrollmentTLSPublicMaterial{
		CertificatePEM:   m.CertificatePEM,
		CACertificatePEM: m.CACertificatePEM,
		CADERHashSHA256:  m.CADERHashSHA256,
		ExpiresAt:        m.ExpiresAt,
	}
}

type validatedEnrollmentLeaf struct {
	certificate *x509.Certificate
	ca          *x509.Certificate
}

func newEnrollmentTLSMaterial(_ string, privateKeyPEM, caPEM string, validated validatedEnrollmentLeaf) EnrollmentTLSMaterial {
	return EnrollmentTLSMaterial{
		CertificatePEM:   canonicalCertificatePEM(validated.certificate),
		PrivateKeyPEM:    privateKeyPEM,
		CACertificatePEM: caPEM,
		CADERHashSHA256:  acme.DERHashSHA256(validated.ca.Raw),
		ExpiresAt:        validated.certificate.NotAfter,
	}
}

func validateEnrollmentLeaf(certificatePEM, keyPEM, caCertPEM, caKeyPEM string) (validatedEnrollmentLeaf, error) {
	certificate, err := parseSingleCertificatePEM(certificatePEM)
	if err != nil {
		return validatedEnrollmentLeaf{}, errors.New("enrollment leaf certificate PEM is invalid")
	}
	pair, err := tls.X509KeyPair([]byte(certificatePEM), []byte(keyPEM))
	if err != nil || len(pair.Certificate) != 1 || !bytes.Equal(pair.Certificate[0], certificate.Raw) {
		return validatedEnrollmentLeaf{}, errors.New("enrollment leaf certificate and private key are invalid or do not match")
	}
	if certificate.IsCA {
		return validatedEnrollmentLeaf{}, errors.New("enrollment leaf certificate is invalid")
	}
	serverAuth := false
	for _, usage := range certificate.ExtKeyUsage {
		serverAuth = serverAuth || usage == x509.ExtKeyUsageServerAuth
	}
	for _, name := range certificate.DNSNames {
		if net.ParseIP(name) != nil {
			return validatedEnrollmentLeaf{}, errors.New("enrollment leaf certificate encodes an IP as a DNS SAN")
		}
	}
	if !serverAuth || len(certificate.DNSNames)+len(certificate.IPAddresses) == 0 {
		return validatedEnrollmentLeaf{}, errors.New("enrollment leaf certificate lacks ServerAuth or SANs")
	}
	caPair, err := tls.X509KeyPair([]byte(caCertPEM), []byte(caKeyPEM))
	if err != nil || len(caPair.Certificate) == 0 {
		return validatedEnrollmentLeaf{}, errors.New("internal CA certificate and key are invalid")
	}
	caCert, err := x509.ParseCertificate(caPair.Certificate[0])
	if err != nil {
		return validatedEnrollmentLeaf{}, errors.New("internal CA certificate is invalid")
	}
	roots := x509.NewCertPool()
	roots.AddCert(caCert)
	if _, err := certificate.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		return validatedEnrollmentLeaf{}, errors.New("enrollment leaf certificate does not chain to the internal CA")
	}
	return validatedEnrollmentLeaf{certificate: certificate, ca: caCert}, nil
}

func parseSingleCertificatePEM(certificatePEM string) (*x509.Certificate, error) {
	data := []byte(certificatePEM)
	if !bytes.HasPrefix(data, []byte("-----BEGIN CERTIFICATE-----")) {
		return nil, errors.New("certificate PEM must start with a CERTIFICATE block")
	}
	block, rest := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("certificate PEM must contain exactly one CERTIFICATE block")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, err
	}
	return certificate, nil
}

func canonicalCertificatePEM(certificate *x509.Certificate) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw}))
}

func sameEnrollmentHostnames(certificate *x509.Certificate, configured []string) bool {
	names := append([]string{}, certificate.DNSNames...)
	for _, name := range certificate.DNSNames {
		if net.ParseIP(name) != nil {
			return false
		}
	}
	for _, ip := range certificate.IPAddresses {
		names = append(names, ip.String())
	}
	normalized, err := NormalizeEnrollmentHostnames(names)
	return err == nil && acme.CertificateCoversHostnames(names, configured) && strings.Join(normalized, "\x00") == strings.Join(configured, "\x00")
}
