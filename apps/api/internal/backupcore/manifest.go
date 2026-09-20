package backupcore

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"time"
)

const FormatVersion = "1"

type Manifest struct {
	FormatVersion   string            `json:"formatVersion"`
	CreatedAt       string            `json:"createdAt"`
	ExporterVersion string            `json:"exporterVersion"`
	InstallationID  string            `json:"installationId"`
	NodeID          string            `json:"nodeId"`
	Role            string            `json:"role"`
	EntryChecksums  map[string]string `json:"entryChecksums"`
	DBTables        []string          `json:"dbTables"`
	Certs           []string          `json:"certs,omitempty"`
	EnvPresent      bool              `json:"envPresent"`
	Encryption      *Encryption       `json:"encryption,omitempty"`
	Counts          map[string]int    `json:"counts,omitempty"`

	unknownTopLevelKeys    []string
	duplicateTopLevelKeys  []string
	duplicateChecksumPaths []string
	invalidTopLevelFields  []string
	envPresentMissing      bool
}

func NewManifest(exporterVersion string) Manifest {
	return Manifest{
		FormatVersion:   FormatVersion,
		CreatedAt:       time.Now().UTC().Format(time.RFC3339),
		ExporterVersion: exporterVersion,
		EntryChecksums:  make(map[string]string),
		DBTables:        []string{},
		Certs:           []string{},
		Counts:          make(map[string]int),
	}
}

var manifestTopLevelKeys = map[string]struct{}{
	"formatVersion": {}, "createdAt": {}, "exporterVersion": {},
	"installationId": {}, "nodeId": {}, "role": {},
	"entryChecksums": {}, "dbTables": {}, "certs": {},
	"envPresent": {}, "encryption": {}, "counts": {},
}

type manifestWire Manifest

func (m *Manifest) UnmarshalJSON(data []byte) error {
	fields, topDuplicates, err := decodeJSONObject(data)
	if err != nil {
		return fmt.Errorf("decode manifest object: %w", err)
	}
	var wire manifestWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return fmt.Errorf("decode manifest fields: %w", err)
	}
	parsed := Manifest(wire)
	parsed.duplicateTopLevelKeys = append([]string(nil), topDuplicates...)
	for key := range fields {
		if _, known := manifestTopLevelKeys[key]; !known {
			parsed.unknownTopLevelKeys = append(parsed.unknownTopLevelKeys, key)
		}
	}
	if _, present := fields["envPresent"]; !present {
		parsed.envPresentMissing = true
	} else if isJSONNull(fields["envPresent"]) {
		parsed.invalidTopLevelFields = append(parsed.invalidTopLevelFields, "envPresent")
	}
	if raw, present := fields["entryChecksums"]; present && !isJSONNull(raw) {
		_, duplicates, err := decodeJSONObject(raw)
		if err != nil {
			return fmt.Errorf("decode entryChecksums object: %w", err)
		}
		parsed.duplicateChecksumPaths = append(parsed.duplicateChecksumPaths, duplicates...)
	}
	sort.Strings(parsed.unknownTopLevelKeys)
	sort.Strings(parsed.duplicateTopLevelKeys)
	sort.Strings(parsed.duplicateChecksumPaths)
	sort.Strings(parsed.invalidTopLevelFields)
	*m = parsed
	return nil
}

func isJSONNull(data []byte) bool {
	return bytes.Equal(bytes.TrimSpace(data), []byte("null"))
}

func decodeJSONObject(data []byte) (map[string]json.RawMessage, []string, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	first, err := decoder.Token()
	if err != nil {
		return nil, nil, err
	}
	if delim, ok := first.(json.Delim); !ok || delim != '{' {
		return nil, nil, fmt.Errorf("expected JSON object")
	}
	fields := make(map[string]json.RawMessage)
	var duplicates []string
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, nil, err
		}
		key, ok := token.(string)
		if !ok {
			return nil, nil, fmt.Errorf("object key is not a string")
		}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return nil, nil, err
		}
		if _, exists := fields[key]; exists {
			duplicates = append(duplicates, key)
		}
		fields[key] = append(json.RawMessage(nil), raw...)
	}
	if closing, err := decoder.Token(); err != nil || closing != json.Delim('}') {
		if err != nil {
			return nil, nil, err
		}
		return nil, nil, fmt.Errorf("object did not close")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, nil, fmt.Errorf("unexpected trailing JSON")
		}
		return nil, nil, err
	}
	return fields, duplicates, nil
}
