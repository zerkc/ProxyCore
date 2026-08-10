package acme

import (
	"bytes"
	"crypto"
	"crypto/x509"
	"encoding/pem"
	"errors"
)

func parsePrivateKey(privateKeyPEM string) (crypto.Signer, error) {
	block, _ := pem.Decode([]byte(privateKeyPEM))
	if block == nil {
		return nil, errors.New("invalid private key PEM")
	}
	if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if signer, ok := key.(crypto.Signer); ok {
			return signer, nil
		}
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	if key, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	return nil, errors.New("unsupported private key format")
}

func publicKeysMatch(certPublicKey any, privateKey crypto.Signer) bool {
	certDER, err := x509.MarshalPKIXPublicKey(certPublicKey)
	if err != nil {
		return false
	}
	keyDER, err := x509.MarshalPKIXPublicKey(privateKey.Public())
	if err != nil {
		return false
	}
	return bytes.Equal(certDER, keyDER)
}
