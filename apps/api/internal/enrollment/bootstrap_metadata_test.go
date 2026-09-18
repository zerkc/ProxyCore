package enrollment

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/binary"
	"testing"
)

func TestBootstrapGrantRoundTripCarriesAuthenticatedMetadata(t *testing.T) {
	recipient, err := NewBootstrapRecipient()
	if err != nil {
		t.Fatalf("NewBootstrapRecipient: %v", err)
	}
	defer recipient.Destroy()

	publicKey, err := recipient.PublicKey()
	if err != nil {
		t.Fatalf("PublicKey: %v", err)
	}
	binding := testBootstrapBinding()
	grant := testBootstrapGrant(t, binding)
	defer grant.Destroy()

	wire, err := SealBootstrap(publicKey, binding, grant)
	if err != nil {
		t.Fatalf("SealBootstrap: %v", err)
	}
	opened, err := recipient.OpenBootstrap(binding, wire)
	if err != nil {
		t.Fatalf("OpenBootstrap: %v", err)
	}
	defer opened.Destroy()

	if opened.Metadata != grant.Metadata {
		t.Fatalf("metadata mismatch: got %#v want %#v", opened.Metadata, grant.Metadata)
	}
	if got := opened.Secrets.NodeCredential.CopyBytes(); !bytes.Equal(got, bytes.Repeat([]byte{0x11}, bootstrapCredentialBytes)) {
		t.Fatal("credential mismatch")
	}
	if got := opened.Secrets.ClusterKEK.CopyBytes(); !bytes.Equal(got, bytes.Repeat([]byte{0x22}, bootstrapKEKBytes)) {
		t.Fatal("KEK mismatch")
	}
}

func TestBootstrapGrantRejectsAuthenticatedMetadataMismatch(t *testing.T) {
	recipient, binding, envelope := testBootstrapEnvelope(t)
	defer recipient.Destroy()
	for _, field := range []string{"protocol", "primary", "installation", "node", "attempt", "generation"} {
		t.Run(field, func(t *testing.T) {
			wire := rewriteBootstrapPlaintext(t, recipient, binding, envelope, func(plaintext []byte) []byte {
				mutateBootstrapGrantField(t, plaintext, field)
				return plaintext
			})
			opened, err := recipient.OpenBootstrap(binding, wire)
			requireBootstrapDenied(t, err)
			opened.Destroy()
		})
	}
}

func TestBootstrapGrantRejectsTruncatedAndTrailingPlaintext(t *testing.T) {
	recipient, binding, envelope := testBootstrapEnvelope(t)
	defer recipient.Destroy()
	cases := map[string]func([]byte) []byte{
		"truncated": func(plaintext []byte) []byte { return plaintext[:len(plaintext)-1] },
		"trailing":  func(plaintext []byte) []byte { return append(plaintext, 0) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			wire := rewriteBootstrapPlaintext(t, recipient, binding, envelope, mutate)
			opened, err := recipient.OpenBootstrap(binding, wire)
			requireBootstrapDenied(t, err)
			opened.Destroy()
		})
	}
}

func TestBootstrapGrantRejectsNonCanonicalPlaintext(t *testing.T) {
	recipient, binding, envelope := testBootstrapEnvelope(t)
	defer recipient.Destroy()
	wire := rewriteBootstrapPlaintext(t, recipient, binding, envelope, func(plaintext []byte) []byte {
		offset := bootstrapGrantIDOffset(t, plaintext, "credential")
		plaintext[offset+4] = 0xff
		return plaintext
	})
	opened, err := recipient.OpenBootstrap(binding, wire)
	requireBootstrapDenied(t, err)
	opened.Destroy()
}

func TestBootstrapGrantEncryptedPayloadRejectsIDBounds(t *testing.T) {
	recipient, binding, envelope := testBootstrapEnvelope(t)
	defer recipient.Destroy()
	fields := []string{"credential", "cluster", "primary", "installation", "node", "attempt"}
	for _, field := range fields {
		for _, test := range []struct {
			name   string
			length uint32
		}{
			{"empty", 0},
			{"oversized", bootstrapMaxBindingStringBytes + 1},
		} {
			t.Run(field+"-"+test.name, func(t *testing.T) {
				wire := rewriteBootstrapPlaintext(t, recipient, binding, envelope, func(plaintext []byte) []byte {
					setBootstrapGrantIDLength(t, plaintext, field, test.length)
					return plaintext
				})
				opened, err := recipient.OpenBootstrap(binding, wire)
				requireBootstrapDenied(t, err)
				opened.Destroy()
			})
		}
	}
}

func TestBootstrapGrantRejectsEmptyAndOversizedIDs(t *testing.T) {
	binding := testBootstrapBinding()
	base := testBootstrapGrant(t, binding)
	defer base.Destroy()
	cases := []struct {
		name string
		edit func(*BootstrapGrantMetadata, int)
	}{
		{"credential-empty", func(metadata *BootstrapGrantMetadata, _ int) { metadata.CredentialID = "" }},
		{"credential-oversized", func(metadata *BootstrapGrantMetadata, max int) {
			metadata.CredentialID = string(bytes.Repeat([]byte{'x'}, max+1))
		}},
		{"cluster-empty", func(metadata *BootstrapGrantMetadata, _ int) { metadata.ClusterKeyID = "" }},
		{"cluster-oversized", func(metadata *BootstrapGrantMetadata, max int) {
			metadata.ClusterKeyID = string(bytes.Repeat([]byte{'x'}, max+1))
		}},
		{"primary-empty", func(metadata *BootstrapGrantMetadata, _ int) { metadata.SourcePrimaryInstallationID = "" }},
		{"primary-oversized", func(metadata *BootstrapGrantMetadata, max int) {
			metadata.SourcePrimaryInstallationID = string(bytes.Repeat([]byte{'x'}, max+1))
		}},
		{"installation-empty", func(metadata *BootstrapGrantMetadata, _ int) { metadata.TargetInstallationID = "" }},
		{"installation-oversized", func(metadata *BootstrapGrantMetadata, max int) {
			metadata.TargetInstallationID = string(bytes.Repeat([]byte{'x'}, max+1))
		}},
		{"node-empty", func(metadata *BootstrapGrantMetadata, _ int) { metadata.TargetNodeID = "" }},
		{"node-oversized", func(metadata *BootstrapGrantMetadata, max int) {
			metadata.TargetNodeID = string(bytes.Repeat([]byte{'x'}, max+1))
		}},
		{"attempt-empty", func(metadata *BootstrapGrantMetadata, _ int) { metadata.AttemptID = "" }},
		{"attempt-oversized", func(metadata *BootstrapGrantMetadata, max int) {
			metadata.AttemptID = string(bytes.Repeat([]byte{'x'}, max+1))
		}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			metadata := base.Metadata
			test.edit(&metadata, bootstrapMaxBindingStringBytes)
			credential := base.Secrets.NodeCredential.CopyBytes()
			kek := base.Secrets.ClusterKEK.CopyBytes()
			candidate, err := NewBootstrapGrant(metadata, credential, kek)
			candidate.Destroy()
			if err != ErrBootstrapDenied {
				t.Fatalf("invalid ID was not denied: %v", err)
			}
		})
	}
}

func TestBootstrapGrantExpiryIsValidatedButNotClockChecked(t *testing.T) {
	binding := testBootstrapBinding()
	base := testBootstrapGrant(t, binding)
	credential := base.Secrets.NodeCredential.CopyBytes()
	kek := base.Secrets.ClusterKEK.CopyBytes()
	base.Destroy()

	for _, expiry := range []int64{0, -1} {
		metadata := testBootstrapMetadata(t, binding)
		metadata.ExpiresAtUnix = expiry
		candidate, err := NewBootstrapGrant(metadata, credential, kek)
		candidate.Destroy()
		if err != ErrBootstrapDenied {
			t.Fatalf("invalid expiry %d was not denied", expiry)
		}
	}

	metadata := testBootstrapMetadata(t, binding)
	metadata.ExpiresAtUnix = 1
	grant, err := NewBootstrapGrant(metadata, credential, kek)
	if err != nil {
		t.Fatalf("expired grant construction: %v", err)
	}
	defer grant.Destroy()
	recipient, err := NewBootstrapRecipient()
	if err != nil {
		t.Fatalf("NewBootstrapRecipient: %v", err)
	}
	defer recipient.Destroy()
	publicKey, err := recipient.PublicKey()
	if err != nil {
		t.Fatalf("PublicKey: %v", err)
	}
	wire, err := SealBootstrap(publicKey, binding, grant)
	if err != nil {
		t.Fatalf("SealBootstrap: %v", err)
	}
	opened, err := recipient.OpenBootstrap(binding, wire)
	if err != nil {
		t.Fatalf("OpenBootstrap: %v", err)
	}
	defer opened.Destroy()
	if opened.Metadata.ExpiresAtUnix != 1 {
		t.Fatalf("expiry changed: got %d", opened.Metadata.ExpiresAtUnix)
	}

	wire = rewriteBootstrapPlaintext(t, recipient, binding, wire, func(plaintext []byte) []byte {
		offset := bootstrapExpiryOffset(t, plaintext)
		binary.BigEndian.PutUint64(plaintext[offset:offset+8], 0)
		return plaintext
	})
	denied, err := recipient.OpenBootstrap(binding, wire)
	requireBootstrapDenied(t, err)
	denied.Destroy()
}

func rewriteBootstrapPlaintext(t *testing.T, recipient BootstrapRecipient, binding BootstrapBinding, wire []byte, mutate func([]byte) []byte) []byte {
	t.Helper()
	envelope, ok := parseBootstrapEnvelopeV2(wire)
	if !ok {
		t.Fatal("test envelope did not parse")
	}
	private, err := ecdh.X25519().NewPrivateKey(recipient.private)
	if err != nil {
		t.Fatalf("test recipient private key: %v", err)
	}
	sender, err := ecdh.X25519().NewPublicKey(envelope.senderEphemeralPublicKey)
	if err != nil {
		t.Fatalf("test sender public key: %v", err)
	}
	shared, err := private.ECDH(sender)
	if err != nil {
		t.Fatalf("test shared key: %v", err)
	}
	defer zeroBytes(shared)
	aad, err := binding.CanonicalBytes()
	if err != nil {
		t.Fatalf("test AAD: %v", err)
	}
	key, err := hkdf.Key(sha256.New, shared, aad, bootstrapHKDFInfo, 32)
	if err != nil {
		t.Fatalf("test HKDF: %v", err)
	}
	defer zeroBytes(key)
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatalf("test AES: %v", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("test GCM: %v", err)
	}
	plaintext, err := gcm.Open(nil, envelope.nonce, envelope.ciphertext, aad)
	if err != nil {
		t.Fatalf("test decrypt: %v", err)
	}
	defer zeroBytes(plaintext)
	plaintext = mutate(plaintext)
	ciphertext := gcm.Seal(nil, envelope.nonce, plaintext, aad)
	defer zeroBytes(ciphertext)
	mutated, err := marshalBootstrapEnvelopeV2(envelope.senderEphemeralPublicKey, envelope.nonce, ciphertext)
	if err != nil {
		t.Fatalf("test envelope: %v", err)
	}
	return mutated
}

func setBootstrapGrantIDLength(t *testing.T, plaintext []byte, field string, length uint32) {
	offset := bootstrapGrantIDOffset(t, plaintext, field)
	binary.BigEndian.PutUint32(plaintext[offset:offset+4], length)
}

func bootstrapGrantIDOffset(t *testing.T, plaintext []byte, field string) int {
	t.Helper()
	reader := bootstrapPlaintextReader{value: plaintext}
	if _, ok := reader.uint32(); !ok {
		t.Fatal("missing plaintext version")
	}
	if _, ok := reader.uint32(); !ok {
		t.Fatal("missing protocol version")
	}
	fields := []struct {
		name string
		size int
	}{
		{"credential", bootstrapCredentialBytes},
		{"cluster", bootstrapKEKBytes},
		{"primary", 0},
		{"installation", 0},
		{"node", 0},
		{"attempt", 0},
	}
	for _, candidate := range fields {
		start := reader.offset
		if _, ok := reader.identifier(); !ok {
			t.Fatalf("missing %s ID", candidate.name)
		}
		if candidate.name == field {
			return start
		}
		if candidate.size > 0 {
			if _, ok := reader.fixed(candidate.size); !ok {
				t.Fatalf("missing %s secret", candidate.name)
			}
		}
	}
	t.Fatalf("unknown bootstrap ID %q", field)
	return 0
}

func mutateBootstrapGrantField(t *testing.T, plaintext []byte, field string) {
	t.Helper()
	if field == "protocol" {
		binary.BigEndian.PutUint32(plaintext[4:8], BootstrapProtocolVersion+1)
		return
	}
	reader := bootstrapPlaintextReader{value: plaintext}
	if _, ok := reader.uint32(); !ok {
		t.Fatal("missing plaintext version")
	}
	if _, ok := reader.uint32(); !ok {
		t.Fatal("missing protocol version")
	}
	if _, ok := reader.identifier(); !ok {
		t.Fatal("missing credential ID")
	}
	if _, ok := reader.fixed(bootstrapCredentialBytes); !ok {
		t.Fatal("missing credential")
	}
	if _, ok := reader.identifier(); !ok {
		t.Fatal("missing cluster key ID")
	}
	if _, ok := reader.fixed(bootstrapKEKBytes); !ok {
		t.Fatal("missing KEK")
	}
	for _, name := range []string{"primary", "installation", "node", "attempt"} {
		start := reader.offset
		if _, ok := reader.identifier(); !ok {
			t.Fatalf("missing %s ID", name)
		}
		if name == field {
			plaintext[start+4] ^= 1
			return
		}
	}
	if field == "generation" {
		start := reader.offset
		if _, ok := reader.uint64(); !ok {
			t.Fatal("missing generation")
		}
		plaintext[start+7] ^= 1
		return
	}
	t.Fatalf("unknown bootstrap field %q", field)
}

func bootstrapExpiryOffset(t *testing.T, plaintext []byte) int {
	t.Helper()
	reader := bootstrapPlaintextReader{value: plaintext}
	if _, ok := reader.uint32(); !ok {
		t.Fatal("missing plaintext version")
	}
	if _, ok := reader.uint32(); !ok {
		t.Fatal("missing protocol version")
	}
	for _, size := range []int{0, bootstrapCredentialBytes, 0, bootstrapKEKBytes} {
		if size == 0 {
			if _, ok := reader.identifier(); !ok {
				t.Fatal("missing grant ID")
			}
		} else if _, ok := reader.fixed(size); !ok {
			t.Fatal("missing grant secret")
		}
	}
	for index := 0; index < 4; index++ {
		if _, ok := reader.identifier(); !ok {
			t.Fatal("missing identity ID")
		}
	}
	if _, ok := reader.uint64(); !ok {
		t.Fatal("missing generation")
	}
	return reader.offset
}
