package backupcore

import (
	"bytes"
	"testing"
)

func TestMarshalManifestGoldenBytes(t *testing.T) {
	manifest := testManifest()
	manifest.Encryption = &Encryption{
		Kdf:             "pbkdf2-hmac-sha256",
		Params:          Params{N: 1, R: 8, P: 1},
		Salt:            "c2FsdA",
		VerificationTag: InsecureTestVerificationTag,
		Cipher:          CipherAES256GCM,
	}
	want := []byte("{\"certs\":[\"certs/site.crt\",\"certs/site.key\"],\"counts\":{\"users\":2,\"zones\":1},\"createdAt\":\"2026-01-02T03:04:05Z\",\"dbTables\":[\"users\",\"zones\"],\"encryption\":{\"cipher\":\"aes-256-gcm\",\"kdf\":\"pbkdf2-hmac-sha256\",\"params\":{\"n\":1,\"p\":1,\"r\":8},\"salt\":\"c2FsdA\",\"verificationTag\":\"dGFn\"},\"entryChecksums\":{\"db/users.json\":\"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\",\"env/env\":\"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\"},\"envPresent\":true,\"exporterVersion\":\"0.4.0\",\"formatVersion\":\"1\",\"installationId\":\"11111111-1111-4111-8111-111111111111\",\"nodeId\":\"node-1\",\"role\":\"primary\"}\n")

	got, err := MarshalManifest(manifest)
	if err != nil {
		t.Fatalf("MarshalManifest: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("manifest bytes differ from golden:\nwant %s\n got %s", want, got)
	}
	if len(got) == 0 || got[len(got)-1] != '\n' {
		t.Fatal("manifest output must end with one newline")
	}
	if bytes.Contains(bytes.TrimSuffix(got, []byte{'\n'}), []byte{'\n'}) {
		t.Fatal("manifest output contains whitespace after the JSON object")
	}
}
