package backupcore

import "encoding/json"

// MarshalManifest emits the canonical manifest representation: lexicographic
// object keys, compact JSON, and exactly one trailing newline.
func MarshalManifest(m Manifest) ([]byte, error) {
	data, err := json.Marshal(manifestJSONValue(m))
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// MarshalJSON makes the regular encoding/json path use the same canonical
// object shape as MarshalManifest, without adding the manifest newline.
func (m Manifest) MarshalJSON() ([]byte, error) {
	return json.Marshal(manifestJSONValue(m))
}

func manifestJSONValue(m Manifest) map[string]any {
	value := map[string]any{
		"createdAt":       m.CreatedAt,
		"dbTables":        m.DBTables,
		"envPresent":      m.EnvPresent,
		"entryChecksums":  m.EntryChecksums,
		"exporterVersion": m.ExporterVersion,
		"formatVersion":   m.FormatVersion,
		"installationId":  m.InstallationID,
		"nodeId":          m.NodeID,
		"role":            m.Role,
	}
	if len(m.Certs) > 0 {
		value["certs"] = m.Certs
	}
	if m.Encryption != nil {
		value["encryption"] = m.Encryption
	}
	if len(m.Counts) > 0 {
		value["counts"] = m.Counts
	}
	return value
}
