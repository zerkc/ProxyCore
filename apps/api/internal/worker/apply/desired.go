package apply

import (
	"bytes"
	"encoding/json"
)

// DesiredSnapshot unwraps the replication envelope. A node renders a near-exact
// copy of the primary with one exception: its own ingress. The envelope's
// nodeLocal.ingress is authoritative on the receiving installation (the API
// importer rewrites it with the node's own address), so its non-empty ipv4/ipv6
// strings replace settings.ingress wholesale, with only the keys that are set.
// The substitution happens at render time, not in the stored envelope, so the
// primary's contentHash and the revision checksum stay verifiable. A snapshot
// that is not an envelope is returned unchanged. Port of desiredSnapshotForApply.
func DesiredSnapshot(snapshot json.RawMessage) json.RawMessage {
	top, ok := decodeObject(snapshot)
	if !ok {
		return snapshot
	}
	replicated, ok := decodeObject(top["replicated"])
	if !ok {
		return snapshot
	}
	configuration, ok := replicated["configuration"]
	desired, isObject := decodeObject(configuration)
	if !ok || !isObject {
		return snapshot
	}
	var ipv4, ipv6 string
	if nodeLocal, ok := decodeObject(top["nodeLocal"]); ok {
		if ingress, ok := decodeObject(nodeLocal["ingress"]); ok {
			ipv4, ipv6 = nonEmptyString(ingress["ipv4"]), nonEmptyString(ingress["ipv6"])
		}
	}
	settings, hasSettings := decodeObject(desired["settings"])
	if (ipv4 == "" && ipv6 == "") || !hasSettings {
		return configuration
	}
	ingress := map[string]string{}
	if ipv4 != "" {
		ingress["ipv4"] = ipv4
	}
	if ipv6 != "" {
		ingress["ipv6"] = ipv6
	}
	settings["ingress"] = mustMarshal(ingress)
	desired["settings"] = mustMarshal(settings)
	return mustMarshal(desired)
}

func decodeObject(raw json.RawMessage) (map[string]json.RawMessage, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, false
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &object); err != nil || object == nil {
		return nil, false
	}
	return object, true
}

func nonEmptyString(raw json.RawMessage) string {
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return ""
	}
	return value
}

// mustMarshal encodes values built from already-valid JSON and plain strings,
// without HTML escaping so stored text stays byte-faithful.
func mustMarshal(value any) json.RawMessage {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		panic(err)
	}
	return json.RawMessage(bytes.TrimRight(buf.Bytes(), "\n"))
}
