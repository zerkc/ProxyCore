package enrollment

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func TestBootstrapRoundTrip(t *testing.T) {
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
	credential := bytes.Repeat([]byte{0x11}, 32)
	kek := bytes.Repeat([]byte{0x22}, 32)

	envelope, err := SealBootstrap(publicKey, binding, credential, kek)
	if err != nil {
		t.Fatalf("SealBootstrap: %v", err)
	}
	opened, err := recipient.OpenBootstrap(binding, envelope)
	if err != nil {
		t.Fatalf("OpenBootstrap: %v", err)
	}
	defer opened.Destroy()

	if got := opened.NodeCredential.CopyBytes(); !bytes.Equal(got, credential) {
		t.Fatal("credential mismatch")
	}
	if got := opened.ClusterKEK.CopyBytes(); !bytes.Equal(got, kek) {
		t.Fatal("KEK mismatch")
	}
}

func testBootstrapBinding() BootstrapBinding {
	return BootstrapBinding{
		ProtocolVersion:             BootstrapProtocolVersion,
		AttemptID:                   "attempt-1",
		SourcePrimaryInstallationID: "primary-1",
		TargetInstallationID:        "installation-1",
		TargetNodeID:                "node-1",
		LeadershipGeneration:        7,
		VerifiedPreviewDigest:       bytes.Repeat([]byte{0x42}, 32),
	}
}

func testBootstrapInputs() ([]byte, []byte) {
	return bytes.Repeat([]byte{0x11}, 32), bytes.Repeat([]byte{0x22}, 32)
}

func testBootstrapEnvelope(t *testing.T) (BootstrapRecipient, BootstrapBinding, []byte) {
	t.Helper()
	recipient, err := NewBootstrapRecipient()
	if err != nil {
		t.Fatalf("NewBootstrapRecipient: %v", err)
	}
	publicKey, err := recipient.PublicKey()
	if err != nil {
		recipient.Destroy()
		t.Fatalf("PublicKey: %v", err)
	}
	credential, kek := testBootstrapInputs()
	envelope, err := SealBootstrap(publicKey, testBootstrapBinding(), credential, kek)
	if err != nil {
		recipient.Destroy()
		t.Fatalf("SealBootstrap: %v", err)
	}
	return recipient, testBootstrapBinding(), envelope
}

func requireBootstrapDenied(t *testing.T, err error) {
	t.Helper()
	if err != ErrBootstrapDenied {
		t.Fatalf("error is not the stable bootstrap denial")
	}
}

func TestBootstrapPublicKeyWireIsCanonicalAndVersioned(t *testing.T) {
	recipient, err := NewBootstrapRecipient()
	if err != nil {
		t.Fatalf("NewBootstrapRecipient: %v", err)
	}
	defer recipient.Destroy()
	key, err := recipient.PublicKey()
	if err != nil {
		t.Fatalf("PublicKey: %v", err)
	}
	wire, err := key.Wire()
	if err != nil {
		t.Fatalf("Wire: %v", err)
	}
	parsed, err := ParseBootstrapRecipientPublicKey(wire)
	if err != nil {
		t.Fatalf("ParseBootstrapRecipientPublicKey: %v", err)
	}
	parsedWire, err := parsed.Wire()
	if err != nil || !bytes.Equal(parsedWire, wire) {
		t.Fatal("public-key wire did not round-trip canonically")
	}
	jsonWire, err := key.MarshalJSON()
	if err != nil || !bytes.Equal(jsonWire, wire) {
		t.Fatal("MarshalJSON did not use the canonical public-key wire")
	}

	var encoded bootstrapRecipientPublicKeyWire
	if err := json.Unmarshal(wire, &encoded); err != nil {
		t.Fatalf("decode public-key wire: %v", err)
	}
	for name, mutate := range map[string]func(*bootstrapRecipientPublicKeyWire){
		"version": func(value *bootstrapRecipientPublicKeyWire) { value.Version++ },
		"base64":  func(value *bootstrapRecipientPublicKeyWire) { value.PublicKey = "!" },
		"unknown": func(value *bootstrapRecipientPublicKeyWire) { value.PublicKey = value.PublicKey + " " },
	} {
		mutated := encoded
		mutate(&mutated)
		mutatedWire, err := json.Marshal(mutated)
		if err != nil {
			t.Fatalf("marshal %s case: %v", name, err)
		}
		if _, err := ParseBootstrapRecipientPublicKey(mutatedWire); err != ErrBootstrapDenied {
			t.Fatalf("public-key %s case did not use the stable denial", name)
		}
	}
	for _, malformed := range [][]byte{
		append([]byte(nil), wire[:len(wire)-1]...),
		append(append([]byte(nil), wire...), '\n'),
		append(append([]byte(nil), wire...), []byte("{}")...),
		[]byte(`{"version":1,"publicKey":"` + encoded.PublicKey + `","extra":1}`),
	} {
		if _, err := ParseBootstrapRecipientPublicKey(malformed); err != ErrBootstrapDenied {
			t.Fatal("malformed public-key wire did not use the stable denial")
		}
	}
}

func TestBootstrapBindingAndSecretShapeValidation(t *testing.T) {
	recipient, err := NewBootstrapRecipient()
	if err != nil {
		t.Fatalf("NewBootstrapRecipient: %v", err)
	}
	defer recipient.Destroy()
	publicKey, err := recipient.PublicKey()
	if err != nil {
		t.Fatalf("PublicKey: %v", err)
	}
	credential, kek := testBootstrapInputs()
	base := testBootstrapBinding()
	cases := []struct {
		name string
		edit func(*BootstrapBinding)
	}{
		{"protocol", func(value *BootstrapBinding) { value.ProtocolVersion++ }},
		{"attempt", func(value *BootstrapBinding) { value.AttemptID = "" }},
		{"primary", func(value *BootstrapBinding) { value.SourcePrimaryInstallationID = "" }},
		{"installation", func(value *BootstrapBinding) { value.TargetInstallationID = "" }},
		{"node", func(value *BootstrapBinding) { value.TargetNodeID = "" }},
		{"generation", func(value *BootstrapBinding) { value.LeadershipGeneration = 0 }},
		{"digest-empty", func(value *BootstrapBinding) { value.VerifiedPreviewDigest = nil }},
		{"digest-oversized", func(value *BootstrapBinding) { value.VerifiedPreviewDigest = bytes.Repeat([]byte{1}, 33) }},
		{"oversized-id", func(value *BootstrapBinding) { value.AttemptID = strings.Repeat("a", bootstrapMaxBindingStringBytes+1) }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			binding := base
			binding.VerifiedPreviewDigest = append([]byte(nil), base.VerifiedPreviewDigest...)
			test.edit(&binding)
			if _, err := SealBootstrap(publicKey, binding, credential, kek); err != ErrBootstrapDenied {
				t.Fatal("invalid binding was not denied")
			}
			if _, err := binding.CanonicalBytes(); err != ErrBootstrapDenied {
				t.Fatal("invalid binding framing was not denied")
			}
		})
	}
	for name, sizes := range map[string][2]int{
		"credential-short": {31, 32},
		"credential-long":  {33, 32},
		"kek-short":        {32, 31},
		"kek-long":         {32, 33},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := SealBootstrap(publicKey, base, bytes.Repeat([]byte{1}, sizes[0]), bytes.Repeat([]byte{2}, sizes[1])); err != ErrBootstrapDenied {
				t.Fatal("invalid secret shape was not denied")
			}
		})
	}
}

func TestBootstrapBindsEveryFieldAndRecipientKey(t *testing.T) {
	recipient, binding, envelope := testBootstrapEnvelope(t)
	defer recipient.Destroy()
	base := binding
	cases := []struct {
		name string
		edit func(*BootstrapBinding)
	}{
		{"protocol", func(value *BootstrapBinding) { value.ProtocolVersion++ }},
		{"attempt", func(value *BootstrapBinding) { value.AttemptID += "-other" }},
		{"primary", func(value *BootstrapBinding) { value.SourcePrimaryInstallationID += "-other" }},
		{"installation", func(value *BootstrapBinding) { value.TargetInstallationID += "-other" }},
		{"node", func(value *BootstrapBinding) { value.TargetNodeID += "-other" }},
		{"generation", func(value *BootstrapBinding) { value.LeadershipGeneration++ }},
		{"digest", func(value *BootstrapBinding) { value.VerifiedPreviewDigest[0] ^= 1 }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			candidate := base
			candidate.VerifiedPreviewDigest = append([]byte(nil), base.VerifiedPreviewDigest...)
			test.edit(&candidate)
			opened, err := recipient.OpenBootstrap(candidate, envelope)
			requireBootstrapDenied(t, err)
			opened.Destroy()
		})
	}

	other, err := NewBootstrapRecipient()
	if err != nil {
		t.Fatalf("second recipient: %v", err)
	}
	defer other.Destroy()
	opened, err := other.OpenBootstrap(binding, envelope)
	requireBootstrapDenied(t, err)
	opened.Destroy()
}

func TestBootstrapEnvelopeTamperAndStrictBounds(t *testing.T) {
	recipient, binding, envelope := testBootstrapEnvelope(t)
	defer recipient.Destroy()
	var encoded bootstrapEnvelopeWire
	if err := json.Unmarshal(envelope, &encoded); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	cases := []struct {
		name string
		edit func(*bootstrapEnvelopeWire)
	}{
		{"version", func(value *bootstrapEnvelopeWire) { value.Version++ }},
		{"sender", func(value *bootstrapEnvelopeWire) {
			value.SenderEphemeralPublicKey = flipWireBit(value.SenderEphemeralPublicKey, bootstrapPublicKeyBytes)
		}},
		{"nonce", func(value *bootstrapEnvelopeWire) { value.Nonce = flipWireBit(value.Nonce, bootstrapNonceBytes) }},
		{"ciphertext", func(value *bootstrapEnvelopeWire) {
			value.Ciphertext = flipWireBit(value.Ciphertext, bootstrapCiphertextBytes)
		}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			mutated := encoded
			test.edit(&mutated)
			wire, err := json.Marshal(mutated)
			if err != nil {
				t.Fatalf("marshal tampered envelope: %v", err)
			}
			opened, err := recipient.OpenBootstrap(binding, wire)
			requireBootstrapDenied(t, err)
			opened.Destroy()
		})
	}
	malformed := []bootstrapEnvelopeWire{
		encoded,
		encoded,
		encoded,
	}
	malformed[0].SenderEphemeralPublicKey = "!"
	malformed[1].Nonce = "!"
	malformed[2].Ciphertext = strings.Repeat("A", base64.RawURLEncoding.EncodedLen(bootstrapCiphertextBytes)+1)
	for _, value := range malformed {
		wire, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("marshal malformed envelope: %v", err)
		}
		opened, err := recipient.OpenBootstrap(binding, wire)
		requireBootstrapDenied(t, err)
		opened.Destroy()
	}
	for _, wire := range [][]byte{
		envelope[:len(envelope)-1],
		append(append([]byte(nil), envelope...), '\n'),
		append(append([]byte(nil), envelope...), []byte("{}")...),
		[]byte(`{"version":1,"senderEphemeralPublicKey":"` + encoded.SenderEphemeralPublicKey + `","nonce":"` + encoded.Nonce + `","ciphertext":"` + encoded.Ciphertext + `","extra":1}`),
		bytes.Repeat([]byte{'x'}, bootstrapMaxWireBytes+1),
	} {
		opened, err := recipient.OpenBootstrap(binding, wire)
		requireBootstrapDenied(t, err)
		opened.Destroy()
	}
}

func TestBootstrapReplayRandomnessAndSecretOwnership(t *testing.T) {
	recipient, binding, envelope := testBootstrapEnvelope(t)
	defer recipient.Destroy()
	replay := append([]byte(nil), envelope...)
	first, err := recipient.OpenBootstrap(binding, replay)
	if err != nil {
		t.Fatalf("first replay: %v", err)
	}
	second, err := recipient.OpenBootstrap(binding, replay)
	if err != nil {
		first.Destroy()
		t.Fatalf("second replay: %v", err)
	}
	credentialCopy := first.NodeCredential.CopyBytes()
	kekCopy := first.ClusterKEK.CopyBytes()
	credentialCopy[0] ^= 1
	kekCopy[0] ^= 1
	if bytes.Equal(credentialCopy, first.NodeCredential.CopyBytes()) || bytes.Equal(kekCopy, first.ClusterKEK.CopyBytes()) {
		first.Destroy()
		second.Destroy()
		t.Fatal("CopyBytes did not return independent memory")
	}
	first.Destroy()
	if len(first.NodeCredential.CopyBytes()) != 0 || len(first.ClusterKEK.CopyBytes()) != 0 {
		second.Destroy()
		t.Fatal("Destroy did not clear the first secret owner")
	}
	second.Destroy()
	if len(second.NodeCredential.CopyBytes()) != 0 || len(second.ClusterKEK.CopyBytes()) != 0 {
		t.Fatal("Destroy did not clear the replayed secret owner")
	}

	publicKey, err := recipient.PublicKey()
	if err != nil {
		t.Fatalf("PublicKey after replay: %v", err)
	}
	credential, kek := testBootstrapInputs()
	otherEnvelope, err := SealBootstrap(publicKey, binding, credential, kek)
	if err != nil {
		t.Fatalf("second seal: %v", err)
	}
	if bytes.Equal(envelope, otherEnvelope) {
		t.Fatal("fresh sealing reused the exact envelope bytes")
	}
}

func flipWireBit(value string, expectedBytes int) string {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(decoded) != expectedBytes {
		return "!"
	}
	decoded[0] ^= 1
	return base64.RawURLEncoding.EncodeToString(decoded)
}
