package enrollment

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// These constants and framing helpers deliberately reconstruct the documented wire
// and plaintext format without calling production seal/codec helpers.
const (
	bootstrapVectorProtocolVersion   uint32 = 1
	bootstrapVectorEnvelopeVersion   uint32 = 1
	bootstrapVectorPlaintextVersion  uint32 = 2
	bootstrapVectorCredentialBytes          = 32
	bootstrapVectorKEKBytes                 = 32
	bootstrapVectorNonceBytes               = 12
	bootstrapVectorPublicKeyBytes           = 32
	bootstrapVectorPlaintextMinBytes        = 4 + 4 + 4 + 32 + 4 + 32 + 4*4 + 8 + 8
	bootstrapVectorBindingDomain            = "proxycore/enrollment/bootstrap/aad/v1"
	bootstrapVectorHKDFInfo                 = "proxycore/enrollment/bootstrap/seal-to-recipient/v1"
)

type bootstrapVectorEnvelopeWire struct {
	Version                  uint32 `json:"version"`
	SenderEphemeralPublicKey string `json:"senderEphemeralPublicKey"`
	Nonce                    string `json:"nonce"`
	Ciphertext               string `json:"ciphertext"`
}

func TestBootstrapDeterministicVector(t *testing.T) {
	var recipientPrivate [bootstrapVectorPublicKeyBytes]byte
	for index := range recipientPrivate {
		recipientPrivate[index] = byte(0x20 + index)
	}
	binding := bootstrapVectorBinding()
	credential := bytes.Repeat([]byte{0xa1}, bootstrapVectorCredentialBytes)
	kek := bytes.Repeat([]byte{0xb2}, bootstrapVectorKEKBytes)
	var senderPrivate [bootstrapVectorPublicKeyBytes]byte
	for index := range senderPrivate {
		senderPrivate[index] = byte(index)
	}
	nonce := make([]byte, bootstrapVectorNonceBytes)
	for index := range nonce {
		nonce[index] = byte(0xa0 + index)
	}
	aad := bootstrapVectorBindingAAD(binding)
	plaintext := bootstrapVectorGrantPlaintext(binding, credential, kek)
	wire := bootstrapVectorSeal(t, recipientPrivate[:], senderPrivate[:], nonce, aad, plaintext)
	wireDigest := sha256.Sum256(wire)
	const wantWireDigest = "77dc5a8dc7bb1b7a3446093236f5409f2294e9787bf9e80c03d4045aa2751b1a"
	if hex.EncodeToString(wireDigest[:]) != wantWireDigest {
		t.Fatal("deterministic bootstrap vector changed")
	}
	again := bootstrapVectorSeal(t, recipientPrivate[:], senderPrivate[:], nonce, aad, plaintext)
	if !bytes.Equal(again, wire) {
		t.Fatal("deterministic entropy did not reproduce the vector")
	}

	recipient := BootstrapRecipient{private: append([]byte(nil), recipientPrivate[:]...)}
	defer recipient.Destroy()
	opened, err := recipient.OpenBootstrap(binding, wire)
	if err != nil {
		t.Fatalf("open deterministic vector: %v", err)
	}
	defer opened.Destroy()
	if !bytes.Equal(opened.Secrets.NodeCredential.CopyBytes(), credential) || !bytes.Equal(opened.Secrets.ClusterKEK.CopyBytes(), kek) {
		t.Fatal("deterministic vector secrets did not open")
	}
}

func TestBootstrapLegacyPNE3AV1PlaintextIsDenied(t *testing.T) {
	var recipientPrivate [bootstrapVectorPublicKeyBytes]byte
	for index := range recipientPrivate {
		recipientPrivate[index] = byte(0x20 + index)
	}
	var senderPrivate [bootstrapVectorPublicKeyBytes]byte
	for index := range senderPrivate {
		senderPrivate[index] = byte(index)
	}
	nonce := bytes.Repeat([]byte{0x77}, bootstrapVectorNonceBytes)
	binding := bootstrapVectorBinding()
	aad := bootstrapVectorBindingAAD(binding)
	credential := bytes.Repeat([]byte{0xa1}, bootstrapVectorCredentialBytes)
	kek := bytes.Repeat([]byte{0xb2}, bootstrapVectorKEKBytes)
	legacy := make([]byte, 4+bootstrapVectorCredentialBytes+bootstrapVectorKEKBytes)
	binary.BigEndian.PutUint32(legacy[:4], bootstrapVectorProtocolVersion)
	copy(legacy[4:4+bootstrapVectorCredentialBytes], credential)
	copy(legacy[4+bootstrapVectorCredentialBytes:], kek)
	paddedLegacy := append(append([]byte(nil), legacy...), make([]byte, bootstrapVectorPlaintextMinBytes-len(legacy))...)
	for name, plaintext := range map[string][]byte{
		"exact-v1":  legacy,
		"padded-v1": paddedLegacy,
	} {
		t.Run(name, func(t *testing.T) {
			wire := bootstrapVectorSeal(t, recipientPrivate[:], senderPrivate[:], nonce, aad, plaintext)
			recipient := BootstrapRecipient{private: append([]byte(nil), recipientPrivate[:]...)}
			defer recipient.Destroy()
			opened, err := recipient.OpenBootstrap(binding, wire)
			requireBootstrapDenied(t, err)
			opened.Destroy()
		})
	}
}

func bootstrapVectorBinding() BootstrapBinding {
	return BootstrapBinding{
		ProtocolVersion:             bootstrapVectorProtocolVersion,
		AttemptID:                   "attempt-vector-01",
		SourcePrimaryInstallationID: "primary-vector-01",
		TargetInstallationID:        "target-vector-01",
		TargetNodeID:                "node-vector-01",
		LeadershipGeneration:        9,
		VerifiedPreviewDigest:       bytes.Repeat([]byte{0x5a}, 32),
	}
}

func bootstrapVectorBindingAAD(binding BootstrapBinding) []byte {
	result := bootstrapVectorLengthPrefix(nil, []byte(bootstrapVectorBindingDomain))
	var fixed [8]byte
	binary.BigEndian.PutUint32(fixed[:4], binding.ProtocolVersion)
	result = append(result, fixed[:4]...)
	for _, value := range []string{binding.AttemptID, binding.SourcePrimaryInstallationID, binding.TargetInstallationID, binding.TargetNodeID} {
		result = bootstrapVectorLengthPrefix(result, []byte(value))
	}
	binary.BigEndian.PutUint64(fixed[:], binding.LeadershipGeneration)
	result = append(result, fixed[:]...)
	return bootstrapVectorLengthPrefix(result, binding.VerifiedPreviewDigest)
}

func bootstrapVectorGrantPlaintext(binding BootstrapBinding, credential, kek []byte) []byte {
	result := make([]byte, 0, bootstrapVectorPlaintextMinBytes+128)
	var fixed [8]byte
	binary.BigEndian.PutUint32(fixed[:4], bootstrapVectorPlaintextVersion)
	result = append(result, fixed[:4]...)
	binary.BigEndian.PutUint32(fixed[:4], bootstrapVectorProtocolVersion)
	result = append(result, fixed[:4]...)
	for _, value := range []string{
		"credential-vector-01",
	} {
		result = bootstrapVectorLengthPrefix(result, []byte(value))
	}
	result = append(result, credential...)
	result = bootstrapVectorLengthPrefix(result, []byte("cluster-key-vector-01"))
	result = append(result, kek...)
	for _, value := range []string{
		binding.SourcePrimaryInstallationID,
		binding.TargetInstallationID,
		binding.TargetNodeID,
		binding.AttemptID,
	} {
		result = bootstrapVectorLengthPrefix(result, []byte(value))
	}
	binary.BigEndian.PutUint64(fixed[:], binding.LeadershipGeneration)
	result = append(result, fixed[:]...)
	binary.BigEndian.PutUint64(fixed[:], 4102444800)
	return append(result, fixed[:]...)
}

func bootstrapVectorLengthPrefix(dst, value []byte) []byte {
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(value)))
	return append(append(dst, length[:]...), value...)
}

func bootstrapVectorSeal(t *testing.T, recipientPrivate, senderPrivate, nonce, aad, plaintext []byte) []byte {
	t.Helper()
	recipient, err := ecdh.X25519().NewPrivateKey(recipientPrivate)
	if err != nil {
		t.Fatalf("recipient key: %v", err)
	}
	sender, err := ecdh.X25519().NewPrivateKey(senderPrivate)
	if err != nil {
		t.Fatalf("sender key: %v", err)
	}
	shared, err := sender.ECDH(recipient.PublicKey())
	if err != nil {
		t.Fatalf("shared key: %v", err)
	}
	defer zeroBootstrapVectorBytes(shared)
	key, err := hkdf.Key(sha256.New, shared, aad, bootstrapVectorHKDFInfo, 32)
	if err != nil {
		t.Fatalf("HKDF: %v", err)
	}
	defer zeroBootstrapVectorBytes(key)
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatalf("AES: %v", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("GCM: %v", err)
	}
	ciphertext := gcm.Seal(nil, nonce, plaintext, aad)
	defer zeroBootstrapVectorBytes(ciphertext)
	wire, err := json.Marshal(bootstrapVectorEnvelopeWire{
		Version:                  bootstrapVectorEnvelopeVersion,
		SenderEphemeralPublicKey: base64.RawURLEncoding.EncodeToString(sender.PublicKey().Bytes()),
		Nonce:                    base64.RawURLEncoding.EncodeToString(nonce),
		Ciphertext:               base64.RawURLEncoding.EncodeToString(ciphertext),
	})
	if err != nil {
		t.Fatalf("wire: %v", err)
	}
	return wire
}

func zeroBootstrapVectorBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}

func TestBootstrapLowOrderPublicKeysAreDenied(t *testing.T) {
	recipient, binding, envelope := testBootstrapEnvelope(t)
	defer recipient.Destroy()
	publicWire := func(value []byte) []byte {
		return []byte(`{"version":1,"publicKey":"` + base64.RawURLEncoding.EncodeToString(value) + `"}`)
	}
	for _, lowOrder := range [][]byte{make([]byte, 32), append([]byte{1}, make([]byte, 31)...)} {
		if _, err := ParseBootstrapRecipientPublicKey(publicWire(lowOrder)); err != ErrBootstrapDenied {
			t.Fatal("low-order recipient public key was accepted")
		}
	}
	var encoded bootstrapEnvelopeWire
	if err := json.Unmarshal(envelope, &encoded); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	for _, lowOrder := range [][]byte{make([]byte, 32), append([]byte{1}, make([]byte, 31)...)} {
		mutated := encoded
		mutated.SenderEphemeralPublicKey = base64.RawURLEncoding.EncodeToString(lowOrder)
		wire, err := json.Marshal(mutated)
		if err != nil {
			t.Fatalf("marshal low-order envelope: %v", err)
		}
		opened, err := recipient.OpenBootstrap(binding, wire)
		requireBootstrapDenied(t, err)
		opened.Destroy()
	}
	zeroRecipient := BootstrapRecipientPublicKey{version: BootstrapRecipientKeyVersion}
	grant := testBootstrapGrant(t, binding)
	defer grant.Destroy()
	if _, err := SealBootstrap(zeroRecipient, binding, grant); err != ErrBootstrapDenied {
		t.Fatal("all-zero recipient key was accepted by SealBootstrap")
	}
}

func TestBootstrapSecretTypesAreRedactionSafe(t *testing.T) {
	recipient, binding, envelope := testBootstrapEnvelope(t)
	defer recipient.Destroy()
	privateBytes := append([]byte(nil), recipient.private...)
	privateHex := hex.EncodeToString(privateBytes)
	zeroBytes(privateBytes)
	opened, err := recipient.OpenBootstrap(binding, envelope)
	if err != nil {
		t.Fatalf("OpenBootstrap: %v", err)
	}
	defer opened.Destroy()
	credential, kek := testBootstrapInputs()
	secretValues := []struct {
		name   string
		value  any
		canary string
	}{
		{"recipient", recipient, privateHex},
		{"credential", opened.Secrets.NodeCredential, hex.EncodeToString(credential)},
		{"kek", opened.Secrets.ClusterKEK, hex.EncodeToString(kek)},
		{"secrets", opened, hex.EncodeToString(credential)},
	}
	for _, test := range secretValues {
		t.Run(test.name, func(t *testing.T) {
			formatted := fmt.Sprintf("%v", test.value)
			if strings.Contains(formatted, test.canary) {
				t.Fatal("formatted value exposed secret material")
			}
			encoded, err := json.Marshal(test.value)
			if err != nil {
				t.Fatalf("json marshal: %v", err)
			}
			if bytes.Contains(encoded, []byte(test.canary)) {
				t.Fatal("JSON value exposed secret material")
			}
			if _, ok := test.value.(fmt.Stringer); ok {
				t.Fatal("secret type implements fmt.Stringer")
			}
			if _, ok := test.value.(encoding.TextMarshaler); ok {
				t.Fatal("secret type implements encoding.TextMarshaler")
			}
			if _, ok := test.value.(json.Marshaler); ok {
				t.Fatal("secret type implements json.Marshaler")
			}
		})
	}
}
