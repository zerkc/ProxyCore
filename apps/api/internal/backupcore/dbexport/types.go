package dbexport

import (
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"
)

// UnsupportedTypeError prevents a column from silently disappearing from an
// export when PostgreSQL returns a type outside the explicit registry.
type UnsupportedTypeError struct {
	OID      uint32
	Column   string
	TypeName string
}

func (e *UnsupportedTypeError) Error() string {
	if e == nil {
		return "unsupported PostgreSQL type"
	}
	if e.Column != "" {
		if e.TypeName != "" {
			return fmt.Sprintf("unsupported PostgreSQL type %q (OID %d) in column %q", e.TypeName, e.OID, e.Column)
		}
		return fmt.Sprintf("unsupported PostgreSQL type OID %d in column %q", e.OID, e.Column)
	}
	if e.TypeName != "" {
		return fmt.Sprintf("unsupported PostgreSQL type %q (OID %d)", e.TypeName, e.OID)
	}
	return fmt.Sprintf("unsupported PostgreSQL type OID %d", e.OID)
}

// typeCoercer is deliberately small: the database driver performs wire
// decoding, while this registry makes the JSON representation explicit.
type typeCoercer func(value any) (any, error)

var builtinTypeCoercers = map[uint32]typeCoercer{
	pgtype.ByteaOID:       coerceBytea,
	pgtype.TextOID:        coerceString,
	pgtype.VarcharOID:     coerceString,
	pgtype.BPCharOID:      coerceString,
	pgtype.QCharOID:       coerceString,
	pgtype.UUIDOID:        coerceUUID,
	pgtype.TimestamptzOID: coerceTimestamp,
	pgtype.TimestampOID:   coerceTimestampWithoutZone,
	pgtype.JSONBOID:       coerceJSON,
	pgtype.JSONOID:        coerceJSON,
	pgtype.BoolOID:        coerceBool,
	pgtype.Int2OID:        coerceNumber,
	pgtype.Int4OID:        coerceNumber,
	pgtype.Int8OID:        coerceNumber,
	pgtype.NumericOID:     coerceNumber,
	pgtype.Float4OID:      coerceNumber,
	pgtype.Float8OID:      coerceNumber,
	pgtype.InetOID:        coerceString,
	pgtype.CIDROID:        coerceString,
	pgtype.MacaddrOID:     coerceString,
}

func builtinTypeCoercer(oid uint32) (typeCoercer, bool) {
	coercer, ok := builtinTypeCoercers[oid]
	return coercer, ok
}

func typeName(typeInfo *pgtype.Type) string {
	if typeInfo == nil {
		return ""
	}
	return typeInfo.Name
}
