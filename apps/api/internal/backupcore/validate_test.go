package backupcore

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func TestValidateAcceptsManifest(t *testing.T) {
	if err := Validate(testManifest()); err != nil {
		t.Fatalf("Validate(valid manifest): %v", err)
	}
}

func TestValidateRejectsMissingRequiredKeys(t *testing.T) {
	data, err := MarshalManifest(testManifest())
	if err != nil {
		t.Fatalf("MarshalManifest: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatalf("json.Unmarshal fields: %v", err)
	}

	required := []string{
		"formatVersion",
		"createdAt",
		"exporterVersion",
		"installationId",
		"nodeId",
		"role",
		"entryChecksums",
		"dbTables",
		"envPresent",
	}
	for _, key := range required {
		t.Run(key, func(t *testing.T) {
			without := make(map[string]json.RawMessage, len(fields)-1)
			for field, value := range fields {
				if field != key {
					without[field] = value
				}
			}
			missing, err := json.Marshal(without)
			if err != nil {
				t.Fatalf("json.Marshal without %s: %v", key, err)
			}
			var manifest Manifest
			if err := json.Unmarshal(missing, &manifest); err != nil {
				t.Fatalf("json.Unmarshal without %s: %v", key, err)
			}
			if err := Validate(manifest); err == nil {
				t.Fatalf("Validate accepted manifest without %s", key)
			}
		})
	}
}

func TestValidateRejectsUnknownTopLevelKey(t *testing.T) {
	data, err := MarshalManifest(testManifest())
	if err != nil {
		t.Fatalf("MarshalManifest: %v", err)
	}
	text := strings.TrimSpace(string(data))
	text = text[:len(text)-1] + `,"futureField":true}`

	var manifest Manifest
	if err := json.Unmarshal([]byte(text), &manifest); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if err := Validate(manifest); err == nil {
		t.Fatal("Validate accepted an unknown top-level key")
	}
}

func TestValidateRejectsWrongFormatVersion(t *testing.T) {
	manifest := testManifest()
	manifest.FormatVersion = "2"
	if err := Validate(manifest); err == nil {
		t.Fatal("Validate accepted formatVersion=2")
	}
}

func TestValidateRejectsDuplicateEntryChecksumPaths(t *testing.T) {
	data := `{"formatVersion":"1","createdAt":"2026-01-02T03:04:05Z","exporterVersion":"0.4.0","installationId":"11111111-1111-4111-8111-111111111111","nodeId":"node-1","role":"primary","entryChecksums":{"db/users.json":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","db/users.json":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},"dbTables":["users"],"envPresent":false}`
	var manifest Manifest
	if err := json.Unmarshal([]byte(data), &manifest); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if err := Validate(manifest); err == nil {
		t.Fatal("Validate accepted duplicate entry checksum paths")
	}
}

func TestValidateRejectsTamperedChecksum(t *testing.T) {
	manifest := testManifest()
	manifest.EntryChecksums["db/users.json"] = strings.Repeat("a", 63)
	if err := Validate(manifest); err == nil {
		t.Fatal("Validate accepted a malformed/tampered checksum")
	}
}

func TestValidateRejectsNonHexChecksum(t *testing.T) {
	manifest := testManifest()
	manifest.EntryChecksums["db/users.json"] = strings.Repeat("g", 64)
	if err := Validate(manifest); err == nil {
		t.Fatal("Validate accepted a 64-character non-hex checksum")
	}
}

func TestValidateRejectsPathsThatEscapeBundleRoot(t *testing.T) {
	for _, path := range []string{
		"/absolute/path",
		"../outside.json",
		"db/../outside.json",
		"certs/../../outside.key",
		"C:/x",
		"C:\\x",
		"c:/x",
		"cert/C:/x.json",
	} {
		t.Run(path, func(t *testing.T) {
			manifest := testManifest()
			manifest.EntryChecksums = map[string]string{path: strings.Repeat("a", 64)}
			if err := Validate(manifest); err == nil {
				t.Fatalf("Validate accepted escaping path %q", path)
			}
		})
	}
}

func TestValidateAcceptsRelativeEntryPaths(t *testing.T) {
	manifest := testManifest()
	manifest.EntryChecksums = map[string]string{
		"db/nested/users.json": strings.Repeat("a", 64),
		"certs/site.crt":       strings.Repeat("b", 64),
		"relative/path.json":   strings.Repeat("c", 64),
		"nested/dir/file":      strings.Repeat("d", 64),
	}
	if err := Validate(manifest); err != nil {
		t.Fatalf("Validate rejected relative paths: %v", err)
	}
}

func TestValidateRejectsEscapingDBTablePath(t *testing.T) {
	for _, tablePath := range []string{"../outside", "db/../outside", "C:/outside"} {
		t.Run(tablePath, func(t *testing.T) {
			manifest := testManifest()
			manifest.DBTables = []string{"users", tablePath, "later/escape"}
			err := Validate(manifest)
			if err == nil {
				t.Fatalf("Validate accepted escaping DB table path %q", tablePath)
			}
			if !strings.Contains(err.Error(), tablePath) {
				t.Fatalf("error = %v, want first offending path %q", err, tablePath)
			}
		})
	}
}

func validEncryptionMetadata() *Encryption {
	return &Encryption{
		Cipher:          CipherAES256GCM,
		Kdf:             "pbkdf2-sha256",
		Params:          Params{N: 1, R: 1, P: 1},
		Salt:            base64.RawURLEncoding.EncodeToString([]byte("0123456789abcdef")),
		VerificationTag: base64.RawURLEncoding.EncodeToString([]byte("fedcba9876543210")),
	}
}

func TestValidateAcceptsOptionalEncryptionMetadata(t *testing.T) {
	t.Run("nil", func(t *testing.T) {
		if err := Validate(testManifest()); err != nil {
			t.Fatalf("Validate(manifest without encryption): %v", err)
		}
	})

	t.Run("valid", func(t *testing.T) {
		manifest := testManifest()
		manifest.Encryption = validEncryptionMetadata()
		if err := Validate(manifest); err != nil {
			t.Fatalf("Validate(valid encryption): %v", err)
		}
	})
}

func TestValidateReportsEachInvalidEncryptionFieldOnce(t *testing.T) {
	tests := []struct {
		name   string
		field  string
		mutate func(*Encryption)
	}{
		{name: "cipher", field: "encryption.cipher", mutate: func(e *Encryption) { e.Cipher = "chacha20-poly1305" }},
		{name: "kdf", field: "encryption.kdf", mutate: func(e *Encryption) { e.Kdf = "scrypt" }},
		{name: "n zero", field: "encryption.params.n", mutate: func(e *Encryption) { e.Params.N = 0 }},
		{name: "n negative", field: "encryption.params.n", mutate: func(e *Encryption) { e.Params.N = -1 }},
		{name: "r zero", field: "encryption.params.r", mutate: func(e *Encryption) { e.Params.R = 0 }},
		{name: "r negative", field: "encryption.params.r", mutate: func(e *Encryption) { e.Params.R = -1 }},
		{name: "p zero", field: "encryption.params.p", mutate: func(e *Encryption) { e.Params.P = 0 }},
		{name: "p negative", field: "encryption.params.p", mutate: func(e *Encryption) { e.Params.P = -1 }},
		{name: "salt malformed", field: "encryption.salt", mutate: func(e *Encryption) { e.Salt = "not base64url!" }},
		{name: "salt wrong length", field: "encryption.salt", mutate: func(e *Encryption) { e.Salt = base64.RawURLEncoding.EncodeToString([]byte("short")) }},
		{name: "tag malformed", field: "encryption.verificationTag", mutate: func(e *Encryption) { e.VerificationTag = "not base64url!" }},
		{name: "tag wrong length", field: "encryption.verificationTag", mutate: func(e *Encryption) { e.VerificationTag = base64.RawURLEncoding.EncodeToString([]byte("short")) }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manifest := testManifest()
			manifest.Encryption = validEncryptionMetadata()
			tt.mutate(manifest.Encryption)
			err := Validate(manifest)
			if err == nil {
				t.Fatalf("Validate accepted invalid %s", tt.field)
			}
			if count := strings.Count(err.Error(), tt.field); count != 1 {
				t.Fatalf("error = %q, want exactly one %q occurrence (got %d)", err, tt.field, count)
			}
		})
	}
}
