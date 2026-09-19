package httpserver_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
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
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/httpserver"
	"github.com/zerkc/ProxyCore/apps/api/internal/identity"
	replicationsnapshot "github.com/zerkc/ProxyCore/apps/api/internal/snapshot"
	syncpublication "github.com/zerkc/ProxyCore/apps/api/internal/sync"
)

func TestPostgresSnapshotPublicationReturnsCanonicalBody(t *testing.T) {
	fixture := newEnrollmentPublicationPostgresFixture(t)
	request := httptest.NewRequest(http.MethodGet, httpserver.EnrollmentSnapshotPublicationPath, nil)
	request.Host = "primary.example:3443"
	request.Header.Set("Authorization", "Bearer "+fixture.credential.BearerCopy())
	request.TLS = &tls.ConnectionState{Version: tls.VersionTLS13, HandshakeComplete: true, ServerName: "primary.example"}
	response := httptest.NewRecorder()
	fixture.handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !bytes.Equal(response.Body.Bytes(), fixture.body) {
		t.Fatalf("status=%d body=%x want=%x", response.Code, response.Body.Bytes(), fixture.body)
	}
}

func TestPostgresSnapshotPublicationLegacyNullBodyReturnsNoContent(t *testing.T) {
	fixture := newEnrollmentPublicationPostgresFixture(t)
	if _, err := fixture.pool.Exec(context.Background(), `update applied_snapshots set snapshot_body = null where id = $1`, fixture.snapshotID); err != nil {
		t.Fatalf("clear snapshot body: %v", err)
	}
	request := httptest.NewRequest(http.MethodGet, httpserver.EnrollmentSnapshotPublicationPath, nil)
	request.Host = "primary.example:3443"
	request.Header.Set("Authorization", "Bearer "+fixture.credential.BearerCopy())
	request.TLS = &tls.ConnectionState{Version: tls.VersionTLS13, HandshakeComplete: true, ServerName: "primary.example"}
	response := httptest.NewRecorder()
	fixture.handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent || response.Body.Len() != 0 {
		t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
	}
}

type enrollmentPublicationPostgresFixture struct {
	pool       *pgxpool.Pool
	handler    http.Handler
	credential syncpublication.NodeCredential
	body       []byte
	snapshotID uuid.UUID
}

func newEnrollmentPublicationPostgresFixture(t *testing.T) *enrollmentPublicationPostgresFixture {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if dsn == "" {
		dsn = strings.TrimSpace(os.Getenv("PHASE2_DATABASE_URL"))
	}
	if dsn == "" {
		t.Skip("DATABASE_URL or PHASE2_DATABASE_URL is not set")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Skip("cannot connect to database: " + err.Error())
	}
	schema := "pne65_http_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	primaryID, primaryNodeID := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `
		insert into installation_identity (id, installation_id, node_id, role, leadership_generation, latest_known_generation)
		values ('default', $1, $2, 'primary', 7, 7)
	`, primaryID, primaryNodeID); err != nil {
		t.Fatal(err)
	}
	masterKey := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x21}, 32))
	keyStore := cluster.NewStore(masterKey)
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	material, err := keyStore.LoadOrCreate(ctx, tx, nil)
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	keyID := material.ID
	keyBytes := material.CopyBytes()
	material.Destroy()
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	for i := range keyBytes {
		keyBytes[i] = 0
	}
	if _, err := pool.Exec(ctx, `update installation_identity set cluster_key_id = $1 where id = 'default'`, keyID); err != nil {
		t.Fatal(err)
	}
	credential, err := syncpublication.NewNodeCredential(uuid.NewString(), bytes.Repeat([]byte{0x42}, syncpublication.NodeCredentialSecretBytes))
	if err != nil {
		t.Fatal(err)
	}
	nodeID, nodeInstallationID, attemptID := uuid.New(), uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `insert into node_credentials (id, node_id, credential_hash, hash_version) values ($1, $2, $3, $4)`, credential.ID(), nodeID, credential.Hash(), syncpublication.NodeCredentialHashVersion); err != nil {
		credential.Destroy()
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `insert into enrolled_nodes (node_id, installation_id, primary_id, credential_id, created_by_attempt_id) values ($1, $2, $3, $4, $5)`, nodeID, nodeInstallationID, primaryID, credential.ID(), attemptID); err != nil {
		credential.Destroy()
		t.Fatal(err)
	}
	body, contentHash := enrollmentPublicationPostgresEnvelope(t, primaryID, primaryNodeID, keyID)
	revisionID, jobID, snapshotID := uuid.New(), uuid.New(), uuid.New()
	desired := []byte(`{"settings":{"revision":1},"zones":[],"streams":[],"certificates":[]}`)
	desiredSum := sha256.Sum256(desired)
	if _, err := pool.Exec(ctx, `insert into config_revisions (id, revision_number, checksum, snapshot, source, source_primary_id, snapshot_content_hash, snapshot_version, replication_version, leadership_generation, applied_at) values ($1, 1, $2, $3, 'ordinary', $4, $5, 1, 1, 7, now())`, revisionID, hex.EncodeToString(desiredSum[:]), desired, primaryID, contentHash); err != nil {
		credential.Destroy()
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `insert into apply_jobs (id, revision_id, target, status, correlation_id, source, source_primary_id, snapshot_content_hash, snapshot_version, replication_version, leadership_generation, finished_at) values ($1, $2, 'combined', 'applied', $3, 'ordinary', $4, $5, 1, 1, 7, now())`, jobID, revisionID, uuid.New(), primaryID, contentHash); err != nil {
		credential.Destroy()
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `insert into applied_snapshots (id, source_primary_id, leadership_generation, snapshot_version, replication_version, content_hash, snapshot_body, revision_id, status, apply_job_id, applied_at) values ($1, $2, 7, 1, 1, $3, $4, $5, 'applied', $6, now())`, snapshotID, primaryID, contentHash, body, revisionID, jobID); err != nil {
		credential.Destroy()
		t.Fatal(err)
	}
	identityService := identity.NewService(identity.NewPgStore(pool))
	if _, err := identityService.Load(ctx); err != nil {
		credential.Destroy()
		t.Fatal(err)
	}
	store := configuration.NewPhase2StoreWithMasterKey(pool, masterKey)
	fixture := &enrollmentPublicationPostgresFixture{pool: pool, credential: credential, body: body, snapshotID: snapshotID}
	fixture.handler = httpserver.NewEnrollmentSnapshotPublicationHandler(httpserver.EnrollmentSnapshotPublicationHandlerOptions{
		Store: store, Identity: identityService,
		Certificate: func(context.Context) (*x509.Certificate, error) {
			return &x509.Certificate{DNSNames: []string{"primary.example"}}, nil
		},
	})
	t.Cleanup(func() { credential.Destroy() })
	return fixture
}

func enrollmentPublicationPostgresEnvelope(t *testing.T, primaryID, primaryNodeID, keyID uuid.UUID) ([]byte, string) {
	t.Helper()
	envelope := replicationsnapshot.Envelope{
		Transient:  replicationsnapshot.TransientFields{SnapshotVersion: domain.SnapshotVersionV1, ReplicationVersion: domain.ReplicationVersionV1, SourcePrimaryID: primaryID, LeadershipGeneration: 7, CapturedAt: time.Now().UTC()},
		NodeLocal:  replicationsnapshot.NodeLocalFields{NodeID: domain.NodeID(primaryNodeID.String()), Role: domain.TopologyRolePrimary, LeadershipGeneration: 7, ClusterKeyRef: &keyID},
		Replicated: replicationsnapshot.ReplicatedFields{Configuration: map[string]any{"settings": map[string]any{}}},
	}
	hash, err := envelope.ExpectedHash()
	if err != nil {
		t.Fatal(err)
	}
	envelope.ContentHash = hash
	body, err := replicationsnapshot.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	return body, hash
}
