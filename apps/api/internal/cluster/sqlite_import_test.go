package cluster

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

func TestImportInWritePersistsMasterWrappedKEKReadableByLoadActive(t *testing.T) {
	f := newSQLiteKeyFixture(t)
	id := uuid.New()
	key := bytes.Repeat([]byte{0x42}, KEKLength)
	if err := f.writer.Write(context.Background(), func(v sqlitedb.WriteValue) error {
		return f.store.ImportInWrite(v, id, key)
	}); err != nil {
		t.Fatalf("ImportInWrite: %v", err)
	}
	material, err := f.store.LoadActive(context.Background(), id)
	if err != nil {
		t.Fatalf("LoadActive: %v", err)
	}
	defer material.Destroy()
	if !bytes.Equal(material.CopyBytes(), key) {
		t.Fatal("imported KEK does not round-trip")
	}
	var wrapped string
	if err := f.db.QueryRow(`select wrapped_kek from cluster_keys where id = ?`, id.String()).Scan(&wrapped); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains([]byte(wrapped), key) {
		t.Fatal("wrapped_kek contains plaintext key")
	}
}

func TestImportInWriteIsIdempotentForSameKeyAndDeniesDifferentKey(t *testing.T) {
	f := newSQLiteKeyFixture(t)
	id := uuid.New()
	key := bytes.Repeat([]byte{0x42}, KEKLength)
	write := func(k []byte) error {
		return f.writer.Write(context.Background(), func(v sqlitedb.WriteValue) error {
			return f.store.ImportInWrite(v, id, k)
		})
	}
	if err := write(key); err != nil {
		t.Fatal(err)
	}
	if err := write(key); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if got := f.count(t, ""); got != 1 {
		t.Fatalf("rows = %d, want 1", got)
	}
	if err := write(bytes.Repeat([]byte{0x43}, KEKLength)); !errors.Is(err, ErrClusterKeyDenied) {
		t.Fatalf("different key err = %v, want ErrClusterKeyDenied", err)
	}
}

func TestImportInWriteRejectsInvalidInput(t *testing.T) {
	f := newSQLiteKeyFixture(t)
	for name, tc := range map[string]struct {
		id  uuid.UUID
		key []byte
	}{
		"nil id":    {uuid.Nil, bytes.Repeat([]byte{1}, KEKLength)},
		"short key": {uuid.New(), []byte{1, 2, 3}},
	} {
		err := f.writer.Write(context.Background(), func(v sqlitedb.WriteValue) error {
			return f.store.ImportInWrite(v, tc.id, tc.key)
		})
		if !errors.Is(err, ErrClusterKeyDenied) {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
	if got := f.count(t, ""); got != 0 {
		t.Fatalf("rows = %d, want 0", got)
	}
}
