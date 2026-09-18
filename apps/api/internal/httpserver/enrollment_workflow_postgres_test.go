package httpserver_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zerkc/ProxyCore/apps/api/internal/auth"
	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/enrollment"
	"github.com/zerkc/ProxyCore/apps/api/internal/httpserver"
	"github.com/zerkc/ProxyCore/apps/api/internal/identity"
	replicationsnapshot "github.com/zerkc/ProxyCore/apps/api/internal/snapshot"
)

func TestPostgresEnrollmentWorkflowDraftValidatesAndCachesRedactedEnvelope(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping PostgreSQL enrollment workflow test in short mode")
	}
	dsn := os.Getenv("DATABASE_URL")
	if strings.TrimSpace(dsn) == "" {
		t.Skip("DATABASE_URL is not set")
	}
	ctx := context.Background()
	adminPool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer adminPool.Close()
	schema := "proxycore_go_pne6_workflow_" + regexp.MustCompile(`[^a-z0-9]+`).ReplaceAllString(strings.ToLower(t.Name()), "_") + "_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	if _, err := adminPool.Exec(ctx, "create schema "+(pgx.Identifier{schema}).Sanitize()); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = adminPool.Exec(context.Background(), "drop schema if exists "+(pgx.Identifier{schema}).Sanitize()+" cascade")
	}()
	poolConfig, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if poolConfig.ConnConfig.RuntimeParams == nil {
		poolConfig.ConnConfig.RuntimeParams = map[string]string{}
	}
	poolConfig.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := auth.NewPostgresStore(pool).EnsureSchema(ctx); err != nil {
		t.Fatal(err)
	}
	if err := configuration.EnsureSchema(ctx, pool); err != nil {
		t.Fatal(err)
	}
	identityService := identity.NewService(identity.NewPgStore(pool))
	if _, _, err := identityService.EnsureBootstrapped(ctx); err != nil {
		t.Fatal(err)
	}
	authService := auth.NewService(auth.NewPostgresStore(pool), auth.ServiceOptions{SessionTTL: time.Hour})
	owner, err := authService.Bootstrap(ctx, "owner", "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	phase2 := configuration.NewPhase2Store(pool)
	authority := httpserver.NewEnrollmentTokenAuthority(phase2, identityService, httpserver.EnrollmentTokenAuthorityOptions{})
	created, err := authority.Create(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	if err := authority.Verify(ctx, created.Token); err != nil {
		t.Fatalf("token verification before workflow: %v", err)
	}
	cache := enrollment.NewDraftCache(enrollment.DraftCacheOptions{})
	envelope := workflowTestEnvelope(t)
	handler := httpserver.NewEnrollmentIdentityMux(httpserver.EnrollmentIdentityHandlerOptions{
		Certificate: func(context.Context) (*x509.Certificate, error) {
			return &x509.Certificate{DNSNames: []string{"primary.example"}}, nil
		},
		Workflow: &httpserver.EnrollmentWorkflowHandlerOptions{
			Cache: cache, VerifyToken: authority.Verify,
			FetchSnapshot: func(context.Context, string, string) (*replicationsnapshot.Envelope, error) { return envelope, nil },
			Validator: snapshotValidatorFunc(func(context.Context, replicationsnapshot.Envelope) replicationsnapshot.ValidationResult {
				return replicationsnapshot.ValidationResult{OK: true}
			}),
			Identity: func(context.Context) (httpserver.EnrollmentLocalIdentity, error) {
				return httpserver.EnrollmentLocalIdentity{InstallationID: domain.NewInstallationID(), NodeID: domain.NewNodeID()}, nil
			},
		},
	})
	body, _ := json.Marshal(map[string]string{"token": created.Token, "primaryURL": "https://primary.example:3443/"})
	request := httptest.NewRequest(http.MethodPost, httpserver.EnrollmentDraftPath, bytes.NewReader(body))
	request.Host = "primary.example:3443"
	request.TLS = &tls.ConnectionState{Version: tls.VersionTLS13, HandshakeComplete: true, ServerName: "primary.example"}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), created.Token) || strings.Contains(response.Body.String(), "secrets") {
		t.Fatalf("workflow status/body=%d %s", response.Code, response.Body.String())
	}
}
