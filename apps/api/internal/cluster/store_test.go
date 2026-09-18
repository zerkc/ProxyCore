package cluster

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestKeyMaterialRedactsAndDestroys(t *testing.T) {
	canary := bytes.Repeat([]byte{0x7a}, KEKLength)
	defer zeroClusterTestBytes(canary)
	material, err := NewKeyMaterial(uuid.MustParse("00112233-4455-4667-8899-aabbccddeeff"), canary)
	if err != nil {
		t.Fatalf("NewKeyMaterial: %v", err)
	}
	if got := material.CopyBytes(); !bytes.Equal(got, canary) {
		t.Fatalf("key copy mismatch: %x", got)
	}
	if rendered := fmt.Sprint(material); rendered == string(canary) || rendered == fmt.Sprintf("%x", canary) {
		t.Fatalf("key material rendered plaintext: %q", rendered)
	}
	material.Destroy()
	if got := material.CopyBytes(); got != nil {
		t.Fatalf("destroyed key copied %x", got)
	}
}

func TestPostgresStoreCreatesAndReloadsWrappedKEK(t *testing.T) {
	pool := newClusterStorePool(t)
	ctx := context.Background()
	masterKey := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x19}, 32))
	store := NewStore(masterKey)
	created := loadOrCreateClusterKey(t, pool, store, nil)
	key := created.CopyBytes()
	defer zeroClusterTestBytes(key)
	createdID := created.ID
	created.Destroy()

	var count int
	var wrapped string
	if err := pool.QueryRow(ctx, `select count(*), max(wrapped_kek) from cluster_keys`).Scan(&count, &wrapped); err != nil {
		t.Fatalf("read cluster key row: %v", err)
	}
	if count != 1 || wrapped == "" || strings.Contains(wrapped, string(key)) {
		t.Fatalf("cluster key persistence leaked plaintext or wrong count: count=%d wrapped=%q", count, wrapped)
	}

	restarted := NewStore(masterKey)
	loaded := loadOrCreateClusterKey(t, pool, restarted, &createdID)
	defer loaded.Destroy()
	if !bytes.Equal(key, loaded.CopyBytes()) {
		t.Fatal("restarted store loaded a different KEK")
	}
	zeroClusterTestBytes(key)
}

func TestPostgresStoreDeniesWrongMasterTamperMissingAndCancelledContext(t *testing.T) {
	pool := newClusterStorePool(t)
	ctx := context.Background()
	masterKey := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x29}, 32))
	created := loadOrCreateClusterKey(t, pool, NewStore(masterKey), nil)
	id := created.ID
	created.Destroy()

	wrong := NewStore(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x39}, 32)))
	if err := loadClusterKeyError(pool, wrong, &id); !errors.Is(err, ErrClusterKeyDenied) {
		t.Fatalf("wrong master error = %v, want ErrClusterKeyDenied", err)
	}
	var originalWrapped string
	if err := pool.QueryRow(ctx, `select wrapped_kek from cluster_keys where id = $1`, id).Scan(&originalWrapped); err != nil {
		t.Fatalf("read wrapped key: %v", err)
	}
	if _, err := pool.Exec(ctx, `update cluster_keys set wrapped_kek = 'tampered' where id = $1`, id); err != nil {
		t.Fatalf("tamper wrapped key: %v", err)
	}
	if err := loadClusterKeyError(pool, NewStore(masterKey), &id); !errors.Is(err, ErrClusterKeyDenied) {
		t.Fatalf("tampered key error = %v, want ErrClusterKeyDenied", err)
	}
	if _, err := pool.Exec(ctx, `update cluster_keys set wrapped_kek = $2 where id = $1`, id, originalWrapped); err != nil {
		t.Fatalf("restore wrapped key: %v", err)
	}
	missing := uuid.New()
	if err := loadClusterKeyError(pool, NewStore(masterKey), &missing); !errors.Is(err, ErrClusterKeyDenied) {
		t.Fatalf("missing key error = %v, want ErrClusterKeyDenied", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin cancelled test: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := NewStore(masterKey).LoadOrCreate(cancelled, tx, nil); !errors.Is(err, ErrClusterKeyDenied) {
		t.Fatalf("cancelled context error = %v, want ErrClusterKeyDenied", err)
	}
}

func TestPostgresStoreConcurrentCreateUsesOneActiveKEK(t *testing.T) {
	pool := newClusterStorePool(t)
	masterKey := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x49}, 32))
	store := NewStore(masterKey)
	results := make(chan clusterKeyResult, 8)
	var wait sync.WaitGroup
	for i := 0; i < cap(results); i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			material, err := loadOrCreateClusterKeyRaw(pool, store, nil)
			if err != nil {
				results <- clusterKeyResult{err: err}
				return
			}
			id := material.ID
			material.Destroy()
			results <- clusterKeyResult{id: id}
		}()
	}
	wait.Wait()
	close(results)
	var first uuid.UUID
	for result := range results {
		if result.err != nil {
			t.Fatalf("concurrent LoadOrCreate: %v", result.err)
		}
		if first == uuid.Nil {
			first = result.id
		} else if result.id != first {
			t.Fatalf("concurrent active key IDs differ: %s and %s", first, result.id)
		}
	}
	var count int
	if err := pool.QueryRow(context.Background(), `select count(*) from cluster_keys where retired_at is null`).Scan(&count); err != nil {
		t.Fatalf("count active keys: %v", err)
	}
	if count != 1 {
		t.Fatalf("active cluster keys = %d, want 1", count)
	}
}

type clusterKeyResult struct {
	id  uuid.UUID
	err error
}

func loadOrCreateClusterKey(t *testing.T, pool *pgxpool.Pool, store *Store, id *uuid.UUID) KeyMaterial {
	t.Helper()
	material, err := loadOrCreateClusterKeyRaw(pool, store, id)
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	return material
}

func loadOrCreateClusterKeyRaw(pool *pgxpool.Pool, store *Store, id *uuid.UUID) (KeyMaterial, error) {
	tx, err := pool.Begin(context.Background())
	if err != nil {
		return KeyMaterial{}, err
	}
	material, err := store.LoadOrCreate(context.Background(), tx, id)
	if err != nil {
		_ = tx.Rollback(context.Background())
		return KeyMaterial{}, err
	}
	if err := tx.Commit(context.Background()); err != nil {
		material.Destroy()
		return KeyMaterial{}, err
	}
	return material, nil
}

func loadClusterKeyError(pool *pgxpool.Pool, store *Store, id *uuid.UUID) error {
	tx, err := pool.Begin(context.Background())
	if err != nil {
		return err
	}
	_, loadErr := store.LoadOrCreate(context.Background(), tx, id)
	_ = tx.Rollback(context.Background())
	return loadErr
}

func newClusterStorePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if dsn == "" {
		t.Skip("DATABASE_URL is not set")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Skip("cannot connect to database: " + err.Error())
	}
	schema := "pne3d_cluster_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	ident := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "create schema "+ident); err != nil {
		admin.Close()
		t.Skip("cannot create schema: " + err.Error())
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		admin.Close()
		t.Fatalf("parse database config: %v", err)
	}
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		admin.Close()
		t.Fatalf("connect isolated schema: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		create table cluster_keys (
			id uuid primary key,
			purpose text not null,
			wrapped_kek text not null,
			wrapping_key_version integer not null,
			created_at timestamptz not null default now(),
			retired_at timestamptz
		)
	`); err != nil {
		pool.Close()
		admin.Close()
		t.Fatalf("create cluster_keys: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(), "drop schema if exists "+ident+" cascade")
		admin.Close()
	})
	return pool
}

func zeroClusterTestBytes(value []byte) {
	for i := range value {
		value[i] = 0
	}
}
