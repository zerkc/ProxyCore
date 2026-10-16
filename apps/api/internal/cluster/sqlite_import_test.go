package cluster

import (
	"bytes"
	"context"
	"database/sql"
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

func (f *sqliteKeyFixture) importKey(id uuid.UUID, key []byte) error {
	return f.writer.Write(context.Background(), func(v sqlitedb.WriteValue) error {
		return f.store.ImportInWrite(v, id, key)
	})
}

func (f *sqliteKeyFixture) retiredAt(t *testing.T, id uuid.UUID) bool {
	t.Helper()
	var retired sql.NullString
	if err := f.db.QueryRow(`select retired_at from cluster_keys where id = ?`, id.String()).Scan(&retired); err != nil {
		t.Fatal(err)
	}
	return retired.Valid
}

func TestImportInWriteReactivatesRetiredRowWithSameMaterial(t *testing.T) {
	f := newSQLiteKeyFixture(t)
	id := uuid.New()
	retired := stampText(t, 1_700_000_100)
	f.seedRow(t, id, seedKey(0x42), ClusterKeyPurpose, ClusterKeyWrappingVersion, stampText(t, 1_700_000_000), &retired)
	if err := f.importKey(id, seedKey(0x42)); err != nil {
		t.Fatalf("ImportInWrite: %v", err)
	}
	if f.retiredAt(t, id) {
		t.Fatal("row is still retired")
	}
	if got := f.count(t, ""); got != 1 {
		t.Fatalf("rows = %d, want 1", got)
	}
	material, err := f.store.LoadActive(context.Background(), id)
	if err != nil {
		t.Fatalf("LoadActive after reactivation: %v", err)
	}
	defer material.Destroy()
	if !bytes.Equal(material.CopyBytes(), seedKey(0x42)) {
		t.Fatal("reactivated KEK does not round-trip")
	}
}

func TestImportInWriteDeniesWithSpecificCause(t *testing.T) {
	cases := []struct {
		name    string
		seed    func(t *testing.T, f *sqliteKeyFixture, id uuid.UUID)
		want    error
		retired bool
	}{
		{"active different material", func(t *testing.T, f *sqliteKeyFixture, id uuid.UUID) {
			f.seedRow(t, id, seedKey(0x43), ClusterKeyPurpose, ClusterKeyWrappingVersion, stampText(t, 1), nil)
		}, ErrClusterKeyMaterialMismatch, false},
		{"active unwrap failure", func(t *testing.T, f *sqliteKeyFixture, id uuid.UUID) {
			f.seedRow(t, id, seedKey(0x42), "other-purpose", ClusterKeyWrappingVersion, stampText(t, 1), nil)
		}, ErrClusterKeyUnwrapFailed, false},
		{"insert failure", func(t *testing.T, f *sqliteKeyFixture, id uuid.UUID) {
			if _, err := f.db.Exec(`create trigger block_cluster_keys before insert on cluster_keys
				begin select raise(abort, 'blocked'); end`); err != nil {
				t.Fatal(err)
			}
		}, ErrClusterKeyInsertFailed, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newSQLiteKeyFixture(t)
			id := uuid.New()
			tc.seed(t, f, id)
			before := f.count(t, "")
			err := f.importKey(id, seedKey(0x42))
			if !errors.Is(err, tc.want) || !errors.Is(err, ErrClusterKeyDenied) {
				t.Fatalf("err = %v, want %v and ErrClusterKeyDenied", err, tc.want)
			}
			for _, other := range []error{ErrClusterKeyMaterialMismatch, ErrClusterKeyUnwrapFailed, ErrClusterKeyInsertFailed} {
				if other != tc.want && errors.Is(err, other) {
					t.Fatalf("err also matches %v", other)
				}
			}
			if got := f.count(t, ""); got != before {
				t.Fatalf("rows = %d, want %d", got, before)
			}
			if before == 1 && f.retiredAt(t, id) != tc.retired {
				t.Fatalf("retired state changed: now retired=%v", !tc.retired)
			}
		})
	}
}

func TestImportInWriteInvalidInputStaysPlainDenied(t *testing.T) {
	f := newSQLiteKeyFixture(t)
	err := f.importKey(uuid.Nil, seedKey(0x42))
	for _, specific := range []error{ErrClusterKeyMaterialMismatch, ErrClusterKeyUnwrapFailed, ErrClusterKeyInsertFailed} {
		if errors.Is(err, specific) {
			t.Fatalf("invalid input matched %v", specific)
		}
	}
	if !errors.Is(err, ErrClusterKeyDenied) {
		t.Fatalf("err = %v", err)
	}
}

func TestImportInWriteReplacesRetiredRowThatCannotBeTrusted(t *testing.T) {
	retiredStamp := func(t *testing.T) *string { s := stampText(t, 1_700_000_100); return &s }
	cases := map[string]struct {
		purpose string
		stored  []byte
	}{
		"different material": {ClusterKeyPurpose, seedKey(0x43)},
		"unwrap failure":     {"other-purpose", seedKey(0x42)},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newSQLiteKeyFixture(t)
			id := uuid.New()
			f.seedRow(t, id, tc.stored, tc.purpose, ClusterKeyWrappingVersion, stampText(t, 1), retiredStamp(t))
			if err := f.importKey(id, seedKey(0x42)); err != nil {
				t.Fatalf("ImportInWrite: %v", err)
			}
			if got := f.count(t, ""); got != 1 {
				t.Fatalf("rows = %d, want 1", got)
			}
			if f.retiredAt(t, id) {
				t.Fatal("replacement row is retired")
			}
			material, err := f.store.LoadActive(context.Background(), id)
			if err != nil {
				t.Fatalf("LoadActive: %v", err)
			}
			defer material.Destroy()
			if !bytes.Equal(material.CopyBytes(), seedKey(0x42)) {
				t.Fatal("replacement KEK does not match the incoming key")
			}
		})
	}
}

func TestImportInWriteReplacementIsAtomic(t *testing.T) {
	f := newSQLiteKeyFixture(t)
	id := uuid.New()
	retired := stampText(t, 1_700_000_100)
	f.seedRow(t, id, seedKey(0x43), ClusterKeyPurpose, ClusterKeyWrappingVersion, stampText(t, 1), &retired)
	if _, err := f.db.Exec(`create trigger block_cluster_keys before insert on cluster_keys
		begin select raise(abort, 'blocked'); end`); err != nil {
		t.Fatal(err)
	}
	if err := f.importKey(id, seedKey(0x42)); !errors.Is(err, ErrClusterKeyInsertFailed) {
		t.Fatalf("err = %v, want ErrClusterKeyInsertFailed", err)
	}
	if got := f.count(t, "where retired_at is not null"); got != 1 {
		t.Fatalf("retired rows = %d, want 1 (delete must roll back)", got)
	}
	var wrapped string
	if err := f.db.QueryRow(`select wrapped_kek from cluster_keys where id = ?`, id.String()).Scan(&wrapped); err != nil {
		t.Fatal(err)
	}
}
