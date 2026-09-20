package backupcore

import (
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

func TestValidateRejectsPathsThatEscapeBundleRoot(t *testing.T) {
	for _, path := range []string{
		"/absolute/path",
		"../outside.json",
		"db/../outside.json",
		"certs/../../outside.key",
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
	}
	if err := Validate(manifest); err != nil {
		t.Fatalf("Validate rejected relative paths: %v", err)
	}
}
