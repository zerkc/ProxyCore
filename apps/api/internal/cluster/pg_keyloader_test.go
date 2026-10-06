package cluster

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// These tests never print key bytes: key assertions use bytes.Equal and report
// booleans only.

type fakePGTx struct {
	pgx.Tx
	commits, rollbacks int
	commitErr          error
}

func (t *fakePGTx) Commit(context.Context) error   { t.commits++; return t.commitErr }
func (t *fakePGTx) Rollback(context.Context) error { t.rollbacks++; return nil }
func (t *fakePGTx) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}
func (t *fakePGTx) QueryRow(context.Context, string, ...any) pgx.Row { return nil }

type fakePGBeginner struct {
	tx       *fakePGTx
	beginErr error
	opts     []pgx.TxOptions
}

func (b *fakePGBeginner) BeginTx(_ context.Context, o pgx.TxOptions) (pgx.Tx, error) {
	b.opts = append(b.opts, o)
	if b.beginErr != nil {
		return nil, b.beginErr
	}
	return b.tx, nil
}

type fakePGLoadStore struct {
	material KeyMaterial
	err      error
	gotID    *uuid.UUID
	gotTx    SQLTx
}

func (s *fakePGLoadStore) LoadOrCreate(_ context.Context, tx SQLTx, id *uuid.UUID) (KeyMaterial, error) {
	s.gotTx, s.gotID = tx, id
	return s.material, s.err
}

func newFakePGLoader(t *testing.T, material KeyMaterial, loadErr error) (*PGKeyLoader, *fakePGBeginner, *fakePGLoadStore) {
	t.Helper()
	b := &fakePGBeginner{tx: &fakePGTx{}}
	s := &fakePGLoadStore{material: material, err: loadErr}
	return &PGKeyLoader{begin: b, store: s}, b, s
}

func TestPGKeyLoaderCommitsAfterMatchingLoad(t *testing.T) {
	id := uuid.New()
	m, err := NewKeyMaterial(id, bytes.Repeat([]byte{7}, KEKLength))
	if err != nil {
		t.Fatal(err)
	}
	l, b, s := newFakePGLoader(t, m, nil)
	got, err := l.LoadActive(context.Background(), id)
	if err != nil {
		t.Fatalf("LoadActive: %v", err)
	}
	defer got.Destroy()
	if got.ID != id || !bytes.Equal(got.CopyBytes(), bytes.Repeat([]byte{7}, KEKLength)) {
		t.Fatal("loaded material does not match")
	}
	if b.tx.commits != 1 {
		t.Fatalf("commits=%d, want 1", b.tx.commits)
	}
	if s.gotID == nil || *s.gotID != id || s.gotTx == nil {
		t.Fatal("store did not receive the requested id and the owned tx")
	}
	if len(b.opts) != 1 || b.opts[0] != (pgx.TxOptions{}) {
		t.Fatalf("tx options=%v, want one default", b.opts)
	}
}

func TestPGKeyLoaderRejectsMismatchedIDBeforeCommit(t *testing.T) {
	m, err := NewKeyMaterial(uuid.New(), bytes.Repeat([]byte{7}, KEKLength))
	if err != nil {
		t.Fatal(err)
	}
	l, b, _ := newFakePGLoader(t, m, nil)
	got, err := l.LoadActive(context.Background(), uuid.New())
	if !errors.Is(err, ErrClusterKeyDenied) {
		t.Fatalf("error=%v, want ErrClusterKeyDenied", err)
	}
	if got.CopyBytes() != nil {
		t.Fatal("material leaked on mismatch")
	}
	if b.tx.commits != 0 {
		t.Fatalf("commits=%d, want 0 (id check precedes commit)", b.tx.commits)
	}
	if b.tx.rollbacks < 1 {
		t.Fatal("transaction was not rolled back")
	}
}

func TestPGKeyLoaderMapsEveryFailureToDeniedAndRollsBack(t *testing.T) {
	id := uuid.New()
	good, err := NewKeyMaterial(id, bytes.Repeat([]byte{7}, KEKLength))
	if err != nil {
		t.Fatal(err)
	}
	t.Run("store error", func(t *testing.T) {
		l, b, _ := newFakePGLoader(t, KeyMaterial{}, errors.New("boom"))
		_, err := l.LoadActive(context.Background(), id)
		if err != ErrClusterKeyDenied {
			t.Fatalf("error=%v, want bare ErrClusterKeyDenied", err)
		}
		if b.tx.commits != 0 || b.tx.rollbacks < 1 {
			t.Fatalf("commits=%d rollbacks=%d", b.tx.commits, b.tx.rollbacks)
		}
	})
	t.Run("commit error", func(t *testing.T) {
		l, b, _ := newFakePGLoader(t, good, nil)
		b.tx.commitErr = errors.New("commit failed")
		got, err := l.LoadActive(context.Background(), id)
		if err != ErrClusterKeyDenied || got.CopyBytes() != nil {
			t.Fatalf("error=%v leaked=%v", err, got.CopyBytes() != nil)
		}
		if b.tx.rollbacks < 1 {
			t.Fatal("transaction was not rolled back after failed commit")
		}
	})
	t.Run("begin error", func(t *testing.T) {
		l, b, s := newFakePGLoader(t, good, nil)
		b.beginErr = errors.New("no conn")
		if _, err := l.LoadActive(context.Background(), id); err != ErrClusterKeyDenied {
			t.Fatalf("error=%v", err)
		}
		if s.gotTx != nil {
			t.Fatal("store used without a transaction")
		}
	})
	t.Run("unconfigured", func(t *testing.T) {
		for name, l := range map[string]*PGKeyLoader{
			"nil": nil, "empty": {}, "nil pool": NewPGKeyLoader(nil, NewStore("x")), "nil store": NewPGKeyLoader(&pgxpool.Pool{}, nil),
		} {
			if _, err := l.LoadActive(context.Background(), id); err != ErrClusterKeyDenied {
				t.Fatalf("%s: error=%v", name, err)
			}
		}
		l, _, _ := newFakePGLoader(t, good, nil)
		//nolint:staticcheck // a nil context is the case under test.
		if _, err := l.LoadActive(nil, id); err != ErrClusterKeyDenied {
			t.Fatalf("nil ctx error=%v", err)
		}
	})
}

func TestPostgresPGKeyLoaderLoadsAfterFailures(t *testing.T) {
	pool := newLocalKEKLoaderPool(t)
	store := NewStore(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x31}, 32)))
	seed := loadLocalKEKTestMaterial(t, pool, store)
	defer seed.Destroy()
	loader := NewPGKeyLoader(pool, store)

	// Rollback-on-failure is pinned by the fake-transaction tests; here a failed
	// load must not poison later loads on the same pool.
	for i := 0; i < 3; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if _, err := loader.LoadActive(ctx, uuid.New()); err != ErrClusterKeyDenied {
			cancel()
			t.Fatalf("unknown id: error=%v, want ErrClusterKeyDenied", err)
		}
		got, err := loader.LoadActive(ctx, seed.ID)
		cancel()
		if err != nil {
			t.Fatalf("load after failure %d: %v", i, err)
		}
		if got.ID != seed.ID || !bytes.Equal(got.CopyBytes(), seed.CopyBytes()) {
			t.Fatal("loaded material differs from persisted key")
		}
		got.Destroy()
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := loader.LoadActive(canceled, seed.ID); err != ErrClusterKeyDenied {
		t.Fatalf("canceled ctx error=%v", err)
	}
}
