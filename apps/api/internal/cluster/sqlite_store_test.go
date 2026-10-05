package cluster

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/secrets"
	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

// These tests never print key, ciphertext or plaintext bytes: every assertion
// on key material compares with bytes.Equal and reports booleans or lengths.

var sqliteTestMaster = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x19}, 32))

type sqliteKeyFixture struct {
	db     *sql.DB
	writer *sqlitedb.Writer
	store  *SQLiteKeyStore
	tick   int64
}

func newSQLiteKeyFixture(t *testing.T) *sqliteKeyFixture {
	t.Helper()
	db, err := sqlitedb.Open(filepath.Join(t.TempDir(), "cluster.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := sqlitedb.Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	f := &sqliteKeyFixture{db: db, tick: 1_700_000_000}
	var mu sync.Mutex
	f.writer = sqlitedb.NewWriter(db, func() sqlitedb.FiniteTimestamp {
		mu.Lock()
		defer mu.Unlock()
		f.tick++
		return sqlitedb.FiniteTimestamp{Seconds: f.tick, Nanos: 7}
	})
	f.store = NewSQLiteKeyStore(db, f.writer, sqliteTestMaster)
	return f
}

func (f *sqliteKeyFixture) count(t *testing.T, where string) int {
	t.Helper()
	var n int
	if err := f.db.QueryRow(`select count(*) from cluster_keys ` + where).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func stampText(t *testing.T, seconds int64) string {
	t.Helper()
	s, err := sqlitedb.EncodeTimestamp(sqlitedb.FiniteTimestamp{Seconds: seconds, Nanos: 1})
	if err != nil {
		t.Fatal(err)
	}
	return *s
}

// seedRow inserts a raw row whose wrapped KEK wraps key under the test master.
func (f *sqliteKeyFixture) seedRow(t *testing.T, id uuid.UUID, key []byte, purpose string, version int, createdAt string, retiredAt *string) {
	t.Helper()
	wrapped, err := secrets.EncryptSecret(string(key), sqliteTestMaster)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`insert into cluster_keys (id, purpose, wrapped_kek, wrapping_key_version, created_at, retired_at)
		values (?, ?, ?, ?, ?, ?)`, id.String(), purpose, wrapped, version, createdAt, retiredAt); err != nil {
		t.Fatal(err)
	}
}

func seedKey(b byte) []byte { return bytes.Repeat([]byte{b}, KEKLength) }

func mustLoadOrCreate(t *testing.T, s *SQLiteKeyStore, id *uuid.UUID) (uuid.UUID, []byte) {
	t.Helper()
	m, err := s.LoadOrCreate(context.Background(), id)
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	defer m.Destroy()
	return m.ID, m.CopyBytes()
}

func TestSQLiteKeyStoreCreatesReloadsAndWrapsAtRest(t *testing.T) {
	f := newSQLiteKeyFixture(t)
	id, key := mustLoadOrCreate(t, f.store, nil)
	if id == uuid.Nil || len(key) != KEKLength {
		t.Fatalf("created material: nil id=%v len=%d", id == uuid.Nil, len(key))
	}
	if f.count(t, "") != 1 {
		t.Fatalf("rows = %d, want 1", f.count(t, ""))
	}
	var storedID, purpose, wrapped, created string
	var version int
	var retired sql.NullString
	if err := f.db.QueryRow(`select id, purpose, wrapped_kek, wrapping_key_version, created_at, retired_at from cluster_keys`).
		Scan(&storedID, &purpose, &wrapped, &version, &created, &retired); err != nil {
		t.Fatal(err)
	}
	if storedID != id.String() || purpose != ClusterKeyPurpose || version != ClusterKeyWrappingVersion || retired.Valid {
		t.Fatalf("row shape: id match=%v purpose=%q version=%d retired=%v", storedID == id.String(), purpose, version, retired.Valid)
	}
	if !strings.HasPrefix(wrapped, "v1:") {
		t.Fatal("wrapped KEK is not a v1 master-key envelope")
	}
	for name, leak := range map[string]string{
		"raw": string(key), "hex": hex.EncodeToString(key),
		"std": base64.StdEncoding.EncodeToString(key), "url": base64.RawURLEncoding.EncodeToString(key),
	} {
		if strings.Contains(wrapped, leak) {
			t.Fatalf("wrapped KEK contains the %s form of the key", name)
		}
	}
	plain, err := secrets.DecryptSecret(wrapped, sqliteTestMaster)
	if err != nil || !bytes.Equal([]byte(plain), key) {
		t.Fatalf("envelope does not unwrap to the returned key: err=%v", err)
	}
	decoded, err := sqlitedb.DecodeTimestamp(&created)
	if fin, ok := decoded.(sqlitedb.FiniteTimestamp); err != nil || !ok || fin.Seconds != f.tick || fin.Nanos != 7 {
		t.Fatalf("created_at %q is not the writer clock stamp (err=%v)", created, err)
	}

	restarted := NewSQLiteKeyStore(f.db, f.writer, sqliteTestMaster)
	m, err := restarted.LoadActive(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if m.ID != id || !bytes.Equal(m.CopyBytes(), key) {
		t.Fatal("restarted store loaded a different KEK")
	}
	m.Destroy()

	for name, arg := range map[string]*uuid.UUID{"nil": nil, "active": &id} {
		gotID, gotKey := mustLoadOrCreate(t, f.store, arg)
		if gotID != id || !bytes.Equal(gotKey, key) {
			t.Fatalf("LoadOrCreate(%s) did not return the existing active KEK", name)
		}
	}
	if f.count(t, "") != 1 {
		t.Fatal("reloading created another row")
	}
}

func TestSQLiteKeyStoreDeniesEveryFailureWithoutDetail(t *testing.T) {
	good := uuid.MustParse("aaaaaaaa-0000-4000-8000-000000000001")
	retiredAt := func(t *testing.T) *string { s := stampText(t, 1_600_000_000); return &s }
	cases := []struct {
		name  string
		setup func(t *testing.T, f *sqliteKeyFixture) uuid.UUID
		store func(f *sqliteKeyFixture) *SQLiteKeyStore
	}{
		{name: "wrong master key", setup: func(t *testing.T, f *sqliteKeyFixture) uuid.UUID {
			f.seedRow(t, good, seedKey(1), ClusterKeyPurpose, 1, stampText(t, 1), nil)
			return good
		}, store: func(f *sqliteKeyFixture) *SQLiteKeyStore {
			return NewSQLiteKeyStore(f.db, f.writer, base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x39}, 32)))
		}},
		{name: "tampered ciphertext", setup: func(t *testing.T, f *sqliteKeyFixture) uuid.UUID {
			f.seedRow(t, good, seedKey(1), ClusterKeyPurpose, 1, stampText(t, 1), nil)
			if _, err := f.db.Exec(`update cluster_keys set wrapped_kek = 'tampered'`); err != nil {
				t.Fatal(err)
			}
			return good
		}},
		{name: "wrong purpose", setup: func(t *testing.T, f *sqliteKeyFixture) uuid.UUID {
			f.seedRow(t, good, seedKey(1), "other-purpose", 1, stampText(t, 1), nil)
			return good
		}},
		{name: "wrong wrapping version", setup: func(t *testing.T, f *sqliteKeyFixture) uuid.UUID {
			f.seedRow(t, good, seedKey(1), ClusterKeyPurpose, 2, stampText(t, 1), nil)
			return good
		}},
		{name: "retired key", setup: func(t *testing.T, f *sqliteKeyFixture) uuid.UUID {
			f.seedRow(t, good, seedKey(1), ClusterKeyPurpose, 1, stampText(t, 1), retiredAt(t))
			return good
		}},
		{name: "unknown id", setup: func(t *testing.T, f *sqliteKeyFixture) uuid.UUID { return uuid.New() }},
		{name: "nil uuid", setup: func(t *testing.T, f *sqliteKeyFixture) uuid.UUID { return uuid.Nil }},
		{name: "wrong key length", setup: func(t *testing.T, f *sqliteKeyFixture) uuid.UUID {
			f.seedRow(t, good, seedKey(1)[:16], ClusterKeyPurpose, 1, stampText(t, 1), nil)
			return good
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newSQLiteKeyFixture(t)
			id := tc.setup(t, f)
			store := f.store
			if tc.store != nil {
				store = tc.store(f)
			}
			check := func(op string, m KeyMaterial, err error) {
				t.Helper()
				if err != ErrClusterKeyDenied || err.Error() != "cluster key denied" {
					t.Fatalf("%s error is not the bare ErrClusterKeyDenied: %v", op, err)
				}
				if got := m.CopyBytes(); got != nil || m.ID != uuid.Nil {
					t.Fatalf("%s returned material on denial", op)
				}
			}
			m, err := store.LoadActive(context.Background(), id)
			check("LoadActive", m, err)
			m, err = store.LoadOrCreate(context.Background(), &id)
			check("LoadOrCreate(id)", m, err)
		})
	}
}

func TestSQLiteKeyStoreNilPathDeniesUnreadableActiveRowWithoutCreating(t *testing.T) {
	good := uuid.MustParse("aaaaaaaa-0000-4000-8000-000000000001")
	for name, mutate := range map[string]func(t *testing.T, f *sqliteKeyFixture){
		"tamper":  func(t *testing.T, f *sqliteKeyFixture) { f.db.Exec(`update cluster_keys set wrapped_kek = 'tampered'`) },
		"purpose": func(t *testing.T, f *sqliteKeyFixture) { f.db.Exec(`update cluster_keys set purpose = 'x'`) },
		"version": func(t *testing.T, f *sqliteKeyFixture) { f.db.Exec(`update cluster_keys set wrapping_key_version = 2`) },
		"master": func(t *testing.T, f *sqliteKeyFixture) {
			f.store = NewSQLiteKeyStore(f.db, f.writer, base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x39}, 32)))
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newSQLiteKeyFixture(t)
			f.seedRow(t, good, seedKey(1), ClusterKeyPurpose, 1, stampText(t, 1), nil)
			mutate(t, f)
			if _, err := f.store.LoadOrCreate(context.Background(), nil); err != ErrClusterKeyDenied {
				t.Fatalf("error = %v, want bare ErrClusterKeyDenied", err)
			}
			if f.count(t, "") != 1 {
				t.Fatal("a second KEK was created next to an unreadable active one")
			}
		})
	}
}

func TestSQLiteKeyStoreSelectsOldestUnretiredAndBreaksTiesByID(t *testing.T) {
	older := uuid.MustParse("ffffffff-0000-4000-8000-000000000001") // largest id, oldest created_at
	newer := uuid.MustParse("00000000-0000-4000-8000-000000000002") // smallest id, newer created_at
	retired := uuid.MustParse("00000000-0000-4000-8000-000000000003")
	t.Run("oldest created_at wins over id order and retired rows are ignored", func(t *testing.T) {
		f := newSQLiteKeyFixture(t)
		r := stampText(t, 50)
		f.seedRow(t, retired, seedKey(9), ClusterKeyPurpose, 1, stampText(t, 1), &r)
		f.seedRow(t, newer, seedKey(2), ClusterKeyPurpose, 1, stampText(t, 200), nil)
		f.seedRow(t, older, seedKey(1), ClusterKeyPurpose, 1, stampText(t, 100), nil)
		id, key := mustLoadOrCreate(t, f.store, nil)
		if id != older || !bytes.Equal(key, seedKey(1)) {
			t.Fatalf("selected id older=%v newer=%v retired=%v", id == older, id == newer, id == retired)
		}
		if f.count(t, "") != 3 {
			t.Fatal("selection created a row")
		}
	})
	t.Run("equal created_at breaks by smallest id", func(t *testing.T) {
		f := newSQLiteKeyFixture(t)
		same := stampText(t, 100)
		f.seedRow(t, older, seedKey(1), ClusterKeyPurpose, 1, same, nil)
		f.seedRow(t, newer, seedKey(2), ClusterKeyPurpose, 1, same, nil)
		id, key := mustLoadOrCreate(t, f.store, nil)
		if id != newer || !bytes.Equal(key, seedKey(2)) {
			t.Fatalf("tie selected the wrong row: smallest id=%v", id == newer)
		}
	})
	t.Run("all retired creates a fresh active key", func(t *testing.T) {
		f := newSQLiteKeyFixture(t)
		r := stampText(t, 50)
		f.seedRow(t, retired, seedKey(9), ClusterKeyPurpose, 1, stampText(t, 1), &r)
		id, key := mustLoadOrCreate(t, f.store, nil)
		if id == retired || bytes.Equal(key, seedKey(9)) || f.count(t, "where retired_at is null") != 1 {
			t.Fatal("retired key was reused or no fresh active key exists")
		}
	})
}

func TestSQLiteKeyStoreConcurrentCreateConvergesOnOneKEK(t *testing.T) {
	f := newSQLiteKeyFixture(t)
	type result struct {
		id  uuid.UUID
		key []byte
		err error
	}
	results := make(chan result, 16)
	var wait sync.WaitGroup
	for i := 0; i < cap(results); i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			m, err := f.store.LoadOrCreate(context.Background(), nil)
			results <- result{id: m.ID, key: m.CopyBytes(), err: err}
			m.Destroy()
		}()
	}
	wait.Wait()
	close(results)
	var first result
	for r := range results {
		if r.err != nil {
			t.Fatalf("concurrent LoadOrCreate: %v", r.err)
		}
		if first.id == uuid.Nil {
			first = r
		} else if r.id != first.id || !bytes.Equal(r.key, first.key) {
			t.Fatal("concurrent creators diverged")
		}
	}
	if f.count(t, "") != 1 || f.count(t, "where retired_at is null") != 1 {
		t.Fatalf("rows = %d, want exactly one active KEK", f.count(t, ""))
	}
}

// The first writer holds the Write open (and creates the KEK in it) while three
// callers read "absent", generate candidates and queue for the Write. The pool
// is capped at four connections, so a stable InUse == 4 proves all three have passed
// the pre-Write read and are queued. Without the re-check inside the Write they would each insert.
func TestSQLiteKeyStoreQueuedCreatorsRecheckInsideWrite(t *testing.T) {
	f := newSQLiteKeyFixture(t)
	const waiters = 3
	var holderID uuid.UUID
	var holderKey []byte
	holderDone := make(chan error, 1)
	inWrite := make(chan struct{})
	go func() {
		holderDone <- f.writer.Write(context.Background(), func(v sqlitedb.WriteValue) error {
			m, err := f.store.LoadOrCreateInWrite(v, nil, nil)
			if err != nil {
				return err
			}
			holderID, holderKey = m.ID, m.CopyBytes()
			m.Destroy()
			close(inWrite)
			// A pre-Write read also holds a connection briefly, so require the
			// full count to stay stable: only queued Writes hold one for good.
			stable := time.Now()
			for time.Since(stable) < 50*time.Millisecond {
				if v.Context().Err() != nil {
					return v.Context().Err()
				}
				if f.db.Stats().InUse != waiters+1 {
					stable = time.Now()
				}
				time.Sleep(time.Millisecond)
			}
			return nil
		})
	}()
	<-inWrite
	var wait sync.WaitGroup
	type result struct {
		id  uuid.UUID
		key []byte
		err error
	}
	results := make(chan result, waiters)
	for i := 0; i < waiters; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			m, err := f.store.LoadOrCreate(context.Background(), nil)
			results <- result{id: m.ID, key: m.CopyBytes(), err: err}
			m.Destroy()
		}()
	}
	wait.Wait()
	close(results)
	if err := <-holderDone; err != nil {
		t.Fatal(err)
	}
	for r := range results {
		if r.err != nil || r.id != holderID || !bytes.Equal(r.key, holderKey) {
			t.Fatalf("queued creator did not converge on the winner: err=%v same id=%v", r.err, r.id == holderID)
		}
	}
	if f.count(t, "") != 1 {
		t.Fatalf("rows = %d, want 1", f.count(t, ""))
	}
}

func TestSQLiteKeyStoreInWriteIsAtomicWithTheCallersWrite(t *testing.T) {
	t.Run("rollback removes the KEK row", func(t *testing.T) {
		f := newSQLiteKeyFixture(t)
		sentinel := errors.New("caller failed")
		err := f.writer.Write(context.Background(), func(v sqlitedb.WriteValue) error {
			m, err := f.store.LoadOrCreateInWrite(v, nil, nil)
			if err != nil {
				return err
			}
			m.Destroy()
			return sentinel
		})
		if !errors.Is(err, sentinel) || f.count(t, "") != 0 {
			t.Fatalf("err=%v rows=%d, want caller error and no rows", err, f.count(t, ""))
		}
	})
	t.Run("commit persists and the caller sees its own uncommitted KEK", func(t *testing.T) {
		f := newSQLiteKeyFixture(t)
		var created, again uuid.UUID
		err := f.writer.Write(context.Background(), func(v sqlitedb.WriteValue) error {
			m, err := f.store.LoadOrCreateInWrite(v, nil, nil)
			if err != nil {
				return err
			}
			created = m.ID
			m.Destroy()
			m, err = f.store.LoadOrCreateInWrite(v, &created, nil)
			if err != nil {
				return err
			}
			again = m.ID
			m.Destroy()
			return nil
		})
		if err != nil || created == uuid.Nil || again != created || f.count(t, "") != 1 {
			t.Fatalf("err=%v same=%v rows=%d", err, again == created, f.count(t, ""))
		}
	})
	t.Run("candidate is consumed as the inserted key", func(t *testing.T) {
		f := newSQLiteKeyFixture(t)
		cand, err := f.store.NewKeyCandidate()
		if err != nil {
			t.Fatal(err)
		}
		defer cand.Destroy()
		var m KeyMaterial
		if err := f.writer.Write(context.Background(), func(v sqlitedb.WriteValue) error {
			var err error
			m, err = f.store.LoadOrCreateInWrite(v, nil, cand)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		defer m.Destroy()
		if m.ID != cand.ID() {
			t.Fatal("inserted id is not the candidate id")
		}
		loaded, err := f.store.LoadActive(context.Background(), cand.ID())
		if err != nil || !bytes.Equal(loaded.CopyBytes(), m.CopyBytes()) {
			t.Fatalf("candidate key was not the stored key: %v", err)
		}
		loaded.Destroy()
	})
	t.Run("existing active key wins over a candidate", func(t *testing.T) {
		f := newSQLiteKeyFixture(t)
		id, key := mustLoadOrCreate(t, f.store, nil)
		cand, err := f.store.NewKeyCandidate()
		if err != nil {
			t.Fatal(err)
		}
		defer cand.Destroy()
		var m KeyMaterial
		if err := f.writer.Write(context.Background(), func(v sqlitedb.WriteValue) error {
			var err error
			m, err = f.store.LoadOrCreateInWrite(v, nil, cand)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		defer m.Destroy()
		if m.ID != id || !bytes.Equal(m.CopyBytes(), key) || f.count(t, "") != 1 {
			t.Fatal("candidate displaced the existing active key")
		}
	})
	t.Run("denials inside a write do not poison the caller's write", func(t *testing.T) {
		f := newSQLiteKeyFixture(t)
		missing := uuid.New()
		if err := f.writer.Write(context.Background(), func(v sqlitedb.WriteValue) error {
			if _, err := f.store.LoadOrCreateInWrite(v, &missing, nil); err != ErrClusterKeyDenied {
				return fmt.Errorf("want bare denial, got %v", err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if f.count(t, "") != 0 {
			t.Fatal("an activeID lookup created a row")
		}
	})
}

func TestSQLiteKeyCandidateRedactsAndDestroys(t *testing.T) {
	f := newSQLiteKeyFixture(t)
	cand, err := f.store.NewKeyCandidate()
	if err != nil {
		t.Fatal(err)
	}
	if cand.ID() == uuid.Nil || cand.material.CopyBytes() == nil || !strings.HasPrefix(cand.wrapped, "v1:") {
		t.Fatal("candidate is incomplete")
	}
	key := cand.material.CopyBytes()
	if rendered := fmt.Sprintf("%v %+v %s", cand, cand, cand); strings.Contains(rendered, hex.EncodeToString(key)) || strings.Contains(rendered, cand.wrapped) {
		t.Fatal("candidate rendering leaked material")
	}
	cand.Destroy()
	cand.Destroy()
	if cand.material.CopyBytes() != nil || cand.wrapped != "" || cand.ID() != uuid.Nil {
		t.Fatal("destroyed candidate kept material")
	}
	(*KeyCandidate)(nil).Destroy()
	if (*KeyCandidate)(nil).ID() != uuid.Nil {
		t.Fatal("nil candidate id")
	}
	var wipe []byte = key
	zeroClusterTestBytes(wipe)
}

func TestSQLiteKeyStoreUnusableStoresAndContextsAreDenied(t *testing.T) {
	f := newSQLiteKeyFixture(t)
	id, _ := mustLoadOrCreate(t, f.store, nil)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	var nilCtx context.Context
	closedDB, err := sqlitedb.Open(filepath.Join(t.TempDir(), "closed.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	closedDB.Close()
	var typedNil *SQLiteKeyStore
	stores := map[string]*SQLiteKeyStore{
		"typed nil":   typedNil,
		"nil db":      NewSQLiteKeyStore(nil, f.writer, sqliteTestMaster),
		"nil writer":  NewSQLiteKeyStore(f.db, nil, sqliteTestMaster),
		"closed db":   NewSQLiteKeyStore(closedDB, sqlitedb.NewWriter(closedDB, nil), sqliteTestMaster),
		"empty value": {},
	}
	for name, s := range stores {
		// LoadActive is a pure read and needs no writer.
		if m, err := s.LoadActive(context.Background(), id); name != "nil writer" && (err != ErrClusterKeyDenied || m.CopyBytes() != nil) {
			t.Fatalf("%s LoadActive err=%v", name, err)
		}
		if m, err := s.LoadOrCreate(context.Background(), nil); err != ErrClusterKeyDenied || m.CopyBytes() != nil {
			t.Fatalf("%s LoadOrCreate err=%v", name, err)
		}
		if _, err := s.NewKeyCandidate(); name == "typed nil" && err != ErrClusterKeyDenied {
			t.Fatalf("%s NewKeyCandidate err=%v", name, err)
		}
	}
	for name, ctx := range map[string]context.Context{"canceled": canceled, "nil": nilCtx} {
		if _, err := f.store.LoadActive(ctx, id); err != ErrClusterKeyDenied {
			t.Fatalf("%s ctx LoadActive err=%v", name, err)
		}
		if _, err := f.store.LoadOrCreate(ctx, &id); err != ErrClusterKeyDenied {
			t.Fatalf("%s ctx LoadOrCreate(id) err=%v", name, err)
		}
		if _, err := f.store.LoadOrCreate(ctx, nil); err != ErrClusterKeyDenied {
			t.Fatalf("%s ctx LoadOrCreate(nil) err=%v", name, err)
		}
	}
	if _, err := f.store.LoadOrCreateInWrite(sqlitedb.WriteValue{}, nil, nil); err != ErrClusterKeyDenied {
		t.Fatalf("zero WriteValue err=%v", err)
	}
	if f.count(t, "") != 1 {
		t.Fatal("a denied call created a row")
	}
}

func TestSQLiteKeyStoreRendersRedacted(t *testing.T) {
	f := newSQLiteKeyFixture(t)
	for _, rendered := range []string{fmt.Sprint(f.store), fmt.Sprintf("%+v", f.store), fmt.Sprintf("%#v", f.store)} {
		if strings.Contains(rendered, sqliteTestMaster) {
			t.Fatal("store rendering leaked the master key")
		}
	}
}

func TestSQLiteKeyStoreImplementsKeyLoader(t *testing.T) {
	f := newSQLiteKeyFixture(t)
	id, key := mustLoadOrCreate(t, f.store, nil)
	var loader KeyLoader = f.store
	m, err := loader.LoadActive(context.Background(), id)
	if err != nil || !bytes.Equal(m.CopyBytes(), key) {
		t.Fatalf("KeyLoader.LoadActive err=%v", err)
	}
	m.Destroy()
}
