package httpserver_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zerkc/ProxyCore/apps/api/internal/auth"
	"github.com/zerkc/ProxyCore/apps/api/internal/cluster"
	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
	"github.com/zerkc/ProxyCore/apps/api/internal/httpserver"
	"github.com/zerkc/ProxyCore/apps/api/internal/identity"
	syncpublication "github.com/zerkc/ProxyCore/apps/api/internal/sync"
)

type enrollmentAcknowledgementPostgresFixture struct {
	pool        *pgxpool.Pool
	admin       *pgxpool.Pool
	handler     http.Handler
	credential  syncpublication.NodeCredential
	nodeID      uuid.UUID
	contentHash string
	appliedAt   time.Time
	keyStore    *cluster.Store
}

func newEnrollmentAcknowledgementPostgresFixture(t *testing.T) *enrollmentAcknowledgementPostgresFixture {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("PNE8_DATABASE_URL"))
	if dsn == "" {
		dsn = strings.TrimSpace(os.Getenv("PHASE2_DATABASE_URL"))
	}
	if dsn == "" {
		dsn = strings.TrimSpace(os.Getenv("DATABASE_URL"))
	}
	if dsn == "" {
		t.Skip("PNE8_DATABASE_URL, PHASE2_DATABASE_URL, or DATABASE_URL is not configured")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Skip("cannot connect to database: " + err.Error())
	}
	schema := "pne8_http_ack_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(), "drop schema if exists "+ident+" cascade")
		admin.Close()
	})
	if err := auth.NewPostgresStore(pool).EnsureSchema(ctx); err != nil {
		t.Fatal(err)
	}
	if err := configuration.EnsureSchema(ctx, pool); err != nil {
		t.Fatal(err)
	}
	ownerID, primaryID, primaryNodeID := uuid.New(), uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `insert into users (id, username, password_hash, role, active) values ($1, 'pne8-http-owner', 'scrypt$16384$8$1$hash$hash', 'owner', true)`, ownerID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `insert into installation_identity (id, installation_id, node_id, role, leadership_generation, latest_known_generation) values ('default', $1, $2, 'primary', 7, 7)`, primaryID, primaryNodeID); err != nil {
		t.Fatal(err)
	}
	masterKey := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x31}, 32))
	keyStore := cluster.NewStore(masterKey)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	material, err := keyStore.LoadOrCreate(ctx, tx, nil)
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	keyID := material.ID
	material.Destroy()
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `update installation_identity set cluster_key_id = $1 where id = 'default'`, keyID); err != nil {
		t.Fatal(err)
	}
	fixture := &enrollmentAcknowledgementPostgresFixture{pool: pool, admin: admin, keyStore: keyStore}
	credential, err := syncpublication.NewNodeCredential(uuid.NewString(), bytes.Repeat([]byte{0x52}, syncpublication.NodeCredentialSecretBytes))
	if err != nil {
		t.Fatal(err)
	}
	nodeID, nodeInstallationID, attemptID, tokenID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	createdAt := time.Date(2026, 2, 3, 3, 5, 6, 0, time.UTC)
	if _, err := pool.Exec(ctx, `insert into node_credentials (id, node_id, credential_hash, hash_version) values ($1, $2, $3, $4)`, credential.ID(), nodeID, credential.Hash(), syncpublication.NodeCredentialHashVersion); err != nil {
		credential.Destroy()
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `insert into enrollment_tokens (id, token_selector, token_hash, hash_version, created_by_user_id, created_at, expires_at, consumed_at, consumed_by_attempt_id) values ($1, $2, 'token-hash', $3, $4, $5, $6, $7, $8)`, tokenID, strings.Repeat("cd", 16), configuration.EnrollmentTokenHashVersion, ownerID, createdAt, createdAt.Add(time.Hour), createdAt.Add(time.Minute), attemptID); err != nil {
		credential.Destroy()
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `insert into enrollment_attempts (id, state, primary_url, expected_primary_id, local_node_ip, cluster_key_id, ephemeral_private_key_wrapped, created_at, updated_at) values ($1, 'committed', 'https://primary.example', $2, '127.0.0.1', $3, 'wrapped', $4, $4)`, attemptID, primaryID, keyID, createdAt); err != nil {
		credential.Destroy()
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `insert into enrollment_grants (attempt_id, token_id, installation_id, node_id, primary_id, primary_generation, node_ephemeral_public_key, sealed_bootstrap_payload, payload_hash, created_at, expires_at) values ($1, $2, $3, $4, $5, 7, 'ephemeral', 'sealed', 'payload-hash', $6, $7)`, attemptID, tokenID, nodeInstallationID, nodeID, primaryID, createdAt, createdAt.Add(time.Hour)); err != nil {
		credential.Destroy()
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `insert into enrolled_nodes (node_id, installation_id, primary_id, credential_id, created_by_attempt_id, enrolled_at) values ($1, $2, $3, $4, $5, $6)`, nodeID, nodeInstallationID, primaryID, credential.ID(), attemptID, createdAt); err != nil {
		credential.Destroy()
		t.Fatal(err)
	}
	contentHash := "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	appliedAt := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	revisionID, jobID := uuid.New(), uuid.New()
	desired := []byte(`{"settings":{}}`)
	if _, err := pool.Exec(ctx, `insert into config_revisions (id, revision_number, checksum, snapshot, source, source_primary_id, snapshot_content_hash, snapshot_version, replication_version, leadership_generation, applied_at) values ($1, 1, $2, $3, 'ordinary', $4, $5, 1, 1, 7, $6)`, revisionID, strings.Repeat("a", 64), desired, primaryID, contentHash, appliedAt); err != nil {
		credential.Destroy()
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `insert into apply_jobs (id, revision_id, target, status, correlation_id, source, source_primary_id, snapshot_content_hash, snapshot_version, replication_version, leadership_generation, finished_at) values ($1, $2, 'combined', 'applied', $3, 'ordinary', $4, $5, 1, 1, 7, $6)`, jobID, revisionID, uuid.NewString(), primaryID, contentHash, appliedAt.Add(time.Minute)); err != nil {
		credential.Destroy()
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `insert into applied_snapshots (id, source_primary_id, leadership_generation, snapshot_version, replication_version, content_hash, snapshot_body, revision_id, status, apply_job_id, applied_at) values ($1, $2, 7, 1, 1, $3, $4, $5, 'applied', $6, $7)`, uuid.New(), primaryID, contentHash, []byte(`{}`), revisionID, jobID, appliedAt); err != nil {
		credential.Destroy()
		t.Fatal(err)
	}
	identityService := identity.NewService(identity.NewPgStore(pool))
	if _, err := identityService.Load(ctx); err != nil {
		credential.Destroy()
		t.Fatal(err)
	}
	store := configuration.NewPhase2StoreWithMasterKey(pool, masterKey)
	fixture = &enrollmentAcknowledgementPostgresFixture{pool: pool, admin: admin, credential: credential, nodeID: nodeID, contentHash: contentHash, appliedAt: appliedAt, keyStore: keyStore}
	fixture.handler = httpserver.NewEnrollmentSnapshotAcknowledgementHandler(httpserver.EnrollmentSnapshotAcknowledgementHandlerOptions{
		Store: store, Identity: identityService, Certificate: func(context.Context) (*x509.Certificate, error) {
			return &x509.Certificate{DNSNames: []string{"primary.example"}}, nil
		},
	})
	t.Cleanup(func() { credential.Destroy() })
	return fixture
}

func TestPostgresEnrollmentAcknowledgmentRecordsExactApply(t *testing.T) {
	fixture := newEnrollmentAcknowledgementPostgresFixture(t)
	body := `{"nodeId":"` + fixture.nodeID.String() + `","primaryUrl":"https://primary.example:3443/","contentHash":"` + fixture.contentHash + `","appliedAt":"` + fixture.appliedAt.Format(time.RFC3339Nano) + `"}`
	request := httptest.NewRequest(http.MethodPost, httpserver.EnrollmentSnapshotAcknowledgementPath, bytes.NewBufferString(body))
	request.Host = "primary.example:3443"
	request.Header.Set("Authorization", "Bearer "+fixture.credential.BearerCopy())
	request.TLS = &tls.ConnectionState{Version: tls.VersionTLS13, HandshakeComplete: true, ServerName: "primary.example"}
	response := httptest.NewRecorder()
	fixture.handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent || response.Body.Len() != 0 {
		t.Fatalf("status=%d bodyLen=%d bodyHash=%x", response.Code, response.Body.Len(), sha256.Sum256(response.Body.Bytes()))
	}
	var count int
	if err := fixture.pool.QueryRow(context.Background(), `select count(*) from node_snapshot_acks where node_id = $1 and content_hash = $2`, fixture.nodeID, fixture.contentHash).Scan(&count); err != nil || count != 1 {
		t.Fatalf("ack count=%d err=%v", count, err)
	}
}

func TestPostgresEnrollmentAcknowledgmentRejectsRevokedCredential(t *testing.T) {
	fixture := newEnrollmentAcknowledgementPostgresFixture(t)
	if _, err := fixture.pool.Exec(context.Background(), `update node_credentials set revoked_at = now() where id = $1`, fixture.credential.ID()); err != nil {
		t.Fatal(err)
	}
	body := `{"nodeId":"` + fixture.nodeID.String() + `","primaryUrl":"https://primary.example:3443/","contentHash":"` + fixture.contentHash + `","appliedAt":"` + fixture.appliedAt.Format(time.RFC3339Nano) + `"}`
	request := httptest.NewRequest(http.MethodPost, httpserver.EnrollmentSnapshotAcknowledgementPath, bytes.NewBufferString(body))
	request.Host = "primary.example:3443"
	request.Header.Set("Authorization", "Bearer "+fixture.credential.BearerCopy())
	request.TLS = &tls.ConnectionState{Version: tls.VersionTLS13, HandshakeComplete: true, ServerName: "primary.example"}
	response := httptest.NewRecorder()
	fixture.handler.ServeHTTP(response, request)
	if response.Code != http.StatusGone || strings.Contains(response.Body.String(), fixture.credential.BearerCopy()) {
		t.Fatalf("status=%d bodyLen=%d bodyHash=%x", response.Code, response.Body.Len(), sha256.Sum256(response.Body.Bytes()))
	}
}

func TestPostgresEnrollmentAcknowledgmentRejectsStaleLineageClusterKey(t *testing.T) {
	fixture := newEnrollmentAcknowledgementPostgresFixture(t)
	ctx := context.Background()
	var originalKeyID uuid.UUID
	if err := fixture.pool.QueryRow(ctx, `select cluster_key_id from installation_identity where id = 'default'`).Scan(&originalKeyID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(ctx, `update cluster_keys set retired_at = now() where id = $1`, originalKeyID); err != nil {
		t.Fatal(err)
	}
	tx, err := fixture.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	material, err := fixture.keyStore.LoadOrCreate(ctx, tx, nil)
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	rotatedKeyID := material.ID
	material.Destroy()
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if rotatedKeyID == originalKeyID {
		t.Fatalf("rotation reused original cluster key %s", originalKeyID)
	}
	if _, err := fixture.pool.Exec(ctx, `update installation_identity set cluster_key_id = $1 where id = 'default'`, rotatedKeyID); err != nil {
		t.Fatal(err)
	}
	body := `{"nodeId":"` + fixture.nodeID.String() + `","primaryUrl":"https://primary.example:3443/","contentHash":"` + fixture.contentHash + `","appliedAt":"` + fixture.appliedAt.Format(time.RFC3339Nano) + `"}`
	request := httptest.NewRequest(http.MethodPost, httpserver.EnrollmentSnapshotAcknowledgementPath, bytes.NewBufferString(body))
	request.Host = "primary.example:3443"
	request.Header.Set("Authorization", "Bearer "+fixture.credential.BearerCopy())
	request.TLS = &tls.ConnectionState{Version: tls.VersionTLS13, HandshakeComplete: true, ServerName: "primary.example"}
	response := httptest.NewRecorder()
	fixture.handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden || strings.Contains(response.Body.String(), fixture.credential.BearerCopy()) {
		t.Fatalf("status=%d bodyLen=%d bodyHash=%x", response.Code, response.Body.Len(), sha256.Sum256(response.Body.Bytes()))
	}
}
