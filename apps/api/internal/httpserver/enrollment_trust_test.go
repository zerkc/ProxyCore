package httpserver_test

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/zerkc/ProxyCore/apps/api/internal/auth"
	"github.com/zerkc/ProxyCore/apps/api/internal/config"
	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/httpserver"
	"github.com/zerkc/ProxyCore/apps/api/internal/identity"
)

const (
	enrollmentTrustPath       = httpserver.EnrollmentTrustPath
	enrollmentTrustCAPEM      = httpserver.EnrollmentTrustCAPEMPath
	enrollmentTrustAliasPath  = "/api/settings/enrollment-trust"
	enrollmentTrustAliasCAPEM = enrollmentTrustAliasPath + "/ca"
)

type enrollmentTrustResponse struct {
	Status           string    `json:"status"`
	Configured       bool      `json:"configured"`
	Ready            bool      `json:"ready"`
	CertificatePEM   string    `json:"certificatePem"`
	CACertificatePEM string    `json:"caCertificatePem"`
	CADERHashSHA256  string    `json:"caDerSha256"`
	ExpiresAt        time.Time `json:"expiresAt"`
}

func assertTrustState(t *testing.T, response *httptest.ResponseRecorder, status string, configured, ready bool) {
	t.Helper()
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" || response.Header().Get("Content-Type") != "application/json; charset=utf-8" {
		t.Fatalf("trust status/headers=%d %q %q body=%s", response.Code, response.Header().Get("Cache-Control"), response.Header().Get("Content-Type"), response.Body.String())
	}
	var body enrollmentTrustResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode trust state: %v", err)
	}
	if body.Status != status || body.Configured != configured || body.Ready != ready {
		t.Fatalf("trust state=%+v, want status=%q configured=%v ready=%v", body, status, configured, ready)
	}
	if !ready && (body.CertificatePEM != "" || body.CACertificatePEM != "" || body.CADERHashSHA256 != "" || !body.ExpiresAt.IsZero()) {
		t.Fatalf("not-ready response exposed material=%+v", body)
	}
}

type establishedTrustFixture struct {
	server   *httpserver.Server
	pool     *pgxpool.Pool
	store    *configuration.Store
	auth     *auth.Service
	identity *identity.Service
	material configuration.EnrollmentTLSMaterial
}

func newEstablishedTrustFixture(t *testing.T, role domain.TopologyRole) *establishedTrustFixture {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping database-backed enrollment trust handler tests in short mode")
	}
	dsn := os.Getenv("DATABASE_URL")
	if strings.TrimSpace(dsn) == "" {
		t.Skip("DATABASE_URL is not set")
	}
	ctx := context.Background()
	adminPool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect admin db: %v", err)
	}
	t.Cleanup(adminPool.Close)
	schema := "proxycore_go_trust_test_" + regexp.MustCompile(`[^a-z0-9]+`).ReplaceAllString(strings.ToLower(t.Name()), "_") + "_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	if _, err := adminPool.Exec(ctx, "create schema "+(pgx.Identifier{schema}).Sanitize()); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		_, _ = adminPool.Exec(context.Background(), "drop schema if exists "+(pgx.Identifier{schema}).Sanitize()+" cascade")
	})
	poolCfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse db config: %v", err)
	}
	if poolCfg.ConnConfig.RuntimeParams == nil {
		poolCfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	poolCfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		t.Fatalf("connect schema db: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := auth.NewPostgresStore(pool).EnsureSchema(ctx); err != nil {
		t.Fatalf("ensure auth schema: %v", err)
	}
	if err := configuration.EnsureSchema(ctx, pool); err != nil {
		t.Fatalf("ensure configuration schema: %v", err)
	}
	const masterKey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	store := configuration.New(pool, masterKey, domain.Ingress{})
	if _, err := store.UpdateEnrollmentHostnames(ctx, []string{"enroll.example"}); err != nil {
		t.Fatalf("configure enrollment hostnames: %v", err)
	}
	material, err := store.EnsureEnrollmentTLSMaterial(ctx)
	if err != nil {
		t.Fatalf("issue enrollment material: %v", err)
	}
	authSvc := auth.NewService(auth.NewPostgresStore(pool), auth.ServiceOptions{SessionTTL: time.Hour})
	identitySvc := identity.NewService(identity.NewPgStore(pool))
	if _, _, err := identitySvc.EnsureBootstrapped(ctx); err != nil {
		t.Fatalf("bootstrap identity: %v", err)
	}
	switch role {
	case domain.TopologyRoleNode:
		if _, err := identitySvc.TransitionTo(ctx, role); err != nil {
			t.Fatalf("transition identity: %v", err)
		}
	case domain.TopologyRoleStalePrimary:
		if _, err := identitySvc.RecordImportedSnapshot(ctx, uuid.New(), 2); err != nil {
			t.Fatalf("mark identity stale: %v", err)
		}
	case domain.TopologyRolePrimary, domain.TopologyRolePrimaryWithNodes:
		if err := identity.NewPgStore(pool).UpdateRole(ctx, role); err != nil {
			t.Fatalf("set identity role %s: %v", role, err)
		}
		if _, err := identitySvc.Load(ctx); err != nil {
			t.Fatalf("reload identity role %s: %v", role, err)
		}
	case domain.TopologyRoleStandalone:
	default:
		t.Fatalf("unsupported test role %s", role)
	}
	return &establishedTrustFixture{
		server:   httpserver.New(config.Config{UIDist: t.TempDir(), SessionCookieName: "proxycore_session", SessionTTL: time.Hour}, log.New(io.Discard, "", 0), httpserver.WithAuthService(authSvc), httpserver.WithConfigurationStore(store), httpserver.WithIdentityService(identitySvc)),
		pool:     pool,
		store:    store,
		auth:     authSvc,
		identity: identitySvc,
		material: material,
	}
}

func (f *establishedTrustFixture) serverWithUnloadedIdentity() *httpserver.Server {
	return httpserver.New(config.Config{UIDist: "", SessionCookieName: "proxycore_session", SessionTTL: time.Hour}, log.New(io.Discard, "", 0), httpserver.WithAuthService(f.auth), httpserver.WithConfigurationStore(f.store), httpserver.WithIdentityService(identity.NewService(identity.NewPgStore(f.pool))))
}

type trustPersistenceState struct {
	SettingsUpdatedAt time.Time
	CAUpdatedAt       time.Time
	SecretCount       int
	StateCount        int
}

func trustDatabaseState(t *testing.T, pool *pgxpool.Pool) trustPersistenceState {
	t.Helper()
	var state trustPersistenceState
	if err := pool.QueryRow(t.Context(), `select updated_at from installation_settings where id = $1`, "default").Scan(&state.SettingsUpdatedAt); err != nil {
		t.Fatalf("read settings timestamp: %v", err)
	}
	if err := pool.QueryRow(t.Context(), `select updated_at from internal_ca where id = $1`, "default").Scan(&state.CAUpdatedAt); err != nil {
		t.Fatalf("read CA timestamp: %v", err)
	}
	if err := pool.QueryRow(t.Context(), `select count(*) from secrets`).Scan(&state.SecretCount); err != nil {
		t.Fatalf("count secrets: %v", err)
	}
	if err := pool.QueryRow(t.Context(), `select count(*) from internal_ca_enrollment_state`).Scan(&state.StateCount); err != nil {
		t.Fatalf("count enrollment state: %v", err)
	}
	return state
}
