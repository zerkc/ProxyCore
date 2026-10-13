package dbexport

import "fmt"

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
