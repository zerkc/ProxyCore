package httpserver_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
)

func topologyReadOnlyCode(t *testing.T, body []byte) string {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		return ""
	}
	code, _ := decoded["code"].(string)
	return code
}

func TestNodeIdentityBlocksOrdinaryMutations(t *testing.T) {
	env := newSQLiteHTTPTestServer(t)
	cookie := sqliteOwnerCookie(t, env.Handler)
	moveIdentityToRole(t, env, domain.TopologyRoleNode)

	cases := []struct {
		name   string
		method string
		target string
		body   any
	}{
		{"settings", http.MethodPut, "/api/settings", map[string]any{"ingress": map[string]any{"ipv4": "192.168.1.11"}}},
		{"create user", http.MethodPost, "/api/users", map[string]any{"username": "x", "password": "long-enough-password", "role": "operator"}},
		{"delete user", http.MethodDelete, "/api/users/abc", nil},
		{"create zone", http.MethodPost, "/api/zones", map[string]any{"name": "example.com"}},
		{"add record", http.MethodPost, "/api/zones/z1/records", map[string]any{"name": "a"}},
		{"create stream", http.MethodPost, "/api/streams", map[string]any{"name": "s"}},
		{"issue certificate", http.MethodPost, "/api/certificates", map[string]any{"domains": []string{"example.com"}}},
		{"delete certificate", http.MethodDelete, "/api/certificates/c1", nil},
		{"apply", http.MethodPost, "/api/apply", nil},
		{"backup import", http.MethodPost, "/api/backup/import?dry_run=true", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			response := doJSON(t, env.Handler, tc.method, tc.target, tc.body, cookie)
			if response.Code != http.StatusForbidden || topologyReadOnlyCode(t, response.Body.Bytes()) != "TOPOLOGY_READ_ONLY" {
				t.Fatalf("%s status=%d body=%s", tc.name, response.Code, response.Body.String())
			}
		})
	}
}

func TestNodeIdentityAllowsUpdateApplyAndBackupExport(t *testing.T) {
	env := newSQLiteHTTPTestServer(t)
	cookie := sqliteOwnerCookie(t, env.Handler)
	moveIdentityToRole(t, env, domain.TopologyRoleNode)

	for _, target := range []string{"/api/updates/apply", "/api/backup/export"} {
		response := doJSON(t, env.Handler, http.MethodPost, target, map[string]any{"targetVersion": "1.0.0"}, cookie)
		if topologyReadOnlyCode(t, response.Body.Bytes()) == "TOPOLOGY_READ_ONLY" {
			t.Fatalf("%s must not be blocked on a node: status=%d body=%s", target, response.Code, response.Body.String())
		}
		if response.Code == http.StatusUnauthorized || response.Code == http.StatusForbidden {
			t.Fatalf("%s owner request rejected: status=%d body=%s", target, response.Code, response.Body.String())
		}
	}
}

func TestNodeIdentityReadsDoNotInitializeIngress(t *testing.T) {
	env := newSQLiteHTTPTestServer(t)
	cookie := sqliteOwnerCookie(t, env.Handler)
	moveIdentityToRole(t, env, domain.TopologyRoleNode)

	before, err := env.Config.GetSettings(context.Background())
	if err != nil {
		t.Fatalf("settings before: %v", err)
	}
	for _, target := range []string{"/api/settings", "/api/zones", "/api/streams", "/api/users", "/api/certificates"} {
		response := doJSON(t, env.Handler, http.MethodGet, target, nil, cookie)
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s on node status=%d body=%s", target, response.Code, response.Body.String())
		}
	}
	after, err := env.Config.GetSettings(context.Background())
	if err != nil {
		t.Fatalf("settings after: %v", err)
	}
	beforeJSON, _ := json.Marshal(before)
	afterJSON, _ := json.Marshal(after)
	if string(beforeJSON) != string(afterJSON) {
		t.Fatalf("node GETs changed settings: before=%s after=%s", beforeJSON, afterJSON)
	}
}
