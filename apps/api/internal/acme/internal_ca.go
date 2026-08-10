package acme

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"time"
)

const (
	defaultCAValidityDays   = 3650 // 10 years
	defaultLeafValidityDays = 365
	internalCACN            = "ProxyCore Internal CA"
)

// CreateInternalCA creates a long-lived private CA used to sign internal leaf certificates.
func CreateInternalCA(validityDays int) (Material, error) {
	if validityDays <= 0 {
		validityDays = defaultCAValidityDays
	}
	now := time.Now()
	expiresAt := now.Add(time.Duration(validityDays) * 24 * time.Hour)

	// 2048-bit keeps CA generation practical on small homelab hosts.
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
		Subject:               pkix.Name{CommonName: internalCACN, Organization: []string{"ProxyCore"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              expiresAt,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
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

// IssueSignedByCA issues a leaf certificate signed by the ProxyCore internal CA.
func IssueSignedByCA(hostnames []string, validityDays int, caCertPEM, caKeyPEM string) (Material, error) {
	names, err := normalizeHostnames(hostnames)
	if err != nil {
		return Material{}, err
	}
	if validityDays <= 0 {
		validityDays = defaultLeafValidityDays
	}
	caCert, caKey, err := parseCAMaterial(caCertPEM, caKeyPEM)
	if err != nil {
		return Material{}, err
	}

	now := time.Now()
	expiresAt := now.Add(time.Duration(validityDays) * 24 * time.Hour)
	if expiresAt.After(caCert.NotAfter) {
		expiresAt = caCert.NotAfter
	}

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
		NotBefore:             now.Add(-5 * time.Minute),
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
	der, err := x509.CreateCertificate(rand.Reader, &template, caCert, &key.PublicKey, caKey)
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

func parseCAMaterial(caCertPEM, caKeyPEM string) (*x509.Certificate, crypto.Signer, error) {
	block, _ := pem.Decode([]byte(caCertPEM))
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, nil, errors.New("internal CA certificate PEM is invalid")
	}
	caCert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, nil, errors.New("internal CA certificate PEM is invalid")
	}
	if !caCert.IsCA {
		return nil, nil, errors.New("internal CA certificate is not a CA")
	}
	caKey, err := parsePrivateKey(caKeyPEM)
	if err != nil {
		return nil, nil, errors.New("internal CA private key PEM is invalid")
	}
	if !publicKeysMatch(caCert.PublicKey, caKey) {
		return nil, nil, errors.New("internal CA certificate and private key do not match")
	}
	return caCert, caKey, nil
}
