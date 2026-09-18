package enrollment

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"
)

const (
	BootstrapProtocolVersion       uint32 = 1
	BootstrapRecipientKeyVersion   uint32 = BootstrapProtocolVersion
	BootstrapEnvelopeVersion       uint32 = BootstrapProtocolVersion
	BootstrapPlaintextVersion      uint32 = BootstrapProtocolVersion
	bootstrapCredentialBytes              = 32
	bootstrapKEKBytes                     = 32
	bootstrapNonceBytes                   = 12
	bootstrapPublicKeyBytes               = 32
	bootstrapPlaintextBytes               = 4 + bootstrapCredentialBytes + bootstrapKEKBytes
	bootstrapCiphertextBytes              = bootstrapPlaintextBytes + 16
	bootstrapMaxBindingStringBytes        = 256
	bootstrapMaxWireBytes                 = 1024
	bootstrapBindingDomain                = "proxycore/enrollment/bootstrap/aad/v1"
	bootstrapHKDFInfo                     = "proxycore/enrollment/bootstrap/seal-to-recipient/v1"
)

var ErrBootstrapDenied = errors.New("bootstrap denied")
var zeroBootstrapPrivate [bootstrapPublicKeyBytes]byte

type BootstrapBinding struct {
	ProtocolVersion             uint32
	AttemptID                   string
	SourcePrimaryInstallationID string
	TargetInstallationID        string
	TargetNodeID                string
	LeadershipGeneration        uint64
	VerifiedPreviewDigest       []byte
}
type BootstrapRecipient struct{ private []byte }
type BootstrapRecipientPublicKey struct {
	version uint32
	key     []byte
}
type bootstrapRecipientPublicKeyWire struct {
	Version   uint32 `json:"version"`
	PublicKey string `json:"publicKey"`
}
type bootstrapEnvelopeWire struct {
	Version                  uint32 `json:"version"`
	SenderEphemeralPublicKey string `json:"senderEphemeralPublicKey"`
	Nonce                    string `json:"nonce"`
	Ciphertext               string `json:"ciphertext"`
}
type NodeCredential struct{ bytes []byte }
type ClusterKEK struct{ bytes []byte }
type BootstrapSecrets struct {
	NodeCredential NodeCredential `json:"-"`
	ClusterKEK     ClusterKEK     `json:"-"`
}

func NewBootstrapRecipient() (BootstrapRecipient, error) {
	private, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return BootstrapRecipient{}, ErrBootstrapDenied
	}
	value := private.Bytes()
	defer zeroBytes(value)
	return BootstrapRecipient{private: newSecretCopy(value)}, nil
}
func (r *BootstrapRecipient) Destroy() {
	if r == nil {
		return
	}
	zeroBytes(r.private)
	r.private = nil
}
func (r *BootstrapRecipient) PublicKey() (BootstrapRecipientPublicKey, error) {
	if r == nil || len(r.private) != bootstrapPublicKeyBytes || bytes.Equal(r.private, zeroBootstrapPrivate[:]) {
		return BootstrapRecipientPublicKey{}, ErrBootstrapDenied
	}
	private, err := ecdh.X25519().NewPrivateKey(r.private)
	if err != nil {
		return BootstrapRecipientPublicKey{}, ErrBootstrapDenied
	}
	value := private.PublicKey().Bytes()
	if !validX25519PublicKey(value) {
		return BootstrapRecipientPublicKey{}, ErrBootstrapDenied
	}
	return BootstrapRecipientPublicKey{version: BootstrapRecipientKeyVersion, key: newSecretCopy(value)}, nil
}
func (p BootstrapRecipientPublicKey) Wire() ([]byte, error) {
	if !validRecipientPublicKey(p) {
		return nil, ErrBootstrapDenied
	}
	return json.Marshal(bootstrapRecipientPublicKeyWire{
		Version: p.version, PublicKey: base64.RawURLEncoding.EncodeToString(p.key),
	})
}
func (p BootstrapRecipientPublicKey) MarshalJSON() ([]byte, error) { return p.Wire() }
func (p *BootstrapRecipientPublicKey) UnmarshalJSON(wire []byte) error {
	if p == nil {
		return ErrBootstrapDenied
	}
	parsed, err := ParseBootstrapRecipientPublicKey(wire)
	if err == nil {
		*p = parsed
	}
	return err
}
func ParseBootstrapRecipientPublicKey(wire []byte) (BootstrapRecipientPublicKey, error) {
	if len(wire) == 0 || len(wire) > bootstrapMaxWireBytes {
		return BootstrapRecipientPublicKey{}, ErrBootstrapDenied
	}
	var encoded bootstrapRecipientPublicKeyWire
	if !decodeCanonicalJSON(wire, &encoded) || encoded.Version != BootstrapRecipientKeyVersion {
		return BootstrapRecipientPublicKey{}, ErrBootstrapDenied
	}
	value, ok := decodeCanonicalBase64(encoded.PublicKey, bootstrapPublicKeyBytes)
	if !ok || !validX25519PublicKey(value) {
		return BootstrapRecipientPublicKey{}, ErrBootstrapDenied
	}
	return BootstrapRecipientPublicKey{version: encoded.Version, key: newSecretCopy(value)}, nil
}
func (b BootstrapBinding) CanonicalBytes() ([]byte, error) {
	if err := validateBootstrapBinding(b); err != nil {
		return nil, err
	}
	result := make([]byte, 0, 4+64+5*(4+bootstrapMaxBindingStringBytes))
	result = appendBootstrapLengthPrefixed(result, []byte(bootstrapBindingDomain))
	var fixed [8]byte
	binary.BigEndian.PutUint32(fixed[:4], b.ProtocolVersion)
	result = append(result, fixed[:4]...)
	for _, value := range []string{b.AttemptID, b.SourcePrimaryInstallationID, b.TargetInstallationID, b.TargetNodeID} {
		result = appendBootstrapLengthPrefixed(result, []byte(value))
	}
	binary.BigEndian.PutUint64(fixed[:], b.LeadershipGeneration)
	result = append(result, fixed[:]...)
	return appendBootstrapLengthPrefixed(result, b.VerifiedPreviewDigest), nil
}
func SealBootstrap(recipient BootstrapRecipientPublicKey, binding BootstrapBinding, credential, clusterKEK []byte) ([]byte, error) {
	return sealBootstrapWithEntropy(recipient, binding, credential, clusterKEK, rand.Reader)
}
func sealBootstrapWithEntropy(recipient BootstrapRecipientPublicKey, binding BootstrapBinding, credential, clusterKEK []byte, entropy io.Reader) ([]byte, error) {
	if entropy == nil || !validRecipientPublicKey(recipient) || len(credential) != bootstrapCredentialBytes || len(clusterKEK) != bootstrapKEKBytes {
		return nil, ErrBootstrapDenied
	}
	aad, err := binding.CanonicalBytes()
	if err != nil {
		return nil, ErrBootstrapDenied
	}
	sender, err := ecdh.X25519().GenerateKey(entropy)
	if err != nil {
		return nil, ErrBootstrapDenied
	}
	nonce := make([]byte, bootstrapNonceBytes)
	if _, err := io.ReadFull(entropy, nonce); err != nil {
		return nil, ErrBootstrapDenied
	}
	defer zeroBytes(nonce)
	return sealBootstrapWithEphemeral(sender, nonce, recipient, aad, credential, clusterKEK)
}
func sealBootstrapWithFixedEphemeral(senderPrivate, nonce []byte, recipient BootstrapRecipientPublicKey, binding BootstrapBinding, credential, clusterKEK []byte) ([]byte, error) {
	if len(senderPrivate) != bootstrapPublicKeyBytes || len(nonce) != bootstrapNonceBytes {
		return nil, ErrBootstrapDenied
	}
	aad, err := binding.CanonicalBytes()
	if err != nil {
		return nil, ErrBootstrapDenied
	}
	sender, err := ecdh.X25519().NewPrivateKey(senderPrivate)
	if err != nil {
		return nil, ErrBootstrapDenied
	}
	return sealBootstrapWithEphemeral(sender, nonce, recipient, aad, credential, clusterKEK)
}
func sealBootstrapWithEphemeral(sender *ecdh.PrivateKey, nonce []byte, recipient BootstrapRecipientPublicKey, aad, credential, clusterKEK []byte) ([]byte, error) {
	if sender == nil || len(nonce) != bootstrapNonceBytes || len(aad) == 0 || !validRecipientPublicKey(recipient) || len(credential) != bootstrapCredentialBytes || len(clusterKEK) != bootstrapKEKBytes {
		return nil, ErrBootstrapDenied
	}
	recipientPublic, err := ecdh.X25519().NewPublicKey(recipient.key)
	if err != nil {
		return nil, ErrBootstrapDenied
	}
	shared, err := sender.ECDH(recipientPublic)
	if err != nil {
		return nil, ErrBootstrapDenied
	}
	defer zeroBytes(shared)
	key, err := hkdf.Key(sha256.New, shared, aad, bootstrapHKDFInfo, 32)
	if err != nil {
		return nil, ErrBootstrapDenied
	}
	defer zeroBytes(key)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, ErrBootstrapDenied
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil || gcm.NonceSize() != bootstrapNonceBytes {
		return nil, ErrBootstrapDenied
	}
	plaintext := make([]byte, bootstrapPlaintextBytes)
	defer zeroBytes(plaintext)
	binary.BigEndian.PutUint32(plaintext[:4], BootstrapPlaintextVersion)
	copy(plaintext[4:4+bootstrapCredentialBytes], credential)
	copy(plaintext[4+bootstrapCredentialBytes:], clusterKEK)
	ciphertext := gcm.Seal(nil, nonce, plaintext, aad)
	defer zeroBytes(ciphertext)
	if len(ciphertext) != bootstrapCiphertextBytes {
		return nil, ErrBootstrapDenied
	}
	wire, err := json.Marshal(bootstrapEnvelopeWire{
		Version:                  BootstrapEnvelopeVersion,
		SenderEphemeralPublicKey: base64.RawURLEncoding.EncodeToString(sender.PublicKey().Bytes()),
		Nonce:                    base64.RawURLEncoding.EncodeToString(nonce),
		Ciphertext:               base64.RawURLEncoding.EncodeToString(ciphertext),
	})
	if err != nil || len(wire) > bootstrapMaxWireBytes {
		return nil, ErrBootstrapDenied
	}
	return wire, nil
}
func (r *BootstrapRecipient) OpenBootstrap(binding BootstrapBinding, wire []byte) (BootstrapSecrets, error) {
	if r == nil || len(r.private) != bootstrapPublicKeyBytes || bytes.Equal(r.private, zeroBootstrapPrivate[:]) {
		return BootstrapSecrets{}, ErrBootstrapDenied
	}
	aad, err := binding.CanonicalBytes()
	if err != nil {
		return BootstrapSecrets{}, ErrBootstrapDenied
	}
	envelope, ok := parseBootstrapEnvelope(wire)
	if !ok {
		return BootstrapSecrets{}, ErrBootstrapDenied
	}
	private, err := ecdh.X25519().NewPrivateKey(r.private)
	if err != nil {
		return BootstrapSecrets{}, ErrBootstrapDenied
	}
	senderPublic, err := ecdh.X25519().NewPublicKey(envelope.senderEphemeralPublicKey)
	if err != nil {
		return BootstrapSecrets{}, ErrBootstrapDenied
	}
	shared, err := private.ECDH(senderPublic)
	if err != nil {
		return BootstrapSecrets{}, ErrBootstrapDenied
	}
	defer zeroBytes(shared)
	key, err := hkdf.Key(sha256.New, shared, aad, bootstrapHKDFInfo, 32)
	if err != nil {
		return BootstrapSecrets{}, ErrBootstrapDenied
	}
	defer zeroBytes(key)
	block, err := aes.NewCipher(key)
	if err != nil {
		return BootstrapSecrets{}, ErrBootstrapDenied
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil || gcm.NonceSize() != bootstrapNonceBytes {
		return BootstrapSecrets{}, ErrBootstrapDenied
	}
	plaintext, err := gcm.Open(nil, envelope.nonce, envelope.ciphertext, aad)
	if err != nil {
		return BootstrapSecrets{}, ErrBootstrapDenied
	}
	defer zeroBytes(plaintext)
	if len(plaintext) != bootstrapPlaintextBytes || binary.BigEndian.Uint32(plaintext[:4]) != BootstrapPlaintextVersion {
		return BootstrapSecrets{}, ErrBootstrapDenied
	}
	return BootstrapSecrets{
		NodeCredential: NodeCredential{bytes: newSecretCopy(plaintext[4 : 4+bootstrapCredentialBytes])},
		ClusterKEK:     ClusterKEK{bytes: newSecretCopy(plaintext[4+bootstrapCredentialBytes:])},
	}, nil
}

type parsedBootstrapEnvelope struct {
	senderEphemeralPublicKey []byte
	nonce                    []byte
	ciphertext               []byte
}

func parseBootstrapEnvelope(wire []byte) (parsedBootstrapEnvelope, bool) {
	if len(wire) == 0 || len(wire) > bootstrapMaxWireBytes {
		return parsedBootstrapEnvelope{}, false
	}
	var encoded bootstrapEnvelopeWire
	if !decodeCanonicalJSON(wire, &encoded) || encoded.Version != BootstrapEnvelopeVersion {
		return parsedBootstrapEnvelope{}, false
	}
	sender, ok := decodeCanonicalBase64(encoded.SenderEphemeralPublicKey, bootstrapPublicKeyBytes)
	if !ok || !validX25519PublicKey(sender) {
		return parsedBootstrapEnvelope{}, false
	}
	nonce, ok := decodeCanonicalBase64(encoded.Nonce, bootstrapNonceBytes)
	if !ok {
		return parsedBootstrapEnvelope{}, false
	}
	ciphertext, ok := decodeCanonicalBase64(encoded.Ciphertext, bootstrapCiphertextBytes)
	if !ok {
		return parsedBootstrapEnvelope{}, false
	}
	return parsedBootstrapEnvelope{sender, nonce, ciphertext}, true
}
func validateBootstrapBinding(binding BootstrapBinding) error {
	if binding.ProtocolVersion != BootstrapProtocolVersion || binding.LeadershipGeneration == 0 || len(binding.VerifiedPreviewDigest) != sha256.Size {
		return ErrBootstrapDenied
	}
	for _, value := range []string{binding.AttemptID, binding.SourcePrimaryInstallationID, binding.TargetInstallationID, binding.TargetNodeID} {
		if len(value) == 0 || len(value) > bootstrapMaxBindingStringBytes || !utf8.ValidString(value) {
			return ErrBootstrapDenied
		}
	}
	return nil
}
func appendBootstrapLengthPrefixed(dst, value []byte) []byte {
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(value)))
	return append(append(dst, length[:]...), value...)
}
func validRecipientPublicKey(key BootstrapRecipientPublicKey) bool {
	return key.version == BootstrapRecipientKeyVersion && validX25519PublicKey(key.key)
}
func validX25519PublicKey(key []byte) bool {
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
func decodeCanonicalJSON(wire []byte, target any) bool {
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
func decodeCanonicalBase64(value string, expectedBytes int) ([]byte, bool) {
	if expectedBytes < 0 || len(value) != base64.RawURLEncoding.EncodedLen(expectedBytes) {
		return nil, false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	return decoded, err == nil && len(decoded) == expectedBytes && base64.RawURLEncoding.EncodeToString(decoded) == value
}
func newSecretCopy(value []byte) []byte    { return append([]byte(nil), value...) }
func (c NodeCredential) CopyBytes() []byte { return newSecretCopy(c.bytes) }
func (k ClusterKEK) CopyBytes() []byte     { return newSecretCopy(k.bytes) }
func (c *NodeCredential) Destroy()         { destroySecret(&c.bytes) }
func (k *ClusterKEK) Destroy()             { destroySecret(&k.bytes) }
func (s *BootstrapSecrets) Destroy() {
	if s != nil {
		s.NodeCredential.Destroy()
		s.ClusterKEK.Destroy()
	}
}
func (r BootstrapRecipient) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, "[bootstrap recipient redacted]")
}
func (c NodeCredential) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, "[node credential redacted]")
}
func (k ClusterKEK) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, "[cluster KEK redacted]")
}
func destroySecret(value *[]byte) {
	if value != nil {
		zeroBytes(*value)
		*value = nil
	}
}
func zeroBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}
