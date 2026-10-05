package sqlitedb

import "errors"

// ParseUUIDLookupText canonicalizes lookup input using PostgreSQL 16/17's UUID
// input grammar, without imposing UUID version or variant constraints. It is
// separate from UUID generation and canonical storage validation.
//
// PostgreSQL accepts optional outer braces and a single optional hyphen after
// each four hex digits, except the final group. No whitespace is discarded.
// Sources: https://www.postgresql.org/docs/16/datatype-uuid.html and
// https://raw.githubusercontent.com/postgres/postgres/REL_17_STABLE/src/backend/utils/adt/uuid.c
// (string_to_uuid; the REL_16_STABLE implementation uses the same grammar).
// Errors intentionally do not promise PostgreSQL wire error format or classes.
func ParseUUIDLookupText(input string) (string, error) {
	invalid := func() (string, error) {
		return "", errors.New("invalid UUID lookup text")
	}
	position := 0
	braced := len(input) > 0 && input[0] == '{'
	if braced {
		position++
	}
	var digits [32]byte
	for pair := 0; pair < 16; pair++ {
		if len(input)-position < 2 {
			return invalid()
		}
		for offset := 0; offset < 2; offset++ {
			ch := input[position+offset]
			switch {
			case ch >= '0' && ch <= '9', ch >= 'a' && ch <= 'f':
			case ch >= 'A' && ch <= 'F':
				ch += 'a' - 'A'
			default:
				return invalid()
			}
			digits[pair*2+offset] = ch
		}
		position += 2
		if pair%2 == 1 && pair < 15 && position < len(input) && input[position] == '-' {
			position++
		}
	}
	if braced {
		if position >= len(input) || input[position] != '}' {
			return invalid()
		}
		position++
	}
	if position != len(input) {
		return invalid()
	}
	var canonical [36]byte
	out := 0
	for i, ch := range digits {
		if i == 8 || i == 12 || i == 16 || i == 20 {
			canonical[out] = '-'
			out++
		}
		canonical[out] = ch
		out++
	}
	return string(canonical[:]), nil
}
