package dbexport

import (
	"errors"
	"math/big"
	"net/netip"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

type fakeRows struct {
	fields []pgconn.FieldDescription
	values [][]any
	index  int
	err    error
}

func newFakeRows(fields []pgconn.FieldDescription, values ...[]any) *fakeRows {
	return &fakeRows{fields: fields, values: values, index: -1}
}

func (r *fakeRows) Close()                                       {}
func (r *fakeRows) Err() error                                   { return r.err }
func (r *fakeRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *fakeRows) FieldDescriptions() []pgconn.FieldDescription { return r.fields }
func (r *fakeRows) Next() bool {
	r.index++
	return r.index < len(r.values)
}
func (r *fakeRows) Scan(dest ...any) error { return errors.New("fakeRows.Scan is not implemented") }
func (r *fakeRows) Values() ([]any, error) {
	if r.index < 0 || r.index >= len(r.values) {
		return nil, errors.New("fakeRows.Values called outside a row")
	}
	return append([]any(nil), r.values[r.index]...), nil
}
func (r *fakeRows) RawValues() [][]byte { return nil }
func (r *fakeRows) Conn() *pgx.Conn     { return nil }

var _ pgx.Rows = (*fakeRows)(nil)

func TestCoerceRowsConvertsSupportedOIDsAndPreservesColumnNames(t *testing.T) {
	when := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	fields := []pgconn.FieldDescription{
		{Name: "bytes", DataTypeOID: pgtype.ByteaOID},
		{Name: "empty_bytes", DataTypeOID: pgtype.ByteaOID},
		{Name: "label", DataTypeOID: pgtype.TextOID},
		{Name: "id", DataTypeOID: pgtype.UUIDOID},
		{Name: "created_at", DataTypeOID: pgtype.TimestamptzOID},
		{Name: "count", DataTypeOID: pgtype.Int8OID},
		{Name: "active", DataTypeOID: pgtype.BoolOID},
		{Name: "payload", DataTypeOID: pgtype.JSONBOID},
		{Name: "address", DataTypeOID: pgtype.InetOID},
	}
	values := []any{
		[]byte("binary"),
		[]byte{},
		"hello",
		pgtype.UUID{Bytes: [16]byte{1}, Valid: true},
		when,
		int64(4),
		true,
		[]byte(`{"nested":[1,true,null],"message":"unicode ✓"}`),
		netip.MustParsePrefix("192.0.2.1/32"),
	}

	got, err := coerceRows(newFakeRows(fields, values))
	if err != nil {
		t.Fatalf("coerceRows: %v", err)
	}
	want := []map[string]any{{
		"bytes":       "YmluYXJ5",
		"empty_bytes": "",
		"label":       "hello",
		"id":          "01000000-0000-0000-0000-000000000000",
		"created_at":  "2026-01-02T03:04:05Z",
		"count":       float64(4),
		"active":      true,
		"payload": map[string]any{
			"nested":  []any{float64(1), true, nil},
			"message": "unicode ✓",
		},
		"address": "192.0.2.1/32",
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("coerceRows = %#v, want %#v", got, want)
	}
}

func TestCoerceRowsMapsNULLToJSONNull(t *testing.T) {
	fields := []pgconn.FieldDescription{
		{Name: "nullable_uuid", DataTypeOID: pgtype.UUIDOID},
		{Name: "nullable_time", DataTypeOID: pgtype.TimestamptzOID},
		{Name: "nullable_json", DataTypeOID: pgtype.JSONBOID},
		{Name: "nullable_bytes", DataTypeOID: pgtype.ByteaOID},
	}
	got, err := coerceRows(newFakeRows(fields, []any{nil, nil, nil, nil}))
	if err != nil {
		t.Fatalf("coerceRows: %v", err)
	}
	for _, name := range []string{"nullable_uuid", "nullable_time", "nullable_json", "nullable_bytes"} {
		if value, ok := got[0][name]; !ok || value != nil {
			t.Fatalf("%s = %#v, want JSON null represented by nil", name, value)
		}
	}
}

func TestCoerceRowsJSONRoundTrip(t *testing.T) {
	fields := []pgconn.FieldDescription{{Name: "document", DataTypeOID: pgtype.JSONOID}}
	input := []byte(`{"object":{"number":1.25,"array":[false,"x"]}}`)
	got, err := coerceRows(newFakeRows(fields, []any{input}))
	if err != nil {
		t.Fatalf("coerceRows: %v", err)
	}
	want := map[string]any{
		"object": map[string]any{
			"number": float64(1.25),
			"array":  []any{false, "x"},
		},
	}
	if !reflect.DeepEqual(got[0]["document"], want) {
		t.Fatalf("document = %#v, want %#v", got[0]["document"], want)
	}
}

func TestCoerceRowsBigintBoundaries(t *testing.T) {
	fields := []pgconn.FieldDescription{{Name: "value", DataTypeOID: pgtype.Int8OID}}
	rows := newFakeRows(fields, []any{int64(1<<53 - 1)}, []any{int64(1<<53 + 1)})
	got, err := coerceRows(rows)
	if err != nil {
		t.Fatalf("coerceRows: %v", err)
	}
	if got[0]["value"] != float64(1<<53-1) {
		t.Fatalf("exact bigint = %#v, want %#v", got[0]["value"], float64(1<<53-1))
	}
	// Values above the IEEE-754 integer-safe range round to the nearest float64.
	if got[1]["value"] != float64(1<<53) {
		t.Fatalf("rounded bigint = %#v, want %#v", got[1]["value"], float64(1<<53))
	}
}

func TestCoerceRowsReportsUnsupportedOID(t *testing.T) {
	rows := newFakeRows(
		[]pgconn.FieldDescription{{Name: "mystery", DataTypeOID: 424242}},
		[]any{"value"},
	)
	_, err := coerceRows(rows)
	if err == nil {
		t.Fatal("coerceRows accepted an unsupported OID")
	}
	var unsupported *UnsupportedTypeError
	if !errors.As(err, &unsupported) {
		t.Fatalf("error = %v, want UnsupportedTypeError", err)
	}
}

func TestCoerceNumericValueFromPgtype(t *testing.T) {
	value := pgtype.Numeric{Int: big.NewInt(123456), Exp: -3, Valid: true}
	got, err := coerceValueForOID(pgtype.NumericOID, value, nil)
	if err != nil {
		t.Fatalf("coerceValueForOID: %v", err)
	}
	if got != float64(123.456) {
		t.Fatalf("numeric = %#v, want %#v", got, float64(123.456))
	}
}
