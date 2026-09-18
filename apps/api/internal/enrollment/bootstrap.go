package enrollment

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"
)

const (
	BootstrapProtocolVersion     uint32 = 1
	BootstrapRecipientKeyVersion uint32 = BootstrapProtocolVersion
	// The public envelope remains wire v1; the encrypted grant format is explicitly v2.
	BootstrapEnvelopeVersion  uint32 = BootstrapProtocolVersion
	BootstrapPlaintextVersion uint32 = bootstrapGrantPlaintextVersionV2

	bootstrapCredentialBytes       = 32
	bootstrapKEKBytes              = 32
	bootstrapNonceBytes            = 12
	bootstrapPublicKeyBytes        = 32
	bootstrapMaxBindingStringBytes = 256
	bootstrapCiphertextBytes       = bootstrapCiphertextMaxBytes
	bootstrapMaxWireBytes          = bootstrapMaxWireBytesV2
	bootstrapBindingDomain         = "proxycore/enrollment/bootstrap/aad/v1"
	bootstrapHKDFInfo              = "proxycore/enrollment/bootstrap/seal-to-recipient/v1"
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
type bootstrapRecipientPublicKeyWire = bootstrapRecipientPublicKeyWireV2
type bootstrapEnvelopeWire = bootstrapEnvelopeWireV2
type NodeCredential struct{ bytes []byte }
type ClusterKEK struct{ bytes []byte }
type BootstrapSecrets struct {
	NodeCredential NodeCredential `json:"-"`
	ClusterKEK     ClusterKEK     `json:"-"`
}

func NewBootstrapRecipient() (BootstrapRecipient, error) {
	return newBootstrapRecipientV2()
}
func (r *BootstrapRecipient) Destroy() {
	if r == nil {
		return
	}
	zeroBytes(r.private)
	r.private = nil
}
func (r *BootstrapRecipient) PublicKey() (BootstrapRecipientPublicKey, error) {
	return bootstrapRecipientPublicKeyV2(r)
}
func (p BootstrapRecipientPublicKey) Wire() ([]byte, error) {
	return marshalBootstrapRecipientPublicKeyV2(p)
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
	return parseBootstrapRecipientPublicKeyV2(wire)
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

func SealBootstrap(recipient BootstrapRecipientPublicKey, binding BootstrapBinding, grant BootstrapGrant) ([]byte, error) {
	return sealBootstrapWithEntropy(recipient, binding, grant, rand.Reader)
}
func sealBootstrapWithEntropy(recipient BootstrapRecipientPublicKey, binding BootstrapBinding, grant BootstrapGrant, entropy io.Reader) ([]byte, error) {
	if entropy == nil || !validRecipientPublicKeyV2(recipient) || grant.validateBinding(binding) != nil {
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
	return sealBootstrapWithEphemeral(sender, nonce, recipient, aad, grant)
}
func sealBootstrapWithEphemeral(sender *ecdh.PrivateKey, nonce []byte, recipient BootstrapRecipientPublicKey, aad []byte, grant BootstrapGrant) ([]byte, error) {
	if sender == nil || len(nonce) != bootstrapNonceBytes || len(aad) == 0 || !validRecipientPublicKeyV2(recipient) || grant.validateShape() != nil {
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
	plaintext, err := encodeBootstrapGrantPlaintext(grant)
	if err != nil {
		return nil, ErrBootstrapDenied
	}
	defer zeroBytes(plaintext)
	ciphertext := gcm.Seal(nil, nonce, plaintext, aad)
	defer zeroBytes(ciphertext)
	if len(ciphertext) < bootstrapCiphertextMinBytes || len(ciphertext) > bootstrapCiphertextMaxBytes {
		return nil, ErrBootstrapDenied
	}
	wire, err := marshalBootstrapEnvelopeV2(sender.PublicKey().Bytes(), nonce, ciphertext)
	if err != nil || len(wire) > bootstrapMaxWireBytes {
		return nil, ErrBootstrapDenied
	}
	return wire, nil
}

func (r *BootstrapRecipient) OpenBootstrap(binding BootstrapBinding, wire []byte) (BootstrapGrant, error) {
	if r == nil || len(r.private) != bootstrapPublicKeyBytes || bytes.Equal(r.private, zeroBootstrapPrivate[:]) {
		return BootstrapGrant{}, ErrBootstrapDenied
	}
	aad, err := binding.CanonicalBytes()
	if err != nil {
		return BootstrapGrant{}, ErrBootstrapDenied
	}
	envelope, ok := parseBootstrapEnvelopeV2(wire)
	if !ok {
		return BootstrapGrant{}, ErrBootstrapDenied
	}
	private, err := ecdh.X25519().NewPrivateKey(r.private)
	if err != nil {
		return BootstrapGrant{}, ErrBootstrapDenied
	}
	senderPublic, err := ecdh.X25519().NewPublicKey(envelope.senderEphemeralPublicKey)
	if err != nil {
		return BootstrapGrant{}, ErrBootstrapDenied
	}
	shared, err := private.ECDH(senderPublic)
	if err != nil {
		return BootstrapGrant{}, ErrBootstrapDenied
	}
	defer zeroBytes(shared)
	key, err := hkdf.Key(sha256.New, shared, aad, bootstrapHKDFInfo, 32)
	if err != nil {
		return BootstrapGrant{}, ErrBootstrapDenied
	}
	defer zeroBytes(key)
	block, err := aes.NewCipher(key)
	if err != nil {
		return BootstrapGrant{}, ErrBootstrapDenied
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil || gcm.NonceSize() != bootstrapNonceBytes {
		return BootstrapGrant{}, ErrBootstrapDenied
	}
	plaintext, err := gcm.Open(nil, envelope.nonce, envelope.ciphertext, aad)
	if err != nil {
		return BootstrapGrant{}, ErrBootstrapDenied
	}
	// Keep cleanup registered before any authenticated-plaintext length or parse branch.
	defer zeroBytes(plaintext)
	if len(plaintext) < bootstrapPlaintextMinBytes || len(plaintext) > bootstrapPlaintextMaxBytes {
		return BootstrapGrant{}, ErrBootstrapDenied
	}
	grant, err := decodeBootstrapGrantPlaintext(plaintext)
	if err != nil {
		return BootstrapGrant{}, ErrBootstrapDenied
	}
	if grant.validateBinding(binding) != nil {
		grant.Destroy()
		return BootstrapGrant{}, ErrBootstrapDenied
	}
	return grant, nil
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
