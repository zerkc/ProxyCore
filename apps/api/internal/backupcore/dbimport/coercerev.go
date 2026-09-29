package dbimport

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// coerceImportValue reverses the JSON representation emitted by dbexport. The
// JSON decoder used by the importer retains json.Number values so PostgreSQL
// bigint values above the IEEE-754 safe range never pass through float64.
func coerceImportValue(oid uint32, value any) (any, error) {
	if value == nil {
		return nil, nil
	}

	switch oid {
	case pgtype.ByteaOID:
		return coerceImportBytea(value)
	case pgtype.UUIDOID:
		return coerceImportUUID(value)
	case pgtype.TimestamptzOID:
		return coerceImportTimestamp(value, false)
	case pgtype.TimestampOID:
		return coerceImportTimestamp(value, true)
	case pgtype.JSONBOID, pgtype.JSONOID:
		return coerceImportJSON(value)
	case pgtype.BoolOID:
		return coerceImportBool(value)
	case pgtype.Int2OID:
		parsed, err := parseImportInt(value, 16)
		return int16(parsed), err
	case pgtype.Int4OID:
		parsed, err := parseImportInt(value, 32)
		return int32(parsed), err
	case pgtype.Int8OID:
		return parseImportInt(value, 64)
	case pgtype.NumericOID:
		return parseImportNumeric(value)
	case pgtype.Float4OID:
		parsed, err := parseImportFloat(value, 32)
		return float32(parsed), err
	case pgtype.Float8OID:
		return parseImportFloat(value, 64)
	case pgtype.TextOID, pgtype.VarcharOID, pgtype.BPCharOID, pgtype.QCharOID,
		pgtype.NameOID, pgtype.InetOID, pgtype.CIDROID, pgtype.MacaddrOID:
		return coerceImportString(value)
	default:
		// Enumerated and domain columns are represented as strings by dbexport.
		// Passing JSON-native values through keeps custom types usable while
		// preserving a useful error for values pgx cannot encode.
		if number, ok := value.(json.Number); ok {
			return number.String(), nil
		}
		return value, nil
	}
}

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

func coerceImportUUID(value any) (pgtype.UUID, error) {
	encoded, ok := value.(string)
	if !ok {
		return pgtype.UUID{}, fmt.Errorf("uuid value has Go type %T", value)
	}
	compact := strings.ReplaceAll(encoded, "-", "")
	if len(compact) != 32 {
		return pgtype.UUID{}, fmt.Errorf("invalid UUID %q", encoded)
	}
	decoded, err := hex.DecodeString(compact)
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("invalid UUID %q: %w", encoded, err)
	}
	var bytes [16]byte
	copy(bytes[:], decoded)
	return pgtype.UUID{Bytes: bytes, Valid: true}, nil
}

func coerceImportTimestamp(value any, withoutZone bool) (any, error) {
	encoded, ok := value.(string)
	if !ok {
		return nil, fmt.Errorf("timestamp value has Go type %T", value)
	}
	if encoded == "infinity" || encoded == "-infinity" {
		modifier := pgtype.Infinity
		if encoded == "-infinity" {
			modifier = pgtype.NegativeInfinity
		}
		if withoutZone {
			return pgtype.Timestamp{InfinityModifier: modifier, Valid: true}, nil
		}
		return pgtype.Timestamptz{InfinityModifier: modifier, Valid: true}, nil
	}
	parsed, err := parseImportTimestamp(encoded)
	if err != nil {
		return nil, err
	}
	if withoutZone {
		return time.Date(parsed.Year(), parsed.Month(), parsed.Day(), parsed.Hour(), parsed.Minute(), parsed.Second(), parsed.Nanosecond(), time.UTC), nil
	}
	return parsed.UTC(), nil
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

func parseImportInt(value any, bitSize int) (int64, error) {
	var encoded string
	switch value := value.(type) {
	case json.Number:
		encoded = value.String()
	case string:
		encoded = value
	case int:
		return int64(value), nil
	case int8:
		return int64(value), nil
	case int16:
		return int64(value), nil
	case int32:
		return int64(value), nil
	case int64:
		return value, nil
	case float64:
		if math.Trunc(value) != value || value < math.MinInt64 || value > math.MaxInt64 {
			return 0, fmt.Errorf("integer value %v is out of range", value)
		}
		return int64(value), nil
	default:
		return 0, fmt.Errorf("integer value has Go type %T", value)
	}
	parsed, err := strconv.ParseInt(encoded, 10, bitSize)
	if err != nil {
		return 0, fmt.Errorf("parse integer value %q: %w", encoded, err)
	}
	return parsed, nil
}

func parseImportFloat(value any, bitSize int) (float64, error) {
	switch value := value.(type) {
	case json.Number:
		parsed, err := strconv.ParseFloat(value.String(), bitSize)
		if err != nil {
			return 0, fmt.Errorf("parse float value %q: %w", value, err)
		}
		return parsed, nil
	case string:
		parsed, err := strconv.ParseFloat(value, bitSize)
		if err != nil {
			return 0, fmt.Errorf("parse float value %q: %w", value, err)
		}
		return parsed, nil
	case float32:
		return float64(value), nil
	case float64:
		return value, nil
	case int:
		return float64(value), nil
	case int64:
		return float64(value), nil
	default:
		return 0, fmt.Errorf("float value has Go type %T", value)
	}
}

func parseImportNumeric(value any) (string, error) {
	switch value := value.(type) {
	case json.Number:
		return value.String(), nil
	case string:
		return value, nil
	case float64:
		return strconv.FormatFloat(value, 'g', -1, 64), nil
	case float32:
		return strconv.FormatFloat(float64(value), 'g', -1, 32), nil
	case int:
		return strconv.FormatInt(int64(value), 10), nil
	case int64:
		return strconv.FormatInt(value, 10), nil
	default:
		return "", fmt.Errorf("numeric value has Go type %T", value)
	}
}

func coerceImportString(value any) (string, error) {
	switch value := value.(type) {
	case string:
		return value, nil
	case json.Number:
		return value.String(), nil
	case []byte:
		return string(value), nil
	default:
		return "", fmt.Errorf("string value has Go type %T", value)
	}
}
