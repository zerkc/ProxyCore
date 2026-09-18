package enrollment

import (
	"bytes"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/acme"
)

func parseCertificate(value string) (*x509.Certificate, error) {
	block, rest := pem.Decode([]byte(value))
	if block == nil || block.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, ErrInvalidIdentityProof
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, ErrInvalidIdentityProof
	}
	return certificate, nil
}

func parseRSAKey(value string) (*rsa.PrivateKey, error) {
	key, err := acme.ParseRSAPrivateKeyPEM(value)
	if errors.Is(err, acme.ErrUnsupportedRSAPrivateKey) {
		return nil, ErrUnsupportedIdentityProofKey
	}
	if err != nil {
		return nil, ErrInvalidIdentityProof
	}
	return key, nil
}

func validateCertificatePair(leaf, ca *x509.Certificate, now time.Time) error {
	if _, ok := leaf.PublicKey.(*rsa.PublicKey); !ok {
		return ErrUnsupportedIdentityProofKey
	}
	if _, ok := ca.PublicKey.(*rsa.PublicKey); !ok {
		return ErrUnsupportedIdentityProofKey
	}
	if leaf.IsCA || !ca.IsCA || ca.KeyUsage&x509.KeyUsageCertSign == 0 ||
		now.Before(leaf.NotBefore) || !now.Before(leaf.NotAfter) ||
		now.Before(ca.NotBefore) || !now.Before(ca.NotAfter) {
		return ErrIdentityProofTimeWindow
	}
	serverAuth := false
	for _, usage := range leaf.ExtKeyUsage {
		serverAuth = serverAuth || usage == x509.ExtKeyUsageServerAuth
	}
	if !serverAuth {
		return ErrInvalidIdentityProof
	}
	if err := ca.CheckSignatureFrom(ca); err != nil {
		return ErrInvalidIdentityProof
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots: roots, CurrentTime: now,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		return ErrInvalidIdentityProof
	}
	return nil
}

func certificateMatchesURL(certificate *x509.Certificate, primaryURL string) error {
	parsed, err := url.Parse(primaryURL)
	if err != nil {
		return ErrIdentityProofBinding
	}
	host, _, err := net.SplitHostPort(parsed.Host)
	if err != nil || certificate.VerifyHostname(host) != nil {
		return ErrIdentityProofBinding
	}
	return nil
}

func decodeSignature(value string) ([]byte, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(decoded) == 0 || len(decoded) > 512 ||
		base64.RawURLEncoding.EncodeToString(decoded) != value {
		return nil, ErrInvalidIdentityProof
	}
	return decoded, nil
}

func validUUIDv4(value string) bool {
	parsed, err := uuid.Parse(value)
	return err == nil && parsed.String() == value && parsed.Version() == 4 && parsed.Variant() == uuid.RFC4122
}

func validNonce(value string) bool {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	return err == nil && len(decoded) == identityProofNonceBytes && base64.RawURLEncoding.EncodeToString(decoded) == value
}

func validHash(value string) bool {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func canonicalTime(value time.Time) bool {
	return !value.IsZero() && value.Location() == time.UTC
}

func samePublicKey(first, second any) bool {
	a, err := x509.MarshalPKIXPublicKey(first)
	if err != nil {
		return false
	}
	b, err := x509.MarshalPKIXPublicKey(second)
	return err == nil && bytes.Equal(a, b)
}

func publicKeyHash(publicKey any) string {
	der, _ := x509.MarshalPKIXPublicKey(publicKey)
	return derHash(der)
}

func derHash(der []byte) string {
	return acme.DERHashSHA256(der)
}
