package backupcore

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func testManifest() Manifest {
	return Manifest{
		FormatVersion:   FormatVersion,
		CreatedAt:       "2026-01-02T03:04:05Z",
		ExporterVersion: "0.4.0",
		InstallationID:  "11111111-1111-4111-8111-111111111111",
		NodeID:          "node-1",
		Role:            "primary",
		EntryChecksums:  map[string]string{"db/users.json": strings.Repeat("a", 64), "env/env": strings.Repeat("b", 64)},
		DBTables:        []string{"users", "zones"},
		Certs:           []string{"certs/site.crt", "certs/site.key"},
		EnvPresent:      true,
		Counts:          map[string]int{"users": 2, "zones": 1},
	}
}

func TestManifestRoundTrip(t *testing.T) {
	want := testManifest()
	data, err := MarshalManifest(want)
	if err != nil {
		t.Fatalf("MarshalManifest: %v", err)
	}

	var got Manifest
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round-trip changed manifest fields:\nwant %#v\n got %#v", want, got)
	}
	gotData, err := MarshalManifest(got)
	if err != nil {
		t.Fatalf("MarshalManifest after unmarshal: %v", err)
	}
	if !bytes.Equal(data, gotData) {
		t.Fatalf("round-trip changed bytes:\nwant %s\n got %s", data, gotData)
	}
}

func TestManifestMarshalIsDeterministicAcrossRuns(t *testing.T) {
	first := testManifest()
	first.EntryChecksums = map[string]string{
		"z-last":  strings.Repeat("c", 64),
		"a-first": strings.Repeat("d", 64),
	}
	first.Counts = map[string]int{"z-last": 9, "a-first": 1}
	second := testManifest()
	second.EntryChecksums = map[string]string{
		"a-first": strings.Repeat("d", 64),
		"z-last":  strings.Repeat("c", 64),
	}
	second.Counts = map[string]int{"a-first": 1, "z-last": 9}

	want, err := MarshalManifest(first)
	if err != nil {
		t.Fatalf("MarshalManifest(first): %v", err)
	}
	got, err := MarshalManifest(second)
	if err != nil {
		t.Fatalf("MarshalManifest(second): %v", err)
	}
	if !bytes.Equal(want, got) {
		t.Fatalf("equivalent manifests have different JSON:\nwant %s\n got %s", want, got)
	}
	for run := 0; run < 10; run++ {
		repeated, err := MarshalManifest(first)
		if err != nil {
			t.Fatalf("MarshalManifest run %d: %v", run, err)
		}
		if !bytes.Equal(want, repeated) {
			t.Fatalf("run %d changed deterministic output", run)
		}
	}
}

func TestManifestValidateRejectsExtraTopLevelKey(t *testing.T) {
	data, err := MarshalManifest(testManifest())
	if err != nil {
		t.Fatalf("MarshalManifest: %v", err)
	}
	data = bytes.TrimSpace(data)
	data = append(data[:len(data)-1], []byte(`,"unexpected":true}`)...)

	var got Manifest
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if err := Validate(got); err == nil {
		t.Fatal("Validate accepted an unknown top-level key")
	}
}

func TestManifestValidateRejectsMissingRequiredKey(t *testing.T) {
	data, err := MarshalManifest(testManifest())
	if err != nil {
		t.Fatalf("MarshalManifest: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatalf("json.Unmarshal fields: %v", err)
	}
	delete(fields, "nodeId")
	missing, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("json.Marshal fields: %v", err)
	}

	var got Manifest
	if err := json.Unmarshal(missing, &got); err != nil {
		t.Fatalf("json.Unmarshal missing key: %v", err)
	}
	if err := Validate(got); err == nil {
		t.Fatal("Validate accepted a manifest missing nodeId")
	}
}

func TestManifestValidateRejectsWrongFormatVersion(t *testing.T) {
	manifest := testManifest()
	manifest.FormatVersion = "2"
	if err := Validate(manifest); err == nil {
		t.Fatal("Validate accepted an unsupported format version")
	}
}
