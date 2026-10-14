package httpserver_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/httpserver"
	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

// seedEnrolledNode inserts one active enrolled node with its credential.
func seedEnrolledNode(t *testing.T, env *sqliteHTTPEnv) {
	t.Helper()
	writer := sqlitedb.NewWriter(env.DB, nil)
	err := writer.Write(context.Background(), func(v sqlitedb.WriteValue) error {
		stmts := []struct {
			q string
			a []any
		}{
			{`insert into node_credentials (id, node_id, credential_hash, hash_version, created_at) values (?, ?, 'h', 'v1', ?)`,
				[]any{"dddddddd-1111-4111-8111-111111111111", "eeeeeeee-1111-4111-8111-111111111111", v.Timestamp()}},
			{`insert into enrolled_nodes (node_id, installation_id, primary_id, credential_id, enrolled_at, created_by_attempt_id)
			  values (?, ?, ?, ?, ?, ?)`,
				[]any{"eeeeeeee-1111-4111-8111-111111111111", "ffffffff-1111-4111-8111-111111111111",
					"99999999-1111-4111-8111-111111111111", "dddddddd-1111-4111-8111-111111111111", v.Timestamp(),
					"88888888-1111-4111-8111-111111111111"}},
		}
		for _, s := range stmts {
			if _, err := v.Conn().ExecContext(v.Context(), s.q, s.a...); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed enrolled node: %v", err)
	}
}

func activeCredentials(t *testing.T, env *sqliteHTTPEnv) int {
	t.Helper()
	var n int
	if err := env.DB.QueryRow(`select count(*) from node_credentials where revoked_at is null`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return body
}

func TestTopologyPromoteRequiresOwnerAndConfirmation(t *testing.T) {
	env := newSQLiteHTTPTestServer(t)
	owner := sqliteOwnerCookie(t, env.Handler)
	if rec := doJSON(t, env.Handler, http.MethodPost, "/api/users", map[string]any{
		"username": "operator", "password": "operator horse battery staple", "role": "operator",
	}, owner); rec.Code != http.StatusCreated {
		t.Fatalf("create operator status=%d body=%s", rec.Code, rec.Body.String())
	}
	login := doJSON(t, env.Handler, http.MethodPost, "/api/auth/login", map[string]any{
		"username": "operator", "password": "operator horse battery staple",
	}, nil)
	var operator *http.Cookie
	if cookies := login.Result().Cookies(); len(cookies) > 0 {
		operator = cookies[0]
	}
	if _, err := env.Identity.TransitionTo(context.Background(), domain.TopologyRoleNode); err != nil {
		t.Fatal(err)
	}

	if rec := doJSON(t, env.Handler, http.MethodPost, httpserver.TopologyPromotePath, map[string]any{"confirm": true}, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous status=%d", rec.Code)
	}
	if rec := doJSON(t, env.Handler, http.MethodPost, httpserver.TopologyPromotePath, map[string]any{"confirm": true}, operator); rec.Code != http.StatusForbidden {
		t.Fatalf("non-owner status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := doJSON(t, env.Handler, http.MethodPost, httpserver.TopologyPromotePath, map[string]any{}, owner); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing confirm status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := doJSON(t, env.Handler, http.MethodPost, httpserver.TopologyPromotePath, map[string]any{"confirm": false}, owner); rec.Code != http.StatusBadRequest {
		t.Fatalf("false confirm status=%d", rec.Code)
	}
	if got := env.Identity.Current().Role; got != domain.TopologyRoleNode {
		t.Fatalf("role changed by refused requests: %s", got)
	}
}

func TestTopologyPromoteBumpsGenerationRevokesNodesAndWarns(t *testing.T) {
	env := newSQLiteHTTPTestServer(t)
	owner := sqliteOwnerCookie(t, env.Handler)
	seedEnrolledNode(t, env)
	if _, err := env.Identity.TransitionTo(context.Background(), domain.TopologyRoleNode); err != nil {
		t.Fatal(err)
	}
	generation := uint64(env.Identity.Current().LeadershipGeneration)

	rec := doJSON(t, env.Handler, http.MethodPost, httpserver.TopologyPromotePath, map[string]any{
		"confirm": true, "expectedGeneration": generation,
	}, owner)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := decodeBody(t, rec)
	identity, _ := body["identity"].(map[string]any)
	if identity["role"] != "primary" || uint64(identity["leadershipGeneration"].(float64)) != generation+1 {
		t.Fatalf("identity=%v", identity)
	}
	warning, _ := body["warning"].(map[string]any)
	if warning["code"] != "SPLIT_BRAIN_RISK" || warning["message"] == "" {
		t.Fatalf("warning=%v", body["warning"])
	}
	if body["nodesMustReenroll"] != true {
		t.Fatalf("nodesMustReenroll=%v", body["nodesMustReenroll"])
	}
	if activeCredentials(t, env) != 0 {
		t.Fatal("old node credentials were not revoked")
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("cache-control=%q", rec.Header().Get("Cache-Control"))
	}
}

func TestTopologyPromoteRejectsStaleExpectedGeneration(t *testing.T) {
	env := newSQLiteHTTPTestServer(t)
	owner := sqliteOwnerCookie(t, env.Handler)
	seedEnrolledNode(t, env)
	if _, err := env.Identity.TransitionTo(context.Background(), domain.TopologyRoleNode); err != nil {
		t.Fatal(err)
	}
	stale := uint64(env.Identity.Current().LeadershipGeneration) + 5
	rec := doJSON(t, env.Handler, http.MethodPost, httpserver.TopologyPromotePath, map[string]any{
		"confirm": true, "expectedGeneration": stale,
	}, owner)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if body := decodeBody(t, rec); body["code"] != "GENERATION_MISMATCH" {
		t.Fatalf("body=%v", body)
	}
	if env.Identity.Current().Role != domain.TopologyRoleNode || activeCredentials(t, env) != 1 {
		t.Fatal("state changed by a refused promotion")
	}
}

func TestTopologyPromoteRefusedForWritablePrimary(t *testing.T) {
	env := newSQLiteHTTPTestServer(t)
	owner := sqliteOwnerCookie(t, env.Handler)
	if _, err := env.Identity.TransitionTo(context.Background(), domain.TopologyRoleNode); err != nil {
		t.Fatal(err)
	}
	if _, err := env.Identity.PromoteToPrimary(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec := doJSON(t, env.Handler, http.MethodPost, httpserver.TopologyPromotePath, map[string]any{"confirm": true}, owner)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestTopologyDemoteRevokesNodesAndEntersNodeRole(t *testing.T) {
	env := newSQLiteHTTPTestServer(t)
	owner := sqliteOwnerCookie(t, env.Handler)
	if _, err := env.Identity.TransitionTo(context.Background(), domain.TopologyRoleNode); err != nil {
		t.Fatal(err)
	}
	if _, err := env.Identity.PromoteToPrimary(context.Background()); err != nil {
		t.Fatal(err)
	}
	seedEnrolledNode(t, env)
	if activeCredentials(t, env) != 1 {
		t.Fatal("expected one active node credential before demotion")
	}

	if rec := doJSON(t, env.Handler, http.MethodPost, httpserver.TopologyDemotePath, map[string]any{}, owner); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing confirm status=%d body=%s", rec.Code, rec.Body.String())
	}
	if env.Identity.Current().Role != domain.TopologyRolePrimary {
		t.Fatal("role changed without confirmation")
	}

	rec := doJSON(t, env.Handler, http.MethodPost, httpserver.TopologyDemotePath, map[string]any{"confirm": true}, owner)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := decodeBody(t, rec)
	identity, _ := body["identity"].(map[string]any)
	if identity["role"] != "node" || identity["writable"] != false {
		t.Fatalf("identity=%v", identity)
	}
	if env.Identity.Current().Role != domain.TopologyRoleNode {
		t.Fatalf("role=%s", env.Identity.Current().Role)
	}
	if activeCredentials(t, env) != 0 {
		t.Fatal("node credentials were not revoked on demotion")
	}
	// A second demotion is an invalid transition from NODE.
	if again := doJSON(t, env.Handler, http.MethodPost, httpserver.TopologyDemotePath, map[string]any{"confirm": true}, owner); again.Code != http.StatusConflict {
		t.Fatalf("second demote status=%d", again.Code)
	}
}

func TestTopologyDemoteRequiresOwner(t *testing.T) {
	env := newSQLiteHTTPTestServer(t)
	if rec := doJSON(t, env.Handler, http.MethodPost, httpserver.TopologyDemotePath, map[string]any{"confirm": true}, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous status=%d", rec.Code)
	}
}
