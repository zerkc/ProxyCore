package secrets

import (
	"context"
	"database/sql"
	"encoding/base64"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

// Synthetic fixture keys only; never real key material.
var (
	testKeyA = base64.StdEncoding.EncodeToString([]byte(strings.Repeat("a", 32)))
	testKeyB = base64.StdEncoding.EncodeToString([]byte(strings.Repeat("b", 32)))
	uuidRe   = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

func openSQLiteSecretsDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sqlitedb.Open(filepath.Join(t.TempDir(), "secrets.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := sqlitedb.Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestSQLiteStoreContract(t *testing.T) {
	ctx := context.Background()
	db := openSQLiteSecretsDB(t)
	store := NewSQLiteStore(db, testKeyA)

	t.Run("round trip returns lowercase uuid and plaintext", func(t *testing.T) {
		id, err := store.Put(ctx, "test-purpose", "synthetic-plaintext")
		if err != nil {
			t.Fatal(err)
		}
		if !uuidRe.MatchString(id) {
			t.Fatalf("id %q is not canonical lowercase uuid", id)
		}
		got, err := store.Get(ctx, id)
		if err != nil || got != "synthetic-plaintext" {
			t.Fatalf("Get = (%q, %v)", got, err)
		}
		// PG's uuid column accepts uppercase input; mirror that.
		got, err = store.Get(ctx, strings.ToUpper(id))
		if err != nil || got != "synthetic-plaintext" {
			t.Fatalf("Get uppercase = (%q, %v)", got, err)
		}
	})

	t.Run("unknown id returns empty and nil", func(t *testing.T) {
		got, err := store.Get(ctx, "00000000-0000-4000-8000-000000000000")
		if err != nil || got != "" {
			t.Fatalf("Get = (%q, %v)", got, err)
		}
	})

	t.Run("malformed id errors like PG invalid uuid", func(t *testing.T) {
		for _, id := range []string{"", "not-a-uuid", "0000", strings.Repeat("g", 36), "00000000-0000-4000-8000-00000000000z"} {
			got, err := store.Get(ctx, id)
			if err == nil || got != "" {
				t.Fatalf("Get(%q) = (%q, %v), want error", id, got, err)
			}
		}
	})

	t.Run("ids are unique", func(t *testing.T) {
		seen := map[string]bool{}
		for i := 0; i < 20; i++ {
			id, err := store.Put(ctx, "p", "same")
			if err != nil || seen[id] {
				t.Fatalf("Put = (%q, %v) dup=%v", id, err, seen[id])
			}
			seen[id] = true
		}
	})

	t.Run("ciphertext at rest is opaque and decrypts with the key", func(t *testing.T) {
		const plaintext = "synthetic-at-rest-value"
		id, err := store.Put(ctx, "rest-purpose", plaintext)
		if err != nil {
			t.Fatal(err)
		}
		var purpose, ciphertext, created, updated string
		if err := db.QueryRow(`select purpose,ciphertext,created_at,updated_at from secrets where id=?`, id).
			Scan(&purpose, &ciphertext, &created, &updated); err != nil {
			t.Fatal(err)
		}
		if purpose != "rest-purpose" || created == "" || created != updated {
			t.Fatalf("row purpose=%q created=%q updated=%q", purpose, created, updated)
		}
		if strings.Contains(ciphertext, plaintext) || !strings.HasPrefix(ciphertext, "v1:") {
			t.Fatal("ciphertext at rest is not an encrypted v1 envelope")
		}
		decrypted, err := DecryptSecret(ciphertext, testKeyA)
		if err != nil || decrypted != plaintext {
			t.Fatalf("DecryptSecret mismatch: %v", err)
		}
	})

	t.Run("get returns stored ciphertext byte-identically decrypted", func(t *testing.T) {
		ct, err := EncryptSecret("synthetic-preseeded", testKeyA)
		if err != nil {
			t.Fatal(err)
		}
		const id = "11111111-2222-4333-8444-555555555555"
		if _, err := db.Exec(`insert into secrets(id,purpose,ciphertext,created_at,updated_at) values(?,?,?,'0','0')`, id, "seed", ct); err != nil {
			t.Fatal(err)
		}
		got, err := store.Get(ctx, id)
		if err != nil || got != "synthetic-preseeded" {
			t.Fatalf("Get = (%q, %v)", got, err)
		}
	})

	t.Run("wrong master key surfaces the DecryptSecret error unchanged", func(t *testing.T) {
		id, err := store.Put(ctx, "p", "synthetic-wrong-key")
		if err != nil {
			t.Fatal(err)
		}
		var ciphertext string
		if err := db.QueryRow(`select ciphertext from secrets where id=?`, id).Scan(&ciphertext); err != nil {
			t.Fatal(err)
		}
		_, want := DecryptSecret(ciphertext, testKeyB)
		if want == nil {
			t.Fatal("expected wrong key to fail decryption")
		}
		got, err := NewSQLiteStore(db, testKeyB).Get(ctx, id)
		if got != "" || err == nil || err.Error() != want.Error() {
			t.Fatalf("Get = (%q, %v), want error %v", got, err, want)
		}
	})

	t.Run("invalid master key fails Put without writing", func(t *testing.T) {
		var before, after int
		db.QueryRow(`select count(*) from secrets`).Scan(&before)
		if id, err := NewSQLiteStore(db, "short").Put(ctx, "p", "x"); err == nil || id != "" {
			t.Fatalf("Put = (%q, %v)", id, err)
		}
		db.QueryRow(`select count(*) from secrets`).Scan(&after)
		if before != after {
			t.Fatal("failed Put wrote a row")
		}
	})

	t.Run("empty purpose accepted like PG; empty plaintext stores but cannot decrypt", func(t *testing.T) {
		id, err := store.Put(ctx, "", "synthetic-empty-purpose")
		if err != nil {
			t.Fatal(err)
		}
		if got, err := store.Get(ctx, id); err != nil || got != "synthetic-empty-purpose" {
			t.Fatalf("Get = (%q, %v)", got, err)
		}
		// PG parity: Put succeeds, and DecryptSecret rejects the empty ciphertext segment.
		id, err = store.Put(ctx, "p", "")
		if err != nil {
			t.Fatal(err)
		}
		if got, err := store.Get(ctx, id); err == nil || got != "" {
			t.Fatalf("Get = (%q, %v), want DecryptSecret error", got, err)
		}
	})

	t.Run("NUL in purpose is rejected", func(t *testing.T) {
		if id, err := store.Put(ctx, "a\x00b", "x"); err == nil || id != "" {
			t.Fatalf("Put = (%q, %v)", id, err)
		}
	})

	t.Run("canceled context", func(t *testing.T) {
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		if id, err := store.Put(canceled, "p", "x"); err == nil || id != "" {
			t.Fatalf("Put = (%q, %v)", id, err)
		}
		if got, err := store.Get(canceled, "00000000-0000-4000-8000-000000000000"); err == nil || got != "" {
			t.Fatalf("Get = (%q, %v)", got, err)
		}
	})
}

func TestSQLiteStoreConcurrentPuts(t *testing.T) {
	ctx := context.Background()
	store := NewSQLiteStore(openSQLiteSecretsDB(t), testKeyA)
	const n = 16
	ids := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ids[i], errs[i] = store.Put(ctx, "concurrent", "synthetic-"+string(rune('a'+i)))
		}(i)
	}
	wg.Wait()
	seen := map[string]bool{}
	for i := 0; i < n; i++ {
		if errs[i] != nil || seen[ids[i]] {
			t.Fatalf("put %d = (%q, %v)", i, ids[i], errs[i])
		}
		seen[ids[i]] = true
		got, err := store.Get(ctx, ids[i])
		if err != nil || got != "synthetic-"+string(rune('a'+i)) {
			t.Fatalf("get %d = (%q, %v)", i, got, err)
		}
	}
}

func TestSQLiteStoreClosedAndNil(t *testing.T) {
	ctx := context.Background()
	db := openSQLiteSecretsDB(t)
	store := NewSQLiteStore(db, testKeyA)
	id, err := store.Put(ctx, "p", "x-synthetic")
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := store.Put(ctx, "p", "x"); err == nil {
		t.Fatal("Put on closed db succeeded")
	}
	if _, err := store.Get(ctx, id); err == nil {
		t.Fatal("Get on closed db succeeded")
	}
	for name, s := range map[string]*SQLiteStore{"nil receiver": nil, "nil db": NewSQLiteStore(nil, testKeyA)} {
		if _, err := s.Put(ctx, "p", "x"); err == nil {
			t.Fatalf("%s: Put succeeded", name)
		}
		if _, err := s.Get(ctx, id); err == nil {
			t.Fatalf("%s: Get succeeded", name)
		}
	}
}
