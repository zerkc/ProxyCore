package acme

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"strings"
	"time"
)

const (
	defaultCAValidityDays   = 3650 // 10 years
	defaultLeafValidityDays = 365
	internalCACN            = "ProxyCore Internal CA"
)

// DERHashSHA256 returns the lowercase SHA-256 fingerprint of the exact DER bytes.
func DERHashSHA256(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

// CertificateDERHashSHA256 fingerprints one syntactically valid certificate PEM value.
func CertificateDERHashSHA256(certificatePEM string) (string, error) {
	block, err := decodeExactPEM(certificatePEM, "CERTIFICATE")
	if err != nil {
		return "", err
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", errors.New("certificate DER is invalid")
	}
	return DERHashSHA256(certificate.Raw), nil
}

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
	return IssueSignedByCAWithKey(hostnames, validityDays, caCertPEM, caKeyPEM, "")
}

func IssueSignedByCAWithKey(hostnames []string, validityDays int, caCertPEM, caKeyPEM, leafKeyPEM string) (Material, error) {
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

	now := time.Now().UTC()
	expiresAt := now.Add(time.Duration(validityDays) * 24 * time.Hour)
	if expiresAt.After(caCert.NotAfter) {
		expiresAt = caCert.NotAfter
	}

	var key crypto.Signer
	var keyPEM string
	if leafKeyPEM == "" {
		generated, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			return Material{}, err
		}
		key = generated
		keyPEM = string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8(generated)}))
	} else {
		key, err = parsePrivateKey(leafKeyPEM)
		if err != nil {
			return Material{}, errors.New("enrollment leaf private key PEM is invalid")
		}
		keyPEM = strings.TrimSpace(leafKeyPEM) + "\n"
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
	der, err := x509.CreateCertificate(rand.Reader, &template, caCert, key.Public(), caKey)
	if err != nil {
		return Material{}, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return Material{
		CertificatePEM: string(certPEM),
		PrivateKeyPEM:  keyPEM,
		ExpiresAt:      expiresAt,
	}, nil
}

func parseCAMaterial(caCertPEM, caKeyPEM string) (*x509.Certificate, crypto.Signer, error) {
	block, err := decodeExactPEM(caCertPEM, "CERTIFICATE")
	if err != nil {
		return nil, nil, errors.New("internal CA certificate PEM is invalid")
	}
	caCert, err := x509.ParseCertificate(block.Bytes)
	if err != nil || !caCert.IsCA || caCert.KeyUsage&x509.KeyUsageCertSign == 0 {
		return nil, nil, errors.New("internal CA certificate PEM is invalid")
	}
	now := time.Now()
	if now.Before(caCert.NotBefore) || !caCert.NotAfter.After(now) {
		return nil, nil, errors.New("internal CA certificate is expired or not yet valid")
	}
	caKey, err := parseExactCAMaterialKey(caKeyPEM)
	if err != nil {
		return nil, nil, errors.New("internal CA private key PEM is invalid")
	}
	if !publicKeysMatch(caCert.PublicKey, caKey) {
		return nil, nil, errors.New("internal CA certificate and private key do not match")
	}
	if err := caCert.CheckSignatureFrom(caCert); err != nil {
		return nil, nil, errors.New("internal CA certificate signature is invalid")
	}
	return caCert, caKey, nil
}

func decodeExactPEM(value, expectedType string) (*pem.Block, error) {
	data := []byte(value)
	prefix := []byte("-----BEGIN " + expectedType + "-----\n")
	if !bytes.HasPrefix(data, prefix) {
		return nil, errors.New("PEM has unexpected leading bytes or block type")
	}
	block, rest := pem.Decode(data)
	if block == nil || block.Type != expectedType || len(block.Headers) != 0 || len(rest) != 0 {
		return nil, errors.New("PEM must contain exactly one block")
	}
	return block, nil
}

// ErrUnsupportedRSAPrivateKey reports a correctly encoded private key that is not RSA.
var ErrUnsupportedRSAPrivateKey = errors.New("private key is not RSA")

// ParseRSAPrivateKeyPEM parses the exact supported RSA private-key PEM forms.
func ParseRSAPrivateKeyPEM(value string) (*rsa.PrivateKey, error) {
	var block *pem.Block
	var err error
	for _, blockType := range []string{"PRIVATE KEY", "RSA PRIVATE KEY"} {
		if bytes.HasPrefix([]byte(value), []byte("-----BEGIN "+blockType+"-----\n")) {
			block, err = decodeExactPEM(value, blockType)
			break
		}
	}
	if err != nil || block == nil {
		return nil, errors.New("RSA private key PEM is invalid")
	}
	switch block.Type {
	case "PRIVATE KEY":
		parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, errors.New("RSA private key PEM is invalid")
		}
		key, ok := parsed.(*rsa.PrivateKey)
		if !ok {
			return nil, ErrUnsupportedRSAPrivateKey
		}
		return key, nil
	case "RSA PRIVATE KEY":
		key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return nil, errors.New("RSA private key PEM is invalid")
		}
		return key, nil
	default:
		return nil, errors.New("RSA private key PEM is invalid")
	}
}

func parseExactCAMaterialKey(keyPEM string) (crypto.Signer, error) {
	return ParseRSAPrivateKeyPEM(keyPEM)
}

func ValidateInternalCAMaterial(caCertPEM, caKeyPEM string) error {
	_, _, err := parseCAMaterial(caCertPEM, caKeyPEM)
	return err
}
