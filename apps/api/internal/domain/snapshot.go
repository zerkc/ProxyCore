package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ConfigurationSnapshot is the desired configuration captured for a revision.
type ConfigurationSnapshot struct {
	Settings     Settings            `json:"settings"`
	Zones        []ZoneState         `json:"zones"`
	Streams      []StreamRoute       `json:"streams"`
	Certificates []CertificateStatus `json:"certificates"`
}

func itoa(value int) string { return strconv.Itoa(value) }

// NormalizeSnapshot mirrors createSnapshot: JSON.parse(stableStringify(value)).
func NormalizeSnapshot(value any) (any, error) {
	stable := StableStringify(value)
	var normalized any
	if err := json.Unmarshal([]byte(stable), &normalized); err != nil {
		return nil, err
	}
	return normalized, nil
}

// ChecksumSnapshot returns the sha256 hex of the stable stringification.
func ChecksumSnapshot(value any) string {
	sum := sha256.Sum256([]byte(StableStringify(value)))
	return hex.EncodeToString(sum[:])
}

// StableStringify mirrors the Node stableStringify: deterministic key ordering,
// Date values rendered as ISO strings, undefined/function/symbol dropped.
func StableStringify(value any) string {
	// Round-trip through encoding/json so struct tags, omitempty, and time
	// formatting are applied consistently before deterministic serialization.
	marshaled, err := json.Marshal(jsonReady(value))
	if err != nil {
		return "null"
	}
	var tree any
	if err := json.Unmarshal(marshaled, &tree); err != nil {
		return "null"
	}
	var builder strings.Builder
	writeStable(&builder, tree)
	return builder.String()
}

// jsonReady converts time.Time values into the millisecond ISO strings Node
// emits, so checksums stay stable regardless of Go's default RFC3339 output.
func jsonReady(value any) any {
	switch v := value.(type) {
	case time.Time:
		return v.UTC().Format("2006-01-02T15:04:05.000Z07:00")
	case *time.Time:
		if v == nil {
			return nil
		}
		return v.UTC().Format("2006-01-02T15:04:05.000Z07:00")
	default:
		return value
	}
}

func writeStable(builder *strings.Builder, value any) {
	switch v := value.(type) {
	case nil:
		builder.WriteString("null")
	case bool:
		if v {
			builder.WriteString("true")
		} else {
			builder.WriteString("false")
		}
	case float64:
		builder.WriteString(formatNumber(v))
	case string:
		encoded, _ := json.Marshal(v)
		builder.Write(encoded)
	case []any:
		builder.WriteByte('[')
		for i, item := range v {
			if i > 0 {
				builder.WriteByte(',')
			}
			writeStable(builder, item)
		}
		builder.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		builder.WriteByte('{')
		for i, key := range keys {
			if i > 0 {
				builder.WriteByte(',')
			}
			encodedKey, _ := json.Marshal(key)
			builder.Write(encodedKey)
			builder.WriteByte(':')
			writeStable(builder, v[key])
		}
		builder.WriteByte('}')
	default:
		encoded, err := json.Marshal(v)
		if err != nil {
			builder.WriteString("null")
			return
		}
		builder.Write(encoded)
	}
}

func formatNumber(value float64) string {
	if math.IsInf(value, 0) || math.IsNaN(value) {
		return "null"
	}
	if value == math.Trunc(value) && math.Abs(value) < 1e21 {
		return strconv.FormatFloat(value, 'f', -1, 64)
	}
	return strconv.FormatFloat(value, 'g', -1, 64)
}
