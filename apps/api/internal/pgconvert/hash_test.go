package pgconvert

import "testing"

func TestRowHashDistinguishesTypesAndNulls(t *testing.T) {
	spec := TableSpec{Name: "t", Columns: []ColumnSpec{{Name: "a", Kind: KindText, Nullable: true}}}
	values := []any{nil, "", []byte{}, "1", int64(1), []byte("1"), "x\x00y"}
	seen := map[[32]byte]int{}
	for i, v := range values {
		h := RowHash(spec, []any{v})
		if j, dup := seen[h]; dup {
			t.Errorf("values %d (%#v) and %d (%#v) collide", j, values[j], i, v)
		}
		seen[h] = i
	}
	other := TableSpec{Name: "u", Columns: spec.Columns}
	if RowHash(spec, []any{"a"}) == RowHash(other, []any{"a"}) {
		t.Error("hash ignores the table name")
	}
}

func TestDigestIsOrderIndependentAndCountSensitive(t *testing.T) {
	spec := TableSpec{Name: "t", Columns: []ColumnSpec{{Name: "a", Kind: KindInt}}}
	hashes := [][32]byte{RowHash(spec, []any{int64(1)}), RowHash(spec, []any{int64(2)}), RowHash(spec, []any{int64(3)})}
	forward, backward := NewDigest(), NewDigest()
	for _, h := range hashes {
		forward.Add(h)
	}
	for i := len(hashes) - 1; i >= 0; i-- {
		backward.Add(hashes[i])
	}
	if forward.Hex() != backward.Hex() || forward.Rows() != 3 {
		t.Errorf("order dependent: %s vs %s (%d rows)", forward.Hex(), backward.Hex(), forward.Rows())
	}
	short := NewDigest()
	short.Add(hashes[0])
	short.Add(hashes[1])
	if short.Hex() == forward.Hex() {
		t.Error("digest ignores missing rows")
	}
	if NewDigest().Hex() == short.Hex() {
		t.Error("empty digest equals non-empty digest")
	}
}
