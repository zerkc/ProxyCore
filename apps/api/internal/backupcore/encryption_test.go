package backupcore

import (
	"bytes"
	"encoding/json"
	"testing"
)

const InsecureTestVerificationTag = "dGFn"

func TestNewEncryptionSetsAES256GCipher(t *testing.T) {
	encryption := NewEncryption(
		"pbkdf2-hmac-sha256",
		Params{N: 600000, R: 8, P: 1},
		"c2FsdA",
		InsecureTestVerificationTag,
	)
	if encryption.Cipher != CipherAES256GCM {
		t.Fatalf("Cipher = %q, want %q", encryption.Cipher, CipherAES256GCM)
	}
	if encryption.Params.N != 600000 || encryption.Params.R != 8 || encryption.Params.P != 1 {
		t.Fatalf("unexpected KDF params: %+v", encryption.Params)
	}
}

func TestEncryptionJSONUsesStableCamelCaseFields(t *testing.T) {
	encryption := NewEncryption(
		"pbkdf2-hmac-sha256",
		Params{N: 1, R: 8, P: 1},
		"c2FsdA",
		InsecureTestVerificationTag,
	)
	got, err := json.Marshal(encryption)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	want := []byte(`{"cipher":"aes-256-gcm","kdf":"pbkdf2-hmac-sha256","params":{"n":1,"p":1,"r":8},"salt":"c2FsdA","verificationTag":"dGFn"}`)
	if !bytes.Equal(got, want) {
		t.Fatalf("unexpected encryption JSON:\nwant %s\n got %s", want, got)
	}
}
