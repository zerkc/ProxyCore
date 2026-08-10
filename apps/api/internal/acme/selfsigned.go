package acme

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"regexp"
	"strings"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
)

// Material is issued/uploaded certificate material.
type Material struct {
	CertificatePEM string
	PrivateKeyPEM  string
	ExpiresAt      time.Time
}

// IssueSelfSigned creates a self-signed certificate covering the hostnames.
func IssueSelfSigned(hostnames []string, validityDays int) (Material, error) {
	names, err := normalizeHostnames(hostnames)
	if err != nil {
		return Material{}, err
	}
	if validityDays <= 0 {
		validityDays = 365
	}
	now := time.Now()
	expiresAt := now.Add(time.Duration(validityDays) * 24 * time.Hour)

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return Material{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return Material{}, err
	}
	template := x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: names[0]},
		NotBefore:             now,
		NotAfter:              expiresAt,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  false,
	}
	for _, name := range names {
		if ip := net.ParseIP(name); ip != nil {
			template.IPAddresses = append(template.IPAddresses, ip)
		} else {
			template.DNSNames = append(template.DNSNames, name)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return Material{}, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8(key)})
	return Material{
		CertificatePEM: string(certPEM),
		PrivateKeyPEM:  string(keyPEM),
		ExpiresAt:      expiresAt,
	}, nil
}

func pkcs8(key *rsa.PrivateKey) []byte {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return x509.MarshalPKCS1PrivateKey(key)
	}
	return der
}

var certificatePEMPattern = regexp.MustCompile(`(?s)-----BEGIN CERTIFICATE-----.+?-----END CERTIFICATE-----`)

// ValidateUploadedCertificate mirrors the Node uploaded-certificate validator.
func ValidateUploadedCertificate(hostnames []string, certificatePEM, privateKeyPEM string) (Material, error) {
	names, err := normalizeHostnames(hostnames)
	if err != nil {
		return Material{}, err
	}
	if !strings.Contains(certificatePEM, "BEGIN CERTIFICATE") {
		return Material{}, errors.New("Certificate PEM is required")
	}
	if !regexp.MustCompile(`BEGIN [A-Z ]*PRIVATE KEY`).MatchString(privateKeyPEM) {
		return Material{}, errors.New("Private key PEM is required")
	}

	leafMatch := certificatePEMPattern.FindString(certificatePEM)
	if leafMatch == "" {
		return Material{}, errors.New("Certificate PEM is invalid")
	}
	block, _ := pem.Decode([]byte(leafMatch))
	if block == nil {
		return Material{}, errors.New("Certificate PEM is invalid")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return Material{}, errors.New("Certificate PEM is invalid")
	}
	expiresAt := certificate.NotAfter
	if expiresAt.IsZero() {
		return Material{}, errors.New("Certificate expiry could not be read")
	}
	if !expiresAt.After(time.Now()) {
		return Material{}, errors.New("Certificate is already expired")
	}

	certificateNames := []string{}
	certificateNames = append(certificateNames, certificate.DNSNames...)
	for _, ip := range certificate.IPAddresses {
		certificateNames = append(certificateNames, ip.String())
	}
	if len(certificateNames) == 0 || !certificateCoversHostnames(certificateNames, names) {
		return Material{}, errors.New("Certificate SANs do not cover every requested hostname")
	}

	privateKey, err := parsePrivateKey(privateKeyPEM)
	if err != nil {
		return Material{}, errors.New("Private key PEM is required")
	}
	if !publicKeysMatch(certificate.PublicKey, privateKey) {
		return Material{}, errors.New("Certificate and private key do not match")
	}

	return Material{
		CertificatePEM: strings.TrimSpace(certificatePEM) + "\n",
		PrivateKeyPEM:  strings.TrimSpace(privateKeyPEM) + "\n",
		ExpiresAt:      expiresAt,
	}, nil
}

// CertificateCoversHostnames reports whether the certificate SANs cover routes.
func CertificateCoversHostnames(certificateHostnames, routeHostnames []string) bool {
	return certificateCoversHostnames(certificateHostnames, routeHostnames)
}

func certificateCoversHostnames(certificateHostnames, routeHostnames []string) bool {
	certificates, err := normalizeHostnames(certificateHostnames)
	if err != nil {
		return false
	}
	routes, err := normalizeHostnames(routeHostnames)
	if err != nil {
		return false
	}
	for _, hostname := range routes {
		matched := false
		for _, candidate := range certificates {
			if strings.HasPrefix(candidate, "*.") {
				if strings.HasSuffix(hostname, candidate[1:]) &&
					len(strings.Split(hostname, ".")) == len(strings.Split(candidate, ".")) {
					matched = true
					break
				}
			} else if candidate == hostname {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

func normalizeHostnames(hostnames []string) ([]string, error) {
	if len(hostnames) == 0 {
		return nil, errors.New("At least one certificate hostname is required")
	}
	seen := map[string]bool{}
	out := []string{}
	for _, hostname := range hostnames {
		normalized := strings.Trim(strings.TrimSpace(hostname), "[]")
		if net.ParseIP(normalized) != nil {
			normalized = strings.ToLower(normalized)
		} else {
			dnsName, err := domain.NormalizeDNSName(normalized, true)
			if err != nil {
				return nil, err
			}
			normalized = dnsName
		}
		if !seen[normalized] {
			seen[normalized] = true
			out = append(out, normalized)
		}
	}
	return out, nil
}
