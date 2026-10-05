package sqlitedb

import (
	"fmt"
	"regexp"
	"strconv"
)

// Timestamp is a tagged SQL value, not a calendar or PostgreSQL date parser.
// Finite seconds are floor Unix seconds; nanos are the nonnegative remainder.
// Persist encoded keys as TEXT COLLATE BINARY. SQL NULL ordering is separate.
type Timestamp interface{ timestampTag() }

type NullTimestamp struct{}
type NegativeInfinity struct{}
type PositiveInfinity struct{}
type FiniteTimestamp struct {
	Seconds int64
	Nanos   int32
}

func (NullTimestamp) timestampTag()    {}
func (NegativeInfinity) timestampTag() {}
func (PositiveInfinity) timestampTag() {}
func (FiniteTimestamp) timestampTag()  {}

const timestampBias uint64 = 1 << 63

var finiteTimestampKey = regexp.MustCompile(`^1([0-9]{20})\.([0-9]{9})$`)

// EncodeTimestamp returns nil only for the explicit NullTimestamp variant.
func EncodeTimestamp(value Timestamp) (*string, error) {
	var key string
	switch v := value.(type) {
	case NullTimestamp:
		return nil, nil
	case NegativeInfinity:
		key = "0"
	case PositiveInfinity:
		key = "2"
	case FiniteTimestamp:
		if v.Nanos < 0 || v.Nanos > 999999999 {
			return nil, fmt.Errorf("timestamp nanos outside 0..999999999")
		}
		key = fmt.Sprintf("1%020d.%09d", uint64(v.Seconds)^timestampBias, v.Nanos)
	default:
		return nil, fmt.Errorf("invalid timestamp variant %T", value)
	}
	return &key, nil
}

// DecodeTimestamp accepts only SQL NULL, the infinity tags, or canonical keys.
func DecodeTimestamp(key *string) (Timestamp, error) {
	if key == nil {
		return NullTimestamp{}, nil
	}
	switch *key {
	case "0":
		return NegativeInfinity{}, nil
	case "2":
		return PositiveInfinity{}, nil
	}
	parts := finiteTimestampKey.FindStringSubmatch(*key)
	if len(*key) != 31 || parts == nil {
		return nil, fmt.Errorf("noncanonical timestamp key")
	}
	biased, err := strconv.ParseUint(parts[1], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("timestamp biased seconds: %w", err)
	}
	nanos, err := strconv.ParseUint(parts[2], 10, 32)
	if err != nil {
		return nil, err
	}
	return FiniteTimestamp{Seconds: int64(biased ^ timestampBias), Nanos: int32(nanos)}, nil
}
