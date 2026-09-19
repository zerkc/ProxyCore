package configuration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zerkc/ProxyCore/apps/api/internal/auth"
	"github.com/zerkc/ProxyCore/apps/api/internal/cluster"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
)

type phase2SnapshotByTokenFixture struct {
	pool       *pgxpool.Pool
	admin      *pgxpool.Pool
	store      *PgPhase2Store
	masterKey  string
	selector   string
	primaryID  string
	nodeID     string
	clusterKey uuid.UUID
	identity   SnapshotPublicationIdentityRecord
}

func newPhase2SnapshotByTokenFixture(t *testing.T) *phase2SnapshotByTokenFixture {
	t.Helper()
	url := os.Getenv("PNE67_DATABASE_URL")
	if url == "" {
		url = os.Getenv("PHASE2_DATABASE_URL")
	}
	if url == "" {
		url = os.Getenv("DATABASE_URL")
	}
	if url == "" {
		t.Skip("PNE67_DATABASE_URL, PHASE2_DATABASE_URL, or DATABASE_URL is not configured")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect postgres: %v", err)
	}
	schema := "pne67_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	ident := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "create schema "+ident); err != nil {
		admin.Close()
		t.Fatalf("create schema: %v", err)
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		admin.Close()
		t.Fatalf("parse postgres config: %v", err)
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
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(), "drop schema if exists "+ident+" cascade")
		admin.Close()
	})
	if err := auth.NewPostgresStore(pool).EnsureSchema(ctx); err != nil {
		t.Fatalf("ensure auth schema: %v", err)
	}
	if err := EnsureSchema(ctx, pool); err != nil {
		t.Fatalf("ensure configuration schema: %v", err)
	}

	primaryID, nodeID, ownerID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	if _, err := pool.Exec(ctx, `
		insert into users (id, username, password_hash, role, active)
		values ($1, 'pne67-owner', 'scrypt$16384$8$1$hash$hash', 'owner', true)
	`, ownerID); err != nil {
		t.Fatalf("insert owner: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into installation_identity (id, installation_id, node_id, role, leadership_generation, latest_known_generation)
		values ('default', $1, $2, 'primary', 7, 7)
	`, primaryID, nodeID); err != nil {
		t.Fatalf("insert identity: %v", err)
	}
	masterKey := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x37}, 32))
	keyStore := cluster.NewStore(masterKey)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin cluster key transaction: %v", err)
	}
	material, err := keyStore.LoadOrCreate(ctx, tx, nil)
	if err != nil {
		t.Fatalf("create cluster key: %v", err)
	}
	clusterKey := material.ID
	material.Destroy()
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit cluster key: %v", err)
	}
	if _, err := pool.Exec(ctx, `update installation_identity set cluster_key_id = $1 where id = 'default'`, clusterKey); err != nil {
		t.Fatalf("bind cluster key: %v", err)
	}
	selector := strings.Repeat("ab", 16)
	createdAt := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	if _, err := pool.Exec(ctx, `
		insert into enrollment_tokens (id, token_selector, token_hash, hash_version, created_by_user_id, created_at, expires_at)
		values ($1, $2, 'stored-hash', $3, $4, $5, $6)
	`, uuid.NewString(), selector, EnrollmentTokenHashVersion, ownerID, createdAt, time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("insert enrollment token: %v", err)
	}
	keyID := clusterKey
	return &phase2SnapshotByTokenFixture{
		pool: pool, admin: admin, store: NewPhase2StoreWithMasterKey(pool, masterKey), masterKey: masterKey,
		selector: selector, primaryID: primaryID, nodeID: nodeID, clusterKey: clusterKey,
		identity: SnapshotPublicationIdentityRecord{
			InstallationID: primaryID, NodeID: nodeID, Role: domain.TopologyRolePrimary,
			LeadershipGeneration: 7, LatestKnownGeneration: 7, ClusterKeyID: &keyID,
		},
	}
}

func (f *phase2SnapshotByTokenFixture) insertSnapshot(t *testing.T, body []byte, terminal bool) {
	t.Helper()
	ctx := context.Background()
	revisionID, jobID, snapshotID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	finishedAt := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	appliedAt := finishedAt.Add(-time.Second)
	if !terminal {
		finishedAt = time.Time{}
	}
	desired := []byte(`{"settings":{}}`)
	desiredSum := sha256.Sum256(desired)
	bodySum := sha256.Sum256(body)
	if _, err := f.pool.Exec(ctx, `
		insert into config_revisions (
			id, revision_number, checksum, snapshot, source, source_primary_id,
			snapshot_content_hash, snapshot_version, replication_version, leadership_generation, applied_at
		) values ($1, 1, $2, $3, 'ordinary', $4, $5, 1, 1, 7, $6)
	`, revisionID, hex.EncodeToString(desiredSum[:]), desired, f.primaryID, hex.EncodeToString(bodySum[:]), nullableSnapshotTime(appliedAt)); err != nil {
		t.Fatalf("insert revision: %v", err)
	}
	jobStatus := "queued"
	if terminal {
		jobStatus = "applied"
	}
	if _, err := f.pool.Exec(ctx, `
		insert into apply_jobs (
			id, revision_id, target, status, correlation_id, source, source_primary_id,
			snapshot_content_hash, snapshot_version, replication_version, leadership_generation, finished_at
		) values ($1, $2, 'combined', $3, $4, 'ordinary', $5, $6, 1, 1, 7, $7)
	`, jobID, revisionID, jobStatus, uuid.NewString(), f.primaryID, hex.EncodeToString(bodySum[:]), nullableSnapshotTime(finishedAt)); err != nil {
		t.Fatalf("insert apply job: %v", err)
	}
	status := "pending"
	if terminal {
		status = "applied"
	}
	if _, err := f.pool.Exec(ctx, `
		insert into applied_snapshots (
			id, source_primary_id, leadership_generation, snapshot_version, replication_version,
			content_hash, snapshot_body, revision_id, status, apply_job_id, applied_at
		) values ($1, $2, 7, 1, 1, $3, $4, $5, $6, $7, $8)
	`, snapshotID, f.primaryID, hex.EncodeToString(bodySum[:]), body, revisionID, status, jobID, nullableSnapshotTime(appliedAt)); err != nil {
		t.Fatalf("insert applied snapshot: %v", err)
	}
}

func nullableSnapshotTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value
}

func (f *phase2SnapshotByTokenFixture) read(t *testing.T, store *PgPhase2Store, identity SnapshotPublicationIdentityRecord, selector ...string) ([]byte, error) {
	t.Helper()
	chosen := f.selector
	if len(selector) > 0 {
		chosen = selector[0]
	}
	return store.ReadSnapshotPublicationByToken(context.Background(), chosen, identity)
}

func TestPhase2ReadSnapshotPublicationByTokenHappyPath(t *testing.T) {
	fixture := newPhase2SnapshotByTokenFixture(t)
	body := []byte(`{"published":"canonical"}`)
	fixture.insertSnapshot(t, body, true)
	got, err := fixture.read(t, fixture.store, fixture.identity)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("read body=%q err=%v want=%q", got, err, body)
	}
	got[0] = 'X'
	second, err := fixture.read(t, fixture.store, fixture.identity)
	if err != nil || !bytes.Equal(second, body) {
		t.Fatalf("defensive copy body=%q err=%v want=%q", second, err, body)
	}
}

func TestPhase2ReadSnapshotPublicationByTokenNoPublishableSnapshot(t *testing.T) {
	for _, test := range []struct {
		name     string
		body     []byte
		terminal bool
	}{
		{name: "legacy null body", terminal: true},
		{name: "incomplete terminal proof", body: []byte(`{"published":true}`), terminal: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newPhase2SnapshotByTokenFixture(t)
			fixture.insertSnapshot(t, test.body, test.terminal)
			_, err := fixture.read(t, fixture.store, fixture.identity)
			if !errors.Is(err, ErrNoPublishableSnapshot) {
				t.Fatalf("error=%v want ErrNoPublishableSnapshot", err)
			}
		})
	}
}

func TestPhase2ReadSnapshotPublicationByTokenRejectsTokenLifecycle(t *testing.T) {
	for _, test := range []struct {
		name string
		want error
		set  string
	}{
		{name: "selector not found", want: ErrSnapshotByTokenUnauthenticated, set: "unknown"},
		{name: "hash version mismatch", want: ErrSnapshotByTokenUnauthenticated, set: "hash"},
		{name: "expired", want: ErrSnapshotByTokenUnauthenticated, set: "expired"},
		{name: "consumed", want: ErrSnapshotByTokenGone, set: "consumed"},
		{name: "revoked", want: ErrSnapshotByTokenGone, set: "revoked"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newPhase2SnapshotByTokenFixture(t)
			selector := fixture.selector
			switch test.set {
			case "unknown":
				selector = strings.Repeat("cd", 16)
			case "hash":
				if _, err := fixture.pool.Exec(context.Background(), `update enrollment_tokens set hash_version = 'sha256-other' where token_selector = $1`, selector); err != nil {
					t.Fatal(err)
				}
			case "expired":
				if _, err := fixture.pool.Exec(context.Background(), `update enrollment_tokens set expires_at = $2 where token_selector = $1`, selector, time.Date(2020, 1, 3, 0, 0, 0, 0, time.UTC)); err != nil {
					t.Fatal(err)
				}
			case "consumed":
				if _, err := fixture.pool.Exec(context.Background(), `update enrollment_tokens set consumed_at = $2 where token_selector = $1`, selector, time.Date(2020, 1, 2, 4, 0, 0, 0, time.UTC)); err != nil {
					t.Fatal(err)
				}
			case "revoked":
				if _, err := fixture.pool.Exec(context.Background(), `update enrollment_tokens set revoked_at = $2 where token_selector = $1`, selector, time.Date(2020, 1, 2, 4, 0, 0, 0, time.UTC)); err != nil {
					t.Fatal(err)
				}
			}
			_, err := fixture.read(t, fixture.store, fixture.identity, selector)
			if !errors.Is(err, test.want) {
				t.Fatalf("error=%v want %v", err, test.want)
			}
		})
	}
}

func TestPhase2ReadSnapshotPublicationByTokenRejectsIdentityAndKeyFailures(t *testing.T) {
	for _, role := range []domain.TopologyRole{domain.TopologyRoleStandalone, domain.TopologyRoleNode, domain.TopologyRoleStalePrimary} {
		t.Run(string(role), func(t *testing.T) {
			fixture := newPhase2SnapshotByTokenFixture(t)
			identity := fixture.identity
			identity.Role = role
			_, err := fixture.read(t, fixture.store, identity)
			if !errors.Is(err, ErrSnapshotByTokenDenied) {
				t.Fatalf("role=%s error=%v", role, err)
			}
		})
	}
	for _, test := range []struct {
		name string
		edit func(*SnapshotPublicationIdentityRecord)
	}{
		{name: "zero generation", edit: func(identity *SnapshotPublicationIdentityRecord) { identity.LeadershipGeneration = 0 }},
		{name: "nil cluster key", edit: func(identity *SnapshotPublicationIdentityRecord) { identity.ClusterKeyID = nil }},
		{name: "wrong cluster key", edit: func(identity *SnapshotPublicationIdentityRecord) { wrong := uuid.New(); identity.ClusterKeyID = &wrong }},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newPhase2SnapshotByTokenFixture(t)
			identity := fixture.identity
			test.edit(&identity)
			_, err := fixture.read(t, fixture.store, identity)
			if !errors.Is(err, ErrSnapshotByTokenDenied) {
				t.Fatalf("error=%v want ErrSnapshotByTokenDenied", err)
			}
		})
	}
	fixture := newPhase2SnapshotByTokenFixture(t)
	wrongMaster := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x38}, 32))
	_, err := fixture.read(t, NewPhase2StoreWithMasterKey(fixture.pool, wrongMaster), fixture.identity)
	if !errors.Is(err, ErrSnapshotByTokenUnavailable) {
		t.Fatalf("wrong master error=%v want ErrSnapshotByTokenUnavailable", err)
	}
}

func TestPhase2ReadSnapshotPublicationByTokenDatabaseFailure(t *testing.T) {
	fixture := newPhase2SnapshotByTokenFixture(t)
	fixture.pool.Close()
	_, err := fixture.read(t, fixture.store, fixture.identity)
	if !errors.Is(err, ErrSnapshotByTokenUnavailable) {
		t.Fatalf("closed database error=%v want ErrSnapshotByTokenUnavailable", err)
	}
}
