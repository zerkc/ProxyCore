package dbimport

import (
	"encoding/base64"
	"encoding/json"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

func TestCoerceImportValueRoundTripsExportTypeMatrix(t *testing.T) {
	tests := []struct {
		name string
		oid  uint32
		in   any
		want any
	}{
		{name: "bytea", oid: pgtype.ByteaOID, in: base64.RawURLEncoding.EncodeToString([]byte{0, 1, 255}), want: []byte{0, 1, 255}},
		{name: "timestamptz", oid: pgtype.TimestamptzOID, in: "2026-01-02T03:04:05.6789Z", want: time.Date(2026, time.January, 2, 3, 4, 5, 678900000, time.UTC)},
		{name: "uuid", oid: pgtype.UUIDOID, in: "01000000-0000-0000-0000-000000000000", want: pgtype.UUID{Bytes: [16]byte{1}, Valid: true}},
		{name: "jsonb", oid: pgtype.JSONBOID, in: map[string]any{"large": json.Number("9007199254740993"), "ok": true}, want: []byte(`{"large":9007199254740993,"ok":true}`)},
		{name: "enum", oid: 900001, in: "A", want: "A"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := coerceImportValue(tt.oid, tt.in)
			if err != nil {
				t.Fatalf("coerceImportValue: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("value = %#v (%T), want %#v (%T)", got, got, tt.want, tt.want)
			}
		})
	}
}

func TestCoerceImportValuePreservesBigintsAboveJSONSafeRange(t *testing.T) {
	for _, input := range []any{json.Number("9007199254740993"), "-9007199254740993"} {
		got, err := coerceImportValue(pgtype.Int8OID, input)
		if err != nil {
			t.Fatalf("coerceImportValue(%v): %v", input, err)
		}
		value, ok := got.(int64)
		if !ok {
			t.Fatalf("value = %#v (%T), want int64", got, got)
		}
		if input == json.Number("9007199254740993") && value != 9007199254740993 {
			t.Fatalf("positive bigint = %d, want exact value", value)
		}
		if input == "-9007199254740993" && value != -9007199254740993 {
			t.Fatalf("negative bigint = %d, want exact value", value)
		}
	}
	if math.MaxInt64 <= 9007199254740993 {
		t.Fatal("test assumes large bigint fits int64")
	}
}
