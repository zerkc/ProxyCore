package enrollment

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zerkc/ProxyCore/apps/api/internal/auth"
	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/identity"
	replicationsnapshot "github.com/zerkc/ProxyCore/apps/api/internal/snapshot"
	syncapply "github.com/zerkc/ProxyCore/apps/api/internal/sync"
)

type nodeConverterPostgresFixture struct {
	pool    *pgxpool.Pool
	admin   *pgxpool.Pool
	store   *configuration.Store
	service *identity.Service
}

func newNodeConverterPostgresFixture(t *testing.T) *nodeConverterPostgresFixture {
	t.Helper()
	url := os.Getenv("PNE5_DATABASE_URL")
	if url == "" {
		url = os.Getenv("PHASE2_DATABASE_URL")
	}
	if url == "" {
		url = os.Getenv("DATABASE_URL")
	}
	if url == "" {
		t.Skip("PNE5_DATABASE_URL, PHASE2_DATABASE_URL, or DATABASE_URL is not configured")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect postgres: %v", err)
	}
	schema := "pne5_converter_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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
	if err := configuration.EnsureSchema(ctx, pool); err != nil {
		t.Fatalf("ensure configuration schema: %v", err)
	}
	installationID, nodeID := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `
		insert into installation_identity (id, installation_id, node_id, role, leadership_generation, latest_known_generation)
		values ('default', $1, $2, 'standalone-primary', 1, 1)
	`, installationID, nodeID); err != nil {
		t.Fatalf("insert identity: %v", err)
	}
	service := identity.NewService(identity.NewPgStore(pool))
	if _, err := service.Load(ctx); err != nil {
		t.Fatalf("load identity: %v", err)
	}
	return &nodeConverterPostgresFixture{
		pool:    pool,
		admin:   admin,
		store:   configuration.New(pool, os.Getenv("PROXYCORE_MASTER_KEY_BASE64"), domain.Ingress{}),
		service: service,
	}
}

type postgresTerminalCompleter struct {
	store          *configuration.Store
	pool           *pgxpool.Pool
	terminalStatus string
	completed      bool
}

func (r *postgresTerminalCompleter) GetApplyJobTerminal(ctx context.Context, jobID uuid.UUID) (syncapply.ApplyJobTerminal, error) {
	current, err := r.store.GetApplyJobTerminal(ctx, jobID)
	if err != nil || r.completed || current.Status != "queued" {
		return current, err
	}
	r.completed = true
	finished := time.Date(2026, 5, 10, 11, 12, 14, 0, time.UTC)
	if _, err := r.pool.Exec(ctx, `update apply_jobs set status = $2, finished_at = $3 where id = $1`, jobID, r.terminalStatus, finished); err != nil {
		return syncapply.ApplyJobTerminal{}, err
	}
	return r.store.GetApplyJobTerminal(ctx, jobID)
}

func TestPostgresNodeConverterArchivesQueuesWaitsAndCommitsIdentity(t *testing.T) {
	fixture := newNodeConverterPostgresFixture(t)
	completer := &postgresTerminalCompleter{store: fixture.store, pool: fixture.pool, terminalStatus: "applied"}
	converter := NewNodeConverter(NodeConverterOptions{
		Importer:      replicationsnapshot.NewImporter(fixture.store, fixture.store, func() time.Time { return time.Date(2026, 5, 10, 11, 12, 13, 0, time.UTC) }),
		Archive:       fixture.store,
		Identity:      fixture.service,
		ApplyWaiter:   completer,
		ApplyEnqueuer: fixture.store,
	})
	env := postgresConverterEnvelope(t)
	current := fixture.service.Current()
	result, err := converter.Convert(context.Background(), NodeConversionInput{
		Envelope:         &env,
		LocalNodeID:      current.NodeID,
		LocalIngress:     domain.Ingress{IPv4: "198.51.100.20"},
		LocalInstallId:   current.InstallationID,
		ArchiveTTL:       24 * time.Hour,
		ApplyWaitTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if result.ArchiveID == uuid.Nil || result.ApplyJobID == uuid.Nil || result.Identity.Role != domain.TopologyRoleNode {
		t.Fatalf("conversion result=%+v", result)
	}
	if fixture.service.Current().Role != domain.TopologyRoleNode {
		t.Fatalf("identity cache role=%s, want node", fixture.service.Current().Role)
	}
	var archives, appliedJobs int
	if err := fixture.pool.QueryRow(context.Background(), `select count(*) from standalone_archives where id = $1`, result.ArchiveID).Scan(&archives); err != nil {
		t.Fatalf("read archive: %v", err)
	}
	if err := fixture.pool.QueryRow(context.Background(), `select count(*) from apply_jobs where id = $1 and source = 'import' and status = 'applied' and finished_at is not null`, result.ApplyJobID).Scan(&appliedJobs); err != nil {
		t.Fatalf("read applied job: %v", err)
	}
	if archives != 1 || appliedJobs != 1 {
		t.Fatalf("archive rows=%d applied job rows=%d", archives, appliedJobs)
	}
	var durableRole string
	if err := fixture.pool.QueryRow(context.Background(), `select role::text from installation_identity where id = 'default'`).Scan(&durableRole); err != nil {
		t.Fatalf("read durable role: %v", err)
	}
	if durableRole != string(domain.TopologyRoleNode) {
		t.Fatalf("durable role=%s, want node", durableRole)
	}
}

func postgresConverterEnvelope(t *testing.T) replicationsnapshot.Envelope {
	t.Helper()
	env := replicationsnapshot.Envelope{
		Transient: replicationsnapshot.TransientFields{
			SnapshotVersion:      domain.SnapshotVersionV1,
			ReplicationVersion:   domain.ReplicationVersionV1,
			SourcePrimaryID:      uuid.New(),
			LeadershipGeneration: 4,
			CapturedAt:           time.Date(2026, 5, 10, 11, 12, 13, 0, time.UTC),
		},
		NodeLocal: replicationsnapshot.NodeLocalFields{
			NodeID: domain.NewNodeID(),
			Role:   domain.TopologyRolePrimary,
		},
		Replicated: replicationsnapshot.ReplicatedFields{
			Configuration: map[string]any{"settings": map[string]any{}},
			Secrets:       []replicationsnapshot.ReplicatedSecret{},
			Owners:        []replicationsnapshot.ReplicatedOwner{},
		},
	}
	var err error
	env.ContentHash, err = env.ExpectedHash()
	if err != nil {
		t.Fatalf("Envelope.ExpectedHash: %v", err)
	}
	return env
}
