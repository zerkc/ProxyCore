package enrollment

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
)

const (
	bootstrapEnvelopeVersionV2 uint32 = BootstrapEnvelopeVersion
	bootstrapMaxWireBytesV2           = 4096
)

type bootstrapRecipientPublicKeyWireV2 struct {
	Version   uint32 `json:"version"`
	PublicKey string `json:"publicKey"`
}
type bootstrapEnvelopeWireV2 struct {
	Version                  uint32 `json:"version"`
	SenderEphemeralPublicKey string `json:"senderEphemeralPublicKey"`
	Nonce                    string `json:"nonce"`
	Ciphertext               string `json:"ciphertext"`
}
type parsedBootstrapEnvelopeV2 struct {
	senderEphemeralPublicKey []byte
	nonce                    []byte
	ciphertext               []byte
}

func newBootstrapRecipientV2() (BootstrapRecipient, error) {
	private, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return BootstrapRecipient{}, ErrBootstrapDenied
	}
	value := private.Bytes()
	defer zeroBytes(value)
	return BootstrapRecipient{private: newSecretCopy(value)}, nil
}

func bootstrapRecipientPublicKeyV2(r *BootstrapRecipient) (BootstrapRecipientPublicKey, error) {
	if r == nil || len(r.private) != bootstrapPublicKeyBytes || bytes.Equal(r.private, zeroBootstrapPrivate[:]) {
		return BootstrapRecipientPublicKey{}, ErrBootstrapDenied
	}
	private, err := ecdh.X25519().NewPrivateKey(r.private)
	if err != nil {
		return BootstrapRecipientPublicKey{}, ErrBootstrapDenied
	}
	value := private.PublicKey().Bytes()
	if !validX25519PublicKeyV2(value) {
		return BootstrapRecipientPublicKey{}, ErrBootstrapDenied
	}
	return BootstrapRecipientPublicKey{version: BootstrapRecipientKeyVersion, key: newSecretCopy(value)}, nil
}

func marshalBootstrapRecipientPublicKeyV2(p BootstrapRecipientPublicKey) ([]byte, error) {
	if !validRecipientPublicKeyV2(p) {
		return nil, ErrBootstrapDenied
	}
	return json.Marshal(bootstrapRecipientPublicKeyWireV2{
		Version: p.version, PublicKey: base64.RawURLEncoding.EncodeToString(p.key),
	})
}

func parseBootstrapRecipientPublicKeyV2(wire []byte) (BootstrapRecipientPublicKey, error) {
	if len(wire) == 0 || len(wire) > bootstrapMaxWireBytesV2 {
		return BootstrapRecipientPublicKey{}, ErrBootstrapDenied
	}
	var encoded bootstrapRecipientPublicKeyWireV2
	if !decodeCanonicalJSONV2(wire, &encoded) || encoded.Version != BootstrapRecipientKeyVersion {
		return BootstrapRecipientPublicKey{}, ErrBootstrapDenied
	}
	value, ok := decodeCanonicalBase64V2(encoded.PublicKey, bootstrapPublicKeyBytes)
	if !ok || !validX25519PublicKeyV2(value) {
		return BootstrapRecipientPublicKey{}, ErrBootstrapDenied
	}
	return BootstrapRecipientPublicKey{version: encoded.Version, key: newSecretCopy(value)}, nil
}

func marshalBootstrapEnvelopeV2(sender, nonce, ciphertext []byte) ([]byte, error) {
	if len(sender) != bootstrapPublicKeyBytes || len(nonce) != bootstrapNonceBytes || len(ciphertext) < bootstrapCiphertextMinBytes || len(ciphertext) > bootstrapCiphertextMaxBytes {
		return nil, ErrBootstrapDenied
	}
	return json.Marshal(bootstrapEnvelopeWireV2{
		Version:                  bootstrapEnvelopeVersionV2,
		SenderEphemeralPublicKey: base64.RawURLEncoding.EncodeToString(sender),
		Nonce:                    base64.RawURLEncoding.EncodeToString(nonce),
		Ciphertext:               base64.RawURLEncoding.EncodeToString(ciphertext),
	})
}

func parseBootstrapEnvelopeV2(wire []byte) (parsedBootstrapEnvelopeV2, bool) {
	if len(wire) == 0 || len(wire) > bootstrapMaxWireBytesV2 {
		return parsedBootstrapEnvelopeV2{}, false
	}
	var encoded bootstrapEnvelopeWireV2
	if !decodeCanonicalJSONV2(wire, &encoded) || encoded.Version != bootstrapEnvelopeVersionV2 {
		return parsedBootstrapEnvelopeV2{}, false
	}
	sender, ok := decodeCanonicalBase64V2(encoded.SenderEphemeralPublicKey, bootstrapPublicKeyBytes)
	if !ok || !validX25519PublicKeyV2(sender) {
		return parsedBootstrapEnvelopeV2{}, false
	}
	nonce, ok := decodeCanonicalBase64V2(encoded.Nonce, bootstrapNonceBytes)
	if !ok {
		return parsedBootstrapEnvelopeV2{}, false
	}
	ciphertext, ok := decodeCanonicalBase64RangeV2(encoded.Ciphertext, bootstrapCiphertextMinBytes, bootstrapCiphertextMaxBytes)
	if !ok {
		return parsedBootstrapEnvelopeV2{}, false
	}
	return parsedBootstrapEnvelopeV2{sender, nonce, ciphertext}, true
}

func validRecipientPublicKeyV2(key BootstrapRecipientPublicKey) bool {
	return key.version == BootstrapRecipientKeyVersion && validX25519PublicKeyV2(key.key)
}

func validX25519PublicKeyV2(key []byte) bool {
	if len(key) != bootstrapPublicKeyBytes {
		return false
	}
	public, err := ecdh.X25519().NewPublicKey(key)
	if err != nil {
		return false
	}
	var check [bootstrapPublicKeyBytes]byte
	check[0] = 1
	private, err := ecdh.X25519().NewPrivateKey(check[:])
	if err != nil {
		return false
	}
	_, err = private.ECDH(public)
	return err == nil
}

func decodeCanonicalJSONV2(wire []byte, target any) bool {
	decoder := json.NewDecoder(bytes.NewReader(wire))
	decoder.DisallowUnknownFields()
	if decoder.Decode(target) != nil {
		return false
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return false
	}
	canonical, err := json.Marshal(target)
	return err == nil && bytes.Equal(wire, canonical)
}

func decodeCanonicalBase64V2(value string, expectedBytes int) ([]byte, bool) {
	return decodeCanonicalBase64RangeV2(value, expectedBytes, expectedBytes)
}

func decodeCanonicalBase64RangeV2(value string, minBytes, maxBytes int) ([]byte, bool) {
	if minBytes < 0 || maxBytes < minBytes || len(value) == 0 {
		return nil, false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	return decoded, err == nil && len(decoded) >= minBytes && len(decoded) <= maxBytes && base64.RawURLEncoding.EncodeToString(decoded) == value
}
