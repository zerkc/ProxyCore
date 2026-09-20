package dbexport

import (
	"encoding/base64"
	"errors"
	"math/big"
	"net/netip"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

func TestCoerceValueSupportsConfiguredPostgresTypes(t *testing.T) {
	when := time.Date(2026, time.January, 2, 3, 4, 5, 678900000, time.FixedZone("fixture", 2*60*60))
	uuidValue := pgtype.UUID{Bytes: [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}, Valid: true}
	numericValue := pgtype.Numeric{Int: big.NewInt(12345), Exp: -2, Valid: true}

	tests := []struct {
		name string
		oid  uint32
		in   any
		want any
	}{
		{name: "bytea", oid: pgtype.ByteaOID, in: []byte("binary"), want: base64.RawURLEncoding.EncodeToString([]byte("binary"))},
		{name: "empty bytea", oid: pgtype.ByteaOID, in: []byte{}, want: ""},
		{name: "text", oid: pgtype.TextOID, in: "text", want: "text"},
		{name: "uuid", oid: pgtype.UUIDOID, in: uuidValue, want: "01020304-0506-0708-090a-0b0c0d0e0f10"},
		{name: "timestamptz", oid: pgtype.TimestamptzOID, in: when, want: "2026-01-02T01:04:05.6789Z"},
		{name: "timestamp", oid: pgtype.TimestampOID, in: pgtype.Timestamp{Time: when, Valid: true}, want: "2026-01-02T03:04:05.6789Z"},
		{name: "numeric", oid: pgtype.NumericOID, in: numericValue, want: float64(123.45)},
		{name: "int2", oid: pgtype.Int2OID, in: int16(7), want: float64(7)},
		{name: "int4", oid: pgtype.Int4OID, in: int32(8), want: float64(8)},
		{name: "int8", oid: pgtype.Int8OID, in: int64(9), want: float64(9)},
		{name: "float4", oid: pgtype.Float4OID, in: float32(1.5), want: float64(1.5)},
		{name: "float8", oid: pgtype.Float8OID, in: float64(2.5), want: float64(2.5)},
		{name: "bool", oid: pgtype.BoolOID, in: true, want: true},
		{name: "inet", oid: pgtype.InetOID, in: netip.MustParsePrefix("192.0.2.1/32"), want: "192.0.2.1/32"},
		{name: "cidr", oid: pgtype.CIDROID, in: netip.MustParsePrefix("192.0.2.0/24"), want: "192.0.2.0/24"},
		{name: "macaddr", oid: pgtype.MacaddrOID, in: "08:00:2b:01:02:03", want: "08:00:2b:01:02:03"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := coerceValueForOID(tt.oid, tt.in, nil)
			if err != nil {
				t.Fatalf("coerceValueForOID: %v", err)
			}
			if got != tt.want {
				t.Fatalf("value = %#v (%T), want %#v (%T)", got, got, tt.want, tt.want)
			}
		})
	}
}

func TestCoerceValueRejectsUnknownOID(t *testing.T) {
	_, err := coerceValueForOID(999999, "opaque", nil)
	if err == nil {
		t.Fatal("coerceValueForOID accepted an unknown OID")
	}
	var unsupported *UnsupportedTypeError
	if !errors.As(err, &unsupported) {
		t.Fatalf("error = %v, want UnsupportedTypeError", err)
	}
	if unsupported.OID != 999999 {
		t.Fatalf("UnsupportedTypeError OID = %d, want 999999", unsupported.OID)
	}
}

func TestCoerceRegisteredCodecFallsBackToString(t *testing.T) {
	const citextOID = 424243
	registered := &pgtype.Type{Name: "public.citext", OID: citextOID, Codec: pgtype.TextCodec{}}
	got, err := coerceValueForOID(citextOID, "case-insensitive", registered)
	if err != nil {
		t.Fatalf("coerceValueForOID registered codec: %v", err)
	}
	if got != "case-insensitive" {
		t.Fatalf("registered value = %#v, want string", got)
	}
}

func TestCoerceBigintUsesJSONSafeNumberOrDecimalString(t *testing.T) {
	const exact = int64(1<<53 - 1)
	got, err := coerceValueForOID(pgtype.Int8OID, exact, nil)
	if err != nil {
		t.Fatalf("coerceValueForOID exact bigint: %v", err)
	}
	if got != float64(exact) {
		t.Fatalf("exact bigint = %#v, want %#v", got, float64(exact))
	}

	const large = int64(1<<53 + 1)
	got, err = coerceValueForOID(pgtype.Int8OID, large, nil)
	if err != nil {
		t.Fatalf("coerceValueForOID large bigint: %v", err)
	}
	if got != "9007199254740993" {
		t.Fatalf("large bigint = %#v, want decimal string %q", got, "9007199254740993")
	}
}
