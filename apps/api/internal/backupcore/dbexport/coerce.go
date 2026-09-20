package dbexport

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

func coerceRows(rows pgx.Rows) ([]map[string]any, error) {
	if rows == nil {
		return nil, fmt.Errorf("coerce rows: nil rows")
	}
	defer rows.Close()

	fields := rows.FieldDescriptions()
	result := make([]map[string]any, 0)
	for rows.Next() {
		raw := rows.RawValues()
		values, err := rows.Values()
		if err != nil {
			return nil, fmt.Errorf("read row values: %w", err)
		}
		row, err := coerceRow(fields, values, raw, rows)
		if err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read rows: %w", err)
	}
	return result, nil
}

func coerceRow(fields []pgconn.FieldDescription, values []any, raw [][]byte, rows pgx.Rows) (map[string]any, error) {
	if len(fields) != len(values) {
		return nil, fmt.Errorf("row has %d values for %d columns", len(values), len(fields))
	}
	result := make(map[string]any, len(fields))
	typeMap := pgtype.NewMap()
	if conn := rows.Conn(); conn != nil {
		typeMap = conn.TypeMap()
	}
	for index, field := range fields {
		var typeInfo *pgtype.Type
		if typeMap != nil {
			typeInfo, _ = typeMap.TypeForOID(field.DataTypeOID)
		}
		var rawValue []byte
		if index < len(raw) {
			rawValue = raw[index]
		}
		coerced, err := coerceValueForOIDWithContext(field.DataTypeOID, values[index], typeInfo, typeMap, rawValue, field.Format)
		if err != nil {
			return nil, fmt.Errorf("column %q (OID %d): %w", field.Name, field.DataTypeOID, err)
		}
		result[field.Name] = coerced
	}
	return result, nil
}

func coerceValueForOID(oid uint32, value any, typeInfo *pgtype.Type) (any, error) {
	return coerceValueForOIDWithContext(oid, value, typeInfo, pgtype.NewMap(), nil, pgtype.TextFormatCode)
}

func coerceValueForOIDWithContext(oid uint32, value any, typeInfo *pgtype.Type, typeMap *pgtype.Map, raw []byte, format int16) (any, error) {
	if value == nil {
		return nil, nil
	}
	if coercer, ok := builtinTypeCoercer(oid); ok {
		return coercer(value)
	}
	if typeInfo == nil && typeMap != nil {
		typeInfo, _ = typeMap.TypeForOID(oid)
	}
	if typeInfo == nil || typeInfo.Codec == nil {
		return nil, &UnsupportedTypeError{OID: oid, TypeName: typeName(typeInfo)}
	}

	// Registered derived codecs are allowed as strings. Decode from the raw
	// wire value when available so custom enums/domains retain their text form.
	if raw != nil && typeMap != nil {
		decoded, err := typeInfo.Codec.DecodeDatabaseSQLValue(typeMap, oid, format, raw)
		if err != nil {
			return nil, fmt.Errorf("decode registered OID %d: %w", oid, err)
		}
		if decoded != nil {
			value = decoded
		}
	}
	coerced, err := coerceString(value)
	if err == nil {
		return coerced, nil
	}
	// The type was registered by pgx, so retaining its textual fmt.Stringer
	// form is safer than silently omitting an otherwise valid column.
	return fmt.Sprint(value), nil
}

func coerceBytea(value any) (any, error) {
	var bytes []byte
	switch value := value.(type) {
	case []byte:
		bytes = value
	case pgtype.DriverBytes:
		bytes = []byte(value)
	case pgtype.PreallocBytes:
		bytes = []byte(value)
	default:
		return nil, fmt.Errorf("bytea value has Go type %T", value)
	}
	if len(bytes) == 0 {
		return "", nil
	}
	return base64.RawURLEncoding.EncodeToString(bytes), nil
}

func coerceUUID(value any) (any, error) {
	switch value := value.(type) {
	case []byte:
		if len(value) == 16 {
			var uuid [16]byte
			copy(uuid[:], value)
			return formatUUID(uuid), nil
		}
		return string(value), nil
	default:
		return coerceString(value)
	}
}

func coerceString(value any) (any, error) {
	switch value := value.(type) {
	case string:
		return value, nil
	case []byte:
		return string(value), nil
	case pgtype.Text:
		if !value.Valid {
			return nil, nil
		}
		return value.String, nil
	case *pgtype.Text:
		if value == nil || !value.Valid {
			return nil, nil
		}
		return value.String, nil
	case pgtype.UUID:
		if !value.Valid {
			return nil, nil
		}
		return formatUUID(value.Bytes), nil
	case *pgtype.UUID:
		if value == nil || !value.Valid {
			return nil, nil
		}
		return formatUUID(value.Bytes), nil
	case [16]byte:
		return formatUUID(value), nil
	case netip.Prefix:
		return value.String(), nil
	case netip.Addr:
		return value.String(), nil
	case net.HardwareAddr:
		return value.String(), nil
	case fmt.Stringer:
		return value.String(), nil
	default:
		return nil, fmt.Errorf("string value has Go type %T", value)
	}
}

func formatUUID(value [16]byte) string {
	var encoded [36]byte
	hex.Encode(encoded[0:8], value[0:4])
	encoded[8] = '-'
	hex.Encode(encoded[9:13], value[4:6])
	encoded[13] = '-'
	hex.Encode(encoded[14:18], value[6:8])
	encoded[18] = '-'
	hex.Encode(encoded[19:23], value[8:10])
	encoded[23] = '-'
	hex.Encode(encoded[24:], value[10:])
	return string(encoded[:])
}

func coerceTimestamp(value any) (any, error) {
	switch value := value.(type) {
	case time.Time:
		return value.UTC().Format(time.RFC3339Nano), nil
	case pgtype.Timestamp:
		return formatPgTimestamp(value.Valid, value.InfinityModifier, value.Time, true)
	case *pgtype.Timestamp:
		if value == nil {
			return nil, nil
		}
		return formatPgTimestamp(value.Valid, value.InfinityModifier, value.Time, true)
	case pgtype.Timestamptz:
		return formatPgTimestamp(value.Valid, value.InfinityModifier, value.Time, false)
	case *pgtype.Timestamptz:
		if value == nil {
			return nil, nil
		}
		return formatPgTimestamp(value.Valid, value.InfinityModifier, value.Time, false)
	case pgtype.InfinityModifier:
		return value.String(), nil
	case string:
		if value == "infinity" || value == "-infinity" {
			return value, nil
		}
		parsed, err := parseTimestamp(value)
		if err != nil {
			return nil, err
		}
		return parsed.UTC().Format(time.RFC3339Nano), nil
	default:
		return nil, fmt.Errorf("timestamp value has Go type %T", value)
	}
}

func coerceTimestampWithoutZone(value any) (any, error) {
	if wall, ok := value.(time.Time); ok {
		return time.Date(wall.Year(), wall.Month(), wall.Day(), wall.Hour(), wall.Minute(), wall.Second(), wall.Nanosecond(), time.UTC).Format(time.RFC3339Nano), nil
	}
	return coerceTimestamp(value)
}

func formatPgTimestamp(valid bool, modifier pgtype.InfinityModifier, value time.Time, withoutZone bool) (any, error) {
	if !valid {
		return nil, nil
	}
	if modifier != pgtype.Finite {
		return modifier.String(), nil
	}
	if withoutZone {
		value = time.Date(value.Year(), value.Month(), value.Day(), value.Hour(), value.Minute(), value.Second(), value.Nanosecond(), time.UTC)
		return value.Format(time.RFC3339Nano), nil
	}
	return value.UTC().Format(time.RFC3339Nano), nil
}

func parseTimestamp(value string) (time.Time, error) {
	if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return parsed, nil
	}
	for _, layout := range []string{
		"2006-01-02 15:04:05.999999999Z07:00",
		"2006-01-02 15:04:05.999999999",
	} {
		location := time.UTC
		if strings.Contains(layout, "Z07:00") {
			if parsed, err := time.Parse(layout, value); err == nil {
				return parsed, nil
			}
			continue
		}
		if parsed, err := time.ParseInLocation(layout, value, location); err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid timestamp %q", value)
}

func coerceJSON(value any) (any, error) {
	switch value := value.(type) {
	case json.RawMessage:
		return decodeJSONBytes(value)
	case []byte:
		return decodeJSONBytes(value)
	case string:
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			return "", nil
		}
		var decoded any
		if err := json.Unmarshal([]byte(trimmed), &decoded); err == nil {
			return normalizeJSON(decoded)
		}
		return value, nil
	default:
		return normalizeJSON(value)
	}
}

func decodeJSONBytes(value []byte) (any, error) {
	if len(value) == 0 {
		return nil, fmt.Errorf("empty JSON value")
	}
	var decoded any
	if err := json.Unmarshal(value, &decoded); err != nil {
		return nil, fmt.Errorf("decode JSON: %w", err)
	}
	return normalizeJSON(decoded)
}

func normalizeJSON(value any) (any, error) {
	switch value := value.(type) {
	case nil, bool, string, float64:
		return value, nil
	case float32:
		return float64(value), nil
	case int:
		return float64(value), nil
	case int8:
		return float64(value), nil
	case int16:
		return float64(value), nil
	case int32:
		return float64(value), nil
	case int64:
		return float64(value), nil
	case uint:
		return float64(value), nil
	case uint8:
		return float64(value), nil
	case uint16:
		return float64(value), nil
	case uint32:
		return float64(value), nil
	case uint64:
		return float64(value), nil
	case json.Number:
		parsed, err := strconv.ParseFloat(string(value), 64)
		if err != nil {
			return nil, fmt.Errorf("JSON number %q: %w", value, err)
		}
		return parsed, nil
	case []any:
		result := make([]any, len(value))
		for index, element := range value {
			coerced, err := normalizeJSON(element)
			if err != nil {
				return nil, err
			}
			result[index] = coerced
		}
		return result, nil
	case map[string]any:
		result := make(map[string]any, len(value))
		for key, element := range value {
			coerced, err := normalizeJSON(element)
			if err != nil {
				return nil, err
			}
			result[key] = coerced
		}
		return result, nil
	default:
		return nil, fmt.Errorf("JSON value has unsupported Go type %T", value)
	}
}

func coerceBool(value any) (any, error) {
	switch value := value.(type) {
	case bool:
		return value, nil
	case pgtype.Bool:
		if !value.Valid {
			return nil, nil
		}
		return value.Bool, nil
	case *pgtype.Bool:
		if value == nil || !value.Valid {
			return nil, nil
		}
		return value.Bool, nil
	default:
		return nil, fmt.Errorf("boolean value has Go type %T", value)
	}
}

func coerceNumber(value any) (any, error) {
	var number float64
	switch value := value.(type) {
	case int:
		number = float64(value)
	case int8:
		number = float64(value)
	case int16:
		number = float64(value)
	case int32:
		number = float64(value)
	case int64:
		number = float64(value)
	case uint:
		number = float64(value)
	case uint8:
		number = float64(value)
	case uint16:
		number = float64(value)
	case uint32:
		number = float64(value)
	case uint64:
		number = float64(value)
	case float32:
		number = float64(value)
	case float64:
		number = value
	case string:
		parsed, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return nil, fmt.Errorf("parse numeric value %q: %w", value, err)
		}
		number = parsed
	case []byte:
		parsed, err := strconv.ParseFloat(string(value), 64)
		if err != nil {
			return nil, fmt.Errorf("parse numeric value %q: %w", value, err)
		}
		number = parsed
	case json.Number:
		parsed, err := strconv.ParseFloat(string(value), 64)
		if err != nil {
			return nil, fmt.Errorf("parse numeric value %q: %w", value, err)
		}
		number = parsed
	case pgtype.Int2:
		if !value.Valid {
			return nil, nil
		}
		number = float64(value.Int16)
	case pgtype.Int4:
		if !value.Valid {
			return nil, nil
		}
		number = float64(value.Int32)
	case pgtype.Int8:
		if !value.Valid {
			return nil, nil
		}
		number = float64(value.Int64)
	case pgtype.Float4:
		if !value.Valid {
			return nil, nil
		}
		number = float64(value.Float32)
	case pgtype.Float8:
		if !value.Valid {
			return nil, nil
		}
		number = value.Float64
	case pgtype.Numeric:
		if !value.Valid {
			return nil, nil
		}
		converted, err := value.Float64Value()
		if err != nil {
			return nil, err
		}
		if !converted.Valid {
			return nil, nil
		}
		number = converted.Float64
	default:
		return nil, fmt.Errorf("numeric value has Go type %T", value)
	}
	if math.IsNaN(number) || math.IsInf(number, 0) {
		return strconv.FormatFloat(number, 'g', -1, 64), nil
	}
	// PostgreSQL integer values are intentionally represented as float64 here:
	// JSON has no integer type, and integers below 2^53 remain lossless.
	return number, nil
}
