package enrollment

import (
	"bytes"
	"crypto/ecdh"
	"crypto/sha256"
	"encoding"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestBootstrapDeterministicVector(t *testing.T) {
	var recipientPrivateBytes [bootstrapPublicKeyBytes]byte
	for index := range recipientPrivateBytes {
		recipientPrivateBytes[index] = byte(0x20 + index)
	}
	recipientPrivate, err := ecdh.X25519().NewPrivateKey(recipientPrivateBytes[:])
	if err != nil {
		t.Fatalf("recipient private key: %v", err)
	}
	var recipientPublicBytes [bootstrapPublicKeyBytes]byte
	copy(recipientPublicBytes[:], recipientPrivate.PublicKey().Bytes())
	recipientPublic := BootstrapRecipientPublicKey{version: BootstrapRecipientKeyVersion, key: newSecretCopy(recipientPublicBytes[:])}
	if !validRecipientPublicKey(recipientPublic) {
		t.Fatal("deterministic recipient public key is invalid")
	}

	binding := BootstrapBinding{
		ProtocolVersion:             BootstrapProtocolVersion,
		AttemptID:                   "attempt-vector-01",
		SourcePrimaryInstallationID: "primary-vector-01",
		TargetInstallationID:        "target-vector-01",
		TargetNodeID:                "node-vector-01",
		LeadershipGeneration:        9,
		VerifiedPreviewDigest:       bytes.Repeat([]byte{0x5a}, 32),
	}
	credential := bytes.Repeat([]byte{0xa1}, bootstrapCredentialBytes)
	kek := bytes.Repeat([]byte{0xb2}, bootstrapKEKBytes)
	entropy := make([]byte, 0, bootstrapPublicKeyBytes+bootstrapNonceBytes)
	for index := 0; index < bootstrapPublicKeyBytes; index++ {
		entropy = append(entropy, byte(index))
	}
	for index := 0; index < bootstrapNonceBytes; index++ {
		entropy = append(entropy, byte(0xa0+index))
	}

	senderPrivate := entropy[:bootstrapPublicKeyBytes]
	nonce := entropy[bootstrapPublicKeyBytes:]
	wire, err := sealBootstrapWithFixedEphemeral(senderPrivate, nonce, recipientPublic, binding, credential, kek)
	if err != nil {
		t.Fatalf("deterministic seal: %v", err)
	}
	wireDigest := sha256.Sum256(wire)
	const wantWireDigest = "efe541dcba8e47c7aea7c47f8bf5736e2b38bc9cb27622dfe261ab7733f00497"
	if hex.EncodeToString(wireDigest[:]) != wantWireDigest {
		t.Fatal("deterministic bootstrap vector changed")
	}
	again, err := sealBootstrapWithFixedEphemeral(senderPrivate, nonce, recipientPublic, binding, credential, kek)
	if err != nil || !bytes.Equal(again, wire) {
		t.Fatal("deterministic entropy did not reproduce the vector")
	}

	recipient := BootstrapRecipient{private: newSecretCopy(recipientPrivateBytes[:])}
	defer recipient.Destroy()
	opened, err := recipient.OpenBootstrap(binding, wire)
	if err != nil {
		t.Fatalf("open deterministic vector: %v", err)
	}
	defer opened.Destroy()
	if !bytes.Equal(opened.NodeCredential.CopyBytes(), credential) || !bytes.Equal(opened.ClusterKEK.CopyBytes(), kek) {
		t.Fatal("deterministic vector secrets did not open")
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
	credential, kek := testBootstrapInputs()
	if _, err := SealBootstrap(zeroRecipient, binding, credential, kek); err != ErrBootstrapDenied {
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
		{"credential", opened.NodeCredential, hex.EncodeToString(credential)},
		{"kek", opened.ClusterKEK, hex.EncodeToString(kek)},
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
