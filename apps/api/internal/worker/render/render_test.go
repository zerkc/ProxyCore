package render_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/zerkc/ProxyCore/apps/api/internal/renderers"
	"github.com/zerkc/ProxyCore/apps/api/internal/secrets"
	"github.com/zerkc/ProxyCore/apps/api/internal/worker/control"
	"github.com/zerkc/ProxyCore/apps/api/internal/worker/render"
	"github.com/zerkc/ProxyCore/apps/api/internal/worker/store"
	"github.com/zerkc/ProxyCore/apps/api/internal/worker/testsupport"
)

const root = "/var/lib/proxycore/candidates"

func job(target store.Target) store.Job {
	return store.Job{ID: "job-1", RevisionID: "revision1", Target: target, CorrelationID: "c-1"}
}

func snapshot(zoneEnabled bool, proxy string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{
	  "settings": {"ingress": {"ipv4": "192.0.2.10"},
	    "defaultPool": {"id": "default", "endpoints": [{"host": "1.1.1.1", "port": 53}]},
	    "forwardingRules": [], "retentionMaxAgeDays": 7, "retentionMaxSizeMb": 50},
	  "zones": [{"id": "zone-1", "name": "example.test", "enabled": %t, "records": [
	    {"id": "record-1", "name": "app.example.test", "type": "A", "value": "192.0.2.20", "ttl": 300,
	     "enabled": true, "proxied": true, "proxy": %s}]}],
	  "streams": [],
	  "certificates": [{"id": "cert-1", "hostnames": ["app.example.test"], "issuer": "self-signed",
	    "challenge": "none", "environment": "staging", "status": "active", "secretId": "KEYSECRET",
	    "certificatePem": "-----BEGIN CERTIFICATE-----\nTEST\n-----END CERTIFICATE-----\n"}]}`, zoneEnabled, proxy))
}

func plainProxy() string {
	return `{"origin": {"ip": "192.0.2.20", "port": 8080, "protocol": "http"}, "tlsEnabled": false}`
}

func secretStore(t *testing.T) (*secrets.SQLiteStore, string, string) {
	t.Helper()
	db, _ := testsupport.NewDB(t)
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	store := secrets.NewSQLiteStore(db, base64.StdEncoding.EncodeToString(key))
	hash, err := secrets.HashBasicAuthPassword("homelab-pass")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	passwordID, err := store.Put(ctx, "basic-auth-password", hash)
	if err != nil {
		t.Fatal(err)
	}
	keyID, err := store.Put(ctx, "certificate-private-key", "-----BEGIN PRIVATE KEY-----\nTEST\n-----END PRIVATE KEY-----\n")
	if err != nil {
		t.Fatal(err)
	}
	return store, passwordID, keyID
}

func TestNginxCandidateIncludesBasicAuthAndTLSFiles(t *testing.T) {
	secretsStore, passwordID, keyID := secretStore(t)
	proxy := fmt.Sprintf(`{"origin": {"ip": "192.0.2.20", "port": 8080, "protocol": "http"}, "tlsEnabled": true,
	  "certificateId": "cert-1", "basicAuth": {"username": "operator", "passwordSecretId": %q}}`, passwordID)
	raw := strings.ReplaceAll(string(snapshot(true, proxy)), "KEYSECRET", keyID)
	candidates, err := render.Candidates(context.Background(), json.RawMessage(raw), job(store.TargetNginx),
		render.Options{CandidateRoot: root, Secrets: secretsStore})
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].Service != control.ServiceNginx {
		t.Fatalf("candidates = %+v", candidates)
	}
	nginx := candidates[0]
	if nginx.CandidatePath != root+"/revision1/nginx" || nginx.Checksum == "" {
		t.Fatalf("candidate = %+v", nginx)
	}
	if got := nginx.Files["basic-auth/"+passwordID]; !strings.HasPrefix(got, "operator:{SHA}") {
		t.Fatalf("basic auth file = %q", got)
	}
	if !strings.Contains(nginx.Files["certs/cert-1.crt"], "BEGIN CERTIFICATE") || !strings.Contains(nginx.Files["certs/cert-1.key"], "BEGIN PRIVATE KEY") {
		t.Fatalf("cert files = %q / %q", nginx.Files["certs/cert-1.crt"], nginx.Files["certs/cert-1.key"])
	}
	conf := nginx.Files["nginx.conf"]
	for _, want := range []string{
		root + "/revision1/nginx/basic-auth/" + passwordID,
		root + "/revision1/nginx/certs/cert-1.crt",
	} {
		if !strings.Contains(conf, want) {
			t.Fatalf("nginx.conf lacks %q", want)
		}
	}
	all, _ := json.Marshal(nginx.Files)
	if strings.Contains(string(all), "homelab-pass") {
		t.Fatal("the plaintext password reached the candidate")
	}
}

func TestNginxFailsClosedWithoutASecretStore(t *testing.T) {
	basicAuth := `{"origin": {"ip": "192.0.2.20", "port": 8080, "protocol": "http"}, "tlsEnabled": false,
	  "basicAuth": {"username": "operator", "passwordSecretId": "secret-missing"}}`
	tls := `{"origin": {"ip": "192.0.2.20", "port": 8080, "protocol": "http"}, "tlsEnabled": true, "certificateId": "cert-1"}`
	for name, tt := range map[string]struct{ proxy, want string }{
		"basic auth": {basicAuth, "Basic Auth secrets require a configured secret store"},
		"tls":        {tls, "TLS certificates require a configured secret store"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := render.Candidates(context.Background(), snapshot(true, tt.proxy), job(store.TargetNginx), render.Options{})
			if err == nil || err.Error() != tt.want {
				t.Fatalf("err = %v", err)
			}
		})
	}
	// Records without secrets render fine without a store.
	if _, err := render.Candidates(context.Background(), snapshot(true, plainProxy()), job(store.TargetNginx), render.Options{}); err != nil {
		t.Fatal(err)
	}
}

func TestCombinedRendersCoreDNSThenNginxUnderTheCandidateRoot(t *testing.T) {
	candidates, err := render.Candidates(context.Background(), snapshot(true, plainProxy()), job(store.TargetCombined),
		render.Options{CandidateRoot: root + "//"})
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 2 || candidates[0].Service != control.ServiceCoreDNS || candidates[1].Service != control.ServiceNginx {
		t.Fatalf("candidates = %+v", candidates)
	}
	if candidates[0].CandidatePath != root+"/revision1/coredns" || candidates[1].CandidatePath != root+"/revision1/nginx" {
		t.Fatalf("paths = %q %q", candidates[0].CandidatePath, candidates[1].CandidatePath)
	}
	corefile := candidates[0].Files["Corefile"]
	if !strings.Contains(corefile, "forward . 1.1.1.1:53") {
		t.Fatalf("Corefile = %q", corefile)
	}
	zoneFiles := 0
	for name := range candidates[0].Files {
		if name != "Corefile" {
			zoneFiles++
		}
	}
	if zoneFiles != 1 || !strings.Contains(candidates[1].Files["nginx.conf"], "events {}") {
		t.Fatalf("coredns files = %d, nginx files = %v", zoneFiles, candidates[1].Files)
	}
}

func TestOnlyEnabledZonesAreRendered(t *testing.T) {
	candidates, err := render.Candidates(context.Background(), snapshot(false, plainProxy()), job(store.TargetCombined), render.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates[0].Files) != 1 {
		t.Fatalf("disabled zone produced files: %v", candidates[0].Files)
	}
	if strings.Contains(candidates[1].Files["nginx.conf"], "app.example.test") {
		t.Fatal("disabled zone's record reached nginx")
	}
}

func TestCandidatesRejectsBadInput(t *testing.T) {
	good := snapshot(true, plainProxy())
	tests := []struct {
		name     string
		snapshot json.RawMessage
		job      store.Job
		want     string
	}{
		{"certificate target", good, job(store.TargetCertificate), "Unsupported apply target: certificate"},
		{"unknown target", good, job("bogus"), "Unsupported apply target: bogus"},
		{"no default pool", json.RawMessage(`{"zones":[]}`), job(store.TargetCoreDNS), "Configuration snapshot has no default resolver pool"},
		{"traversing revision id", good, store.Job{RevisionID: "../etc", Target: store.TargetNginx}, "Revision ID is invalid for a candidate path"},
		{"slash in revision id", good, store.Job{RevisionID: "a/b", Target: store.TargetCoreDNS}, "Revision ID is invalid for a candidate path"},
		{"empty revision id", good, store.Job{RevisionID: "", Target: store.TargetCoreDNS}, "Revision ID is invalid for a candidate path"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := render.Candidates(context.Background(), tt.snapshot, tt.job, render.Options{})
			if err == nil || err.Error() != tt.want {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestCapabilitiesAndACMEUpstreamReachTheNginxRenderer(t *testing.T) {
	proxy := `{"origin": {"ip": "192.0.2.20", "port": 8080, "protocol": "http"}, "tlsEnabled": false}`
	candidates, err := render.Candidates(context.Background(), snapshot(true, proxy), job(store.TargetNginx),
		render.Options{ACMEUpstream: "http://api:3000", Capabilities: renderers.Http3Capabilities{}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(candidates[0].Files["nginx.conf"], "http://api:3000") {
		t.Fatalf("ACME upstream missing: %s", candidates[0].Files["nginx.conf"])
	}
}
