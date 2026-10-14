package httpserver_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/httpserver"
)

// enrollFixtureNode inserts one live credential and enrolled node row.
func enrollFixtureNode(t *testing.T, env *sqliteHTTPEnv) (nodeID string) {
	t.Helper()
	nodeID = uuid.NewString()
	credential := uuid.NewString()
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`insert into node_credentials(id,node_id,credential_hash,hash_version,created_at) values(?,?,'synthetic','fixture','0')`, []any{credential, nodeID}},
		{`insert into enrolled_nodes(node_id,installation_id,primary_id,credential_id,enrolled_at,created_by_attempt_id) values(?,?,?,?,'0',?)`,
			[]any{nodeID, uuid.NewString(), uuid.NewString(), credential, uuid.NewString()}},
	} {
		if _, err := env.DB.Exec(statement.sql, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	return nodeID
}

func TestUnlinkNodeRouteOnPrimary(t *testing.T) {
	env := newSQLiteHTTPTestServer(t)
	cookie := sqliteOwnerCookie(t, env.Handler)
	// Creating the first enrollment token promotes the identity to primary.
	if rec := doJSON(t, env.Handler, http.MethodPost, httpserver.EnrollmentTokensPath, map[string]any{}, cookie); rec.Code != http.StatusCreated {
		t.Fatalf("promote via token status=%d body=%s", rec.Code, rec.Body.String())
	}
	node := enrollFixtureNode(t, env)

	rec := doJSON(t, env.Handler, http.MethodPost, httpserver.TopologyNodesPath+"/"+node+"/unlink", nil, cookie)
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("unlink status=%d headers=%v body=%s", rec.Code, rec.Header(), rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["nodeId"] != node || body["unlinkedAt"] == nil {
		t.Fatalf("body=%s err=%v", rec.Body.String(), err)
	}
	var unlinked, revoked, credentialRevoked *string
	if err := env.DB.QueryRow(`select n.unlinked_at, n.revoked_at, c.revoked_at from enrolled_nodes n join node_credentials c on c.id = n.credential_id where n.node_id = ?`, node).
		Scan(&unlinked, &revoked, &credentialRevoked); err != nil || unlinked == nil || revoked == nil || credentialRevoked == nil {
		t.Fatalf("rows not marked: %v %v %v err=%v", unlinked, revoked, credentialRevoked, err)
	}

	if again := doJSON(t, env.Handler, http.MethodPost, httpserver.TopologyNodesPath+"/"+node+"/unlink", nil, cookie); again.Code != http.StatusOK {
		t.Fatalf("idempotent unlink status=%d", again.Code)
	}
	if missing := doJSON(t, env.Handler, http.MethodPost, httpserver.TopologyNodesPath+"/"+uuid.NewString()+"/unlink", nil, cookie); missing.Code != http.StatusNotFound {
		t.Fatalf("unknown node status=%d", missing.Code)
	}
	if bad := doJSON(t, env.Handler, http.MethodPost, httpserver.TopologyNodesPath+"/not-a-uuid/unlink", nil, cookie); bad.Code != http.StatusNotFound {
		t.Fatalf("malformed node status=%d", bad.Code)
	}
	if anonymous := doJSON(t, env.Handler, http.MethodPost, httpserver.TopologyNodesPath+"/"+node+"/unlink", nil, nil); anonymous.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous status=%d", anonymous.Code)
	}
}

func TestUnlinkNodeRouteRejectsNonPrimaryRoles(t *testing.T) {
	env := newSQLiteHTTPTestServer(t)
	cookie := sqliteOwnerCookie(t, env.Handler)
	node := enrollFixtureNode(t, env)
	target := httpserver.TopologyNodesPath + "/" + node + "/unlink"

	// STANDALONE has no nodes to unlink.
	if rec := doJSON(t, env.Handler, http.MethodPost, target, nil, cookie); rec.Code != http.StatusConflict {
		t.Fatalf("standalone status=%d body=%s", rec.Code, rec.Body.String())
	}
	moveIdentityToRole(t, env, domain.TopologyRoleNode)
	rec := doJSON(t, env.Handler, http.MethodPost, target, nil, cookie)
	if rec.Code != http.StatusForbidden || topologyReadOnlyCode(t, rec.Body.Bytes()) != "TOPOLOGY_READ_ONLY" {
		t.Fatalf("node status=%d body=%s", rec.Code, rec.Body.String())
	}
	var unlinked *string
	if err := env.DB.QueryRow(`select unlinked_at from enrolled_nodes where node_id = ?`, node).Scan(&unlinked); err != nil || unlinked != nil {
		t.Fatalf("rejected unlink mutated the node: %v err=%v", unlinked, err)
	}
}
