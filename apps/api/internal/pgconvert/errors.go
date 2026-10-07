package pgconvert

import "fmt"

// SchemaErrorKind classifies a fail-closed source schema mismatch.
type SchemaErrorKind string

const (
	ErrUnknownTable     SchemaErrorKind = "unknown_table"
	ErrMissingTable     SchemaErrorKind = "missing_table"
	ErrUnknownColumn    SchemaErrorKind = "unknown_column"
	ErrMissingColumn    SchemaErrorKind = "missing_required_column"
	ErrTypeMismatch     SchemaErrorKind = "type_mismatch"
	ErrUnknownEnumValue SchemaErrorKind = "unknown_enum_value"
)

// SchemaError is returned when the PostgreSQL schema is not one the converter
// knows how to map without guessing.
type SchemaError struct {
	Kind          SchemaErrorKind
	Table, Column string
	Detail        string
}

func (e *SchemaError) Error() string {
	target := e.Table
	if e.Column != "" {
		target += "." + e.Column
	}
	if e.Detail != "" {
		return fmt.Sprintf("pgconvert: %s: %s: %s", e.Kind, target, e.Detail)
	}
	return fmt.Sprintf("pgconvert: %s: %s", e.Kind, target)
}

// DataError reports a source value that cannot be copied faithfully.
type DataError struct {
	Table, Column string
	Detail        string
}

func (e *DataError) Error() string {
	return fmt.Sprintf("pgconvert: invalid data in %s.%s: %s", e.Table, e.Column, e.Detail)
}
