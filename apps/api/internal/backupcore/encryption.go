package backupcore

import "encoding/json"

// CipherAES256GCM is the only cipher used for an encrypted backup bundle.
const CipherAES256GCM = "aes-256-gcm"

// KDFPBKDF2HMACSHA256 is the only key-derivation function accepted for an
// encrypted backup bundle.
const KDFPBKDF2HMACSHA256 = "pbkdf2-hmac-sha256"

// Params contains the integer work parameters recorded for the key derivation
// function. The exact KDF policy is selected by the caller and identified by
// Encryption.Kdf.
type Params struct {
	N int `json:"n"`
	R int `json:"r"`
	P int `json:"p"`
}

// EncryptionParams and KDFParams are descriptive aliases for callers that
// prefer an explicit name for the parameter object.
type EncryptionParams = Params
type KDFParams = Params

// Encryption records the optional wrapping metadata for a backup bundle.
type Encryption struct {
	Kdf             string `json:"kdf"`
	Params          Params `json:"params"`
	Salt            string `json:"salt"`
	VerificationTag string `json:"verificationTag"`
	Cipher          string `json:"cipher"`
}

// NewEncryption constructs encryption metadata and pins the cipher to the
// bundle format's AES-256-GCM value.
func NewEncryption(kdf string, params Params, salt, verificationTag string) *Encryption {
	if kdf == "" {
		kdf = KDFPBKDF2HMACSHA256
	}
	return &Encryption{
		Kdf:             kdf,
		Params:          params,
		Salt:            salt,
		VerificationTag: verificationTag,
		Cipher:          CipherAES256GCM,
	}
}

// MarshalJSON keeps encryption object keys deterministic and prevents callers
// from serializing a cipher value other than the format's fixed cipher.
func (e Encryption) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]any{
		"cipher":          CipherAES256GCM,
		"kdf":             e.Kdf,
		"params":          map[string]int{"n": e.Params.N, "p": e.Params.P, "r": e.Params.R},
		"salt":            e.Salt,
		"verificationTag": e.VerificationTag,
	})
}
