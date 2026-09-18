package sync

import (
	"bytes"
	"testing"
)

func FuzzParseNodeCredentialNoPanic(f *testing.F) {
	valid, err := NewNodeCredential("00112233-4455-4667-8899-aabbccddeeff", bytes.Repeat([]byte{0x19}, NodeCredentialSecretBytes))
	if err != nil {
		f.Fatal(err)
	}
	f.Add(valid.BearerCopy())
	valid.Destroy()

	f.Fuzz(func(t *testing.T, candidate string) {
		credential, err := ParseNodeCredential(candidate)
		if err == nil {
			_ = credential.ID()
			_ = credential.CopyBytes()
			_ = credential.Hash()
			_ = credential.BearerCopy()
			credential.Destroy()
		}
	})
}

func FuzzAuthenticateNodeCredentialNoPanic(f *testing.F) {
	credential, err := NewNodeCredential("00112233-4455-4667-8899-aabbccddeeff", bytes.Repeat([]byte{0x29}, NodeCredentialSecretBytes))
	if err != nil {
		f.Fatal(err)
	}
	record := validCredentialRecord(credential, "ffeeddcc-bbaa-4999-8877-665544332211")
	f.Add(credential.BearerCopy(), record.Hash)
	credential.Destroy()

	f.Fuzz(func(t *testing.T, presented, storedHash string) {
		candidate := record
		candidate.Hash = storedHash
		_, _ = AuthenticateNodeCredential(presented, &candidate)
	})
}
