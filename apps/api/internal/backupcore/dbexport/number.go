package dbexport

import "strconv"

// BigintAboveFloat64Safe returns a JSON-safe number through 2^53 and a
// decimal string above that boundary. This keeps exported JSON values as
// lossless JSON Numbers within the IEEE-754 integer-safe range and as decimal
// digit strings for larger PostgreSQL integers.
func BigintAboveFloat64Safe(value int64) any {
	const maxJSONSafeInteger = int64(1 << 53)
	if value >= -maxJSONSafeInteger && value <= maxJSONSafeInteger {
		return float64(value)
	}
	return strconv.FormatInt(value, 10)
}
