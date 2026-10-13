package pgconvert

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strings"
)

// RowHash is the canonical per-row hash shared by the source and destination
// sides. It covers the table name, every column name in spec order, and each
// destination-ready value with an explicit type tag, so NULL, empty text, empty
// BLOB, text "1" and integer 1 are all distinct.
func RowHash(table TableSpec, row []any) [32]byte {
	h := sha256.New()
	writeBytes(h, 'T', []byte(table.Name))
	for i, c := range table.Columns {
		writeBytes(h, 'C', []byte(c.Name))
		switch v := row[i].(type) {
		case nil:
			h.Write([]byte{0})
		case int64:
			var b [9]byte
			b[0] = 1
			binary.BigEndian.PutUint64(b[1:], uint64(v))
			h.Write(b[:])
		case string:
			writeBytes(h, 2, []byte(v))
		case []byte:
			writeBytes(h, 3, v)
		default:
			panic(fmt.Sprintf("pgconvert: unsupported value type %T in %s.%s", v, table.Name, c.Name))
		}
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

type byteWriter interface{ Write([]byte) (int, error) }

func writeBytes(w byteWriter, tag byte, data []byte) {
	var head [9]byte
	head[0] = tag
	binary.BigEndian.PutUint64(head[1:], uint64(len(data)))
	w.Write(head[:])
	w.Write(data)
}

// Digest folds row hashes into an order-independent table hash: the sum of the
// row hashes modulo 2^256, bound to the row count.
type Digest struct {
	sum  [32]byte
	rows int64
}

func NewDigest() *Digest { return &Digest{} }

func (d *Digest) Add(rowHash [32]byte) {
	carry := 0
	for i := 31; i >= 0; i-- {
		total := int(d.sum[i]) + int(rowHash[i]) + carry
		d.sum[i] = byte(total)
		carry = total >> 8
	}
	d.rows++
}

func (d *Digest) Rows() int64 { return d.rows }

func (d *Digest) Hex() string {
	h := sha256.New()
	var count [8]byte
	binary.BigEndian.PutUint64(count[:], uint64(d.rows))
	h.Write(count[:])
	h.Write(d.sum[:])
	return hex.EncodeToString(h.Sum(nil))
}

// HashPGTable returns the row count and canonical hash of a source table.
func HashPGTable(ctx context.Context, src *Source, plan *Plan, table string) (int64, string, error) {
	spec, err := tableSpec(table)
	if err != nil {
		return 0, "", err
	}
	digest := NewDigest()
	err = src.ReadTable(ctx, plan, table, func(row []any) error {
		digest.Add(RowHash(spec, row))
		return nil
	})
	return digest.Rows(), digest.Hex(), err
}

// Querier is satisfied by *sql.DB, *sql.Conn and *sql.Tx.
type Querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// HashSQLiteTable returns the row count and canonical hash of a destination
// table, reading back the stored values (not the inserted ones).
func HashSQLiteTable(ctx context.Context, q Querier, spec TableSpec) (int64, string, error) {
	names := make([]string, len(spec.Columns))
	for i, c := range spec.Columns {
		names[i] = quoteIdent(c.Name)
	}
	rows, err := q.QueryContext(ctx, `select `+strings.Join(names, ", ")+` from `+quoteIdent(spec.Name))
	if err != nil {
		return 0, "", fmt.Errorf("pgconvert: read back %s: %w", spec.Name, err)
	}
	defer rows.Close()
	digest := NewDigest()
	for rows.Next() {
		values := make([]any, len(spec.Columns))
		targets := make([]any, len(values))
		for i := range values {
			targets[i] = &values[i]
		}
		if err := rows.Scan(targets...); err != nil {
			return 0, "", fmt.Errorf("pgconvert: scan %s: %w", spec.Name, err)
		}
		for i, v := range values {
			switch x := v.(type) {
			case nil, int64, string, []byte:
			default:
				return 0, "", &DataError{Table: spec.Name, Column: spec.Columns[i].Name, Detail: fmt.Sprintf("unexpected SQLite value type %T", x)}
			}
		}
		digest.Add(RowHash(spec, values))
	}
	return digest.Rows(), digest.Hex(), rows.Err()
}
