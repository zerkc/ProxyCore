package dbimport

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

func coerceImportBytea(value any) ([]byte, error) {
	encoded, ok := value.(string)
	if !ok {
		if bytes, ok := value.([]byte); ok {
			return append([]byte(nil), bytes...), nil
		}
		return nil, fmt.Errorf("bytea value has Go type %T", value)
	}
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		decoded, err = base64.URLEncoding.DecodeString(encoded)
	}
	if err != nil {
		return nil, fmt.Errorf("decode bytea: %w", err)
	}
	return decoded, nil
}

func parseImportTimestamp(value string) (time.Time, error) {
	if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return parsed, nil
	}
	for _, layout := range []string{
		"2006-01-02 15:04:05.999999999Z07:00",
		"2006-01-02 15:04:05.999999999",
	} {
		if strings.Contains(layout, "Z07:00") {
			if parsed, err := time.Parse(layout, value); err == nil {
				return parsed, nil
			}
			continue
		}
		if parsed, err := time.ParseInLocation(layout, value, time.UTC); err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid timestamp %q", value)
}

func coerceImportJSON(value any) ([]byte, error) {
	if raw, ok := value.(json.RawMessage); ok {
		if !json.Valid(raw) {
			return nil, fmt.Errorf("invalid JSON value")
		}
		return append([]byte(nil), raw...), nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode JSON value: %w", err)
	}
	return encoded, nil
}

func coerceImportBool(value any) (bool, error) {
	if parsed, ok := value.(bool); ok {
		return parsed, nil
	}
	encoded, ok := value.(string)
	if !ok {
		return false, fmt.Errorf("boolean value has Go type %T", value)
	}
	parsed, err := strconv.ParseBool(encoded)
	if err != nil {
		return false, fmt.Errorf("parse boolean value %q: %w", encoded, err)
	}
	return parsed, nil
}
