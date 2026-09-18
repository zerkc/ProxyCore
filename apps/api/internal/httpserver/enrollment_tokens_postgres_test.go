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

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zerkc/ProxyCore/apps/api/internal/auth"
	"github.com/zerkc/ProxyCore/apps/api/internal/config"
	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
	"github.com/zerkc/ProxyCore/apps/api/internal/httpserver"
	"github.com/zerkc/ProxyCore/apps/api/internal/identity"
)

func TestPostgresEnrollmentTokenHTTPWorkflowPersistsOnlyHashAndListsRedactedMetadata(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping PostgreSQL enrollment token test in short mode")
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
	schema := "proxycore_go_pne6_token_" + regexp.MustCompile(`[^a-z0-9]+`).ReplaceAllString(strings.ToLower(t.Name()), "_") + "_" + strconv.FormatInt(time.Now().UnixNano(), 36)
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
	authStore := auth.NewPostgresStore(pool)
	if err := authStore.EnsureSchema(ctx); err != nil {
		t.Fatal(err)
	}
	if err := configuration.EnsureSchema(ctx, pool); err != nil {
		t.Fatal(err)
	}
	identityService := identity.NewService(identity.NewPgStore(pool))
	if _, _, err := identityService.EnsureBootstrapped(ctx); err != nil {
		t.Fatal(err)
	}
	authService := auth.NewService(authStore, auth.ServiceOptions{SessionTTL: time.Hour})
	owner, err := authService.Bootstrap(ctx, "owner", "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	session, err := authService.Login(ctx, owner.Username, "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	phase2 := configuration.NewPhase2StoreWithMasterKey(pool, strings.Repeat("A", 43))
	authority := httpserver.NewEnrollmentTokenAuthority(phase2, identityService, httpserver.EnrollmentTokenAuthorityOptions{
		List: func(ctx context.Context, ownerID string) ([]httpserver.EnrollmentTokenRecord, error) {
			rows, err := pool.Query(ctx, `select id::text, token_selector, created_at, expires_at, consumed_at, revoked_at from enrollment_tokens where created_by_user_id = $1 order by created_at`, ownerID)
			if err != nil {
				return nil, err
			}
			defer rows.Close()
			var result []httpserver.EnrollmentTokenRecord
			for rows.Next() {
				var record httpserver.EnrollmentTokenRecord
				if err := rows.Scan(&record.ID, &record.Selector, &record.CreatedAt, &record.ExpiresAt, &record.ConsumedAt, &record.RevokedAt); err != nil {
					return nil, err
				}
				result = append(result, record)
			}
			return result, rows.Err()
		},
	})
	srv := httpserver.New(config.Config{UIDist: t.TempDir(), SessionCookieName: "proxycore_session", SessionTTL: time.Hour}, log.New(io.Discard, "", 0), httpserver.WithAuthService(authService), httpserver.WithEnrollmentTokenAuthority(authority))
	cookie := &http.Cookie{Name: "proxycore_session", Value: session.Token}
	createRequest := httptest.NewRequest(http.MethodPost, httpserver.EnrollmentTokensPath, strings.NewReader(`{}`))
	createRequest.AddCookie(cookie)
	createdResponse := httptest.NewRecorder()
	srv.Handler().ServeHTTP(createdResponse, createRequest)
	if createdResponse.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", createdResponse.Code, createdResponse.Body.String())
	}
	var created httpserver.EnrollmentTokenCreation
	if err := json.Unmarshal(createdResponse.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	var storedSelector, storedHash string
	if err := pool.QueryRow(ctx, `select token_selector, token_hash from enrollment_tokens where id = $1`, created.ID).Scan(&storedSelector, &storedHash); err != nil {
		t.Fatal(err)
	}
	if storedSelector != created.Selector || storedHash == "" || strings.Contains(storedHash, created.Token) || strings.Contains(storedHash, "PRIVATE") {
		t.Fatalf("stored token projection selector=%q hash=%q", storedSelector, storedHash)
	}
	listRequest := httptest.NewRequest(http.MethodGet, httpserver.EnrollmentTokensPath, nil)
	listRequest.AddCookie(cookie)
	listResponse := httptest.NewRecorder()
	srv.Handler().ServeHTTP(listResponse, listRequest)
	if listResponse.Code != http.StatusOK || strings.Contains(listResponse.Body.String(), created.Token) {
		t.Fatalf("list status/body=%d %s", listResponse.Code, listResponse.Body.String())
	}
	revokeRequest := httptest.NewRequest(http.MethodPost, httpserver.EnrollmentTokensPath+"/"+created.ID+"/revoke?confirm=replace-and-revoke", nil)
	revokeRequest.AddCookie(cookie)
	revokeResponse := httptest.NewRecorder()
	srv.Handler().ServeHTTP(revokeResponse, revokeRequest)
	if revokeResponse.Code != http.StatusNoContent {
		t.Fatalf("revoke status/body=%d %s", revokeResponse.Code, revokeResponse.Body.String())
	}
	var revokedAt *time.Time
	if err := pool.QueryRow(ctx, `select revoked_at from enrollment_tokens where id = $1`, created.ID).Scan(&revokedAt); err != nil {
		t.Fatal(err)
	}
	if revokedAt == nil {
		t.Fatal("revoke did not persist lifecycle metadata")
	}
}
