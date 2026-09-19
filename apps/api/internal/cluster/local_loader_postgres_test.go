package cluster

import (
	"bytes"
	"context"
	"encoding/base64"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/identity"
)

func TestPostgresLocalKEKLoaderOwnsAndCommitsTransaction(t *testing.T) {
	pool := newLocalKEKLoaderPool(t)
	ctx := context.Background()
	masterKey := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x31}, 32))
	store := NewStore(masterKey)
	material := loadLocalKEKTestMaterial(t, pool, store)
	keyID := material.ID
	keyBytes := material.CopyBytes()
	material.Destroy()
	installationID := domain.NewInstallationID()
	nodeID := domain.NewNodeID()
	if _, err := pool.Exec(ctx, `
		insert into installation_identity (id, installation_id, node_id, role, leadership_generation, latest_known_generation, cluster_key_id)
		values ('default', $1, $2, 'node', 2, 2, $3)
	`, installationID, nodeID, keyID); err != nil {
		t.Fatalf("insert identity: %v", err)
	}
	identityService := identity.NewService(identity.NewPgStore(pool))
	if _, err := identityService.Load(ctx); err != nil {
		t.Fatalf("load identity: %v", err)
	}
	loader := NewLocalKEKLoader(LocalKEKLoaderOptions{Pool: pool, Store: store, Identity: identityService})
	kek, loadedID, err := loader.LoadLocalKEK(ctx)
	if err != nil {
		t.Fatalf("LoadLocalKEK: %v", err)
	}
	if loadedID != keyID {
		t.Fatalf("loaded key id=%s want %s", loadedID, keyID)
	}
	defer kek.Destroy()
	wrapped, err := kek.Wrap([]byte("transaction committed"))
	if err != nil {
		t.Fatalf("wrap with loaded KEK: %v", err)
	}
	plaintext, err := kek.Unwrap(wrapped)
	if err != nil || string(plaintext) != "transaction committed" {
		t.Fatalf("unwrap=%q err=%v", plaintext, err)
	}
	for i := range plaintext {
		plaintext[i] = 0
	}
	if !bytes.Equal(keyBytes, loadLocalKEKTestBytes(t, pool, store, keyID)) {
		t.Fatal("committed loader changed persisted key")
	}
	for i := range keyBytes {
		keyBytes[i] = 0
	}
}

func loadLocalKEKTestMaterial(t *testing.T, pool *pgxpool.Pool, store *Store) KeyMaterial {
	t.Helper()
	tx, err := pool.BeginTx(context.Background(), pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	material, err := store.LoadOrCreate(context.Background(), tx, nil)
	if err != nil {
		_ = tx.Rollback(context.Background())
		t.Fatal(err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		material.Destroy()
		t.Fatal(err)
	}
	return material
}

func loadLocalKEKTestBytes(t *testing.T, pool *pgxpool.Pool, store *Store, id uuid.UUID) []byte {
	t.Helper()
	tx, err := pool.BeginTx(context.Background(), pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	material, err := store.LoadOrCreate(context.Background(), tx, &id)
	if err != nil {
		_ = tx.Rollback(context.Background())
		t.Fatal(err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		material.Destroy()
		t.Fatal(err)
	}
	defer material.Destroy()
	return material.CopyBytes()
}

func newLocalKEKLoaderPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("PHASE2_DATABASE_URL"))
	if dsn == "" {
		dsn = strings.TrimSpace(os.Getenv("DATABASE_URL"))
	}
	if dsn == "" {
		t.Skip("PHASE2_DATABASE_URL or DATABASE_URL is not set")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Skip("cannot connect to database: " + err.Error())
	}
	schema := "pne65_cluster_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	ident := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "create schema "+ident); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		admin.Close()
		t.Fatal(err)
	}
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		admin.Close()
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		create table cluster_keys (
			id uuid primary key, purpose text not null, wrapped_kek text not null,
			wrapping_key_version integer not null, created_at timestamptz not null default now(), retired_at timestamptz
		);
		create table installation_identity (
			id text primary key, installation_id uuid not null, node_id uuid not null,
			role text not null, leadership_generation bigint not null,
			latest_known_generation bigint not null, cluster_key_id uuid, updated_at timestamptz
		);
	`); err != nil {
		pool.Close()
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(), "drop schema if exists "+ident+" cascade")
		admin.Close()
	})
	return pool
}
