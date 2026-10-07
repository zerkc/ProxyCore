package pgconvert

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

const testPGEnv = "PROXYCORE_CONVERT_TEST_PG_URL"

// pgFixture is a freshly migrated, seeded database owned by one test.
type pgFixture struct {
	URL string
}

func (f pgFixture) connect(t *testing.T) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), f.URL)
	if err != nil {
		t.Fatalf("connect fixture: %v", err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	return conn
}

func (f pgFixture) exec(t *testing.T, sql string) {
	t.Helper()
	if _, err := f.connect(t).Exec(context.Background(), sql); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

// newPGFixture creates an empty database and applies the real Drizzle migrations.
func newPGFixture(t *testing.T) pgFixture {
	t.Helper()
	base := os.Getenv(testPGEnv)
	if base == "" {
		t.Skipf("%s not set", testPGEnv)
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	defer admin.Close(ctx)
	var suffix [6]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	name := "conv_" + hex.EncodeToString(suffix[:])
	if _, err := admin.Exec(ctx, "create database "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), base)
		if err != nil {
			return
		}
		defer c.Close(context.Background())
		_, _ = c.Exec(context.Background(), "drop database if exists "+name+" with (force)")
	})
	parsed, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	parsed.Path = "/" + name
	f := pgFixture{URL: parsed.String()}
	conn := f.connect(t)
	files, err := filepath.Glob(filepath.Join("..", "..", "..", "..", "packages", "db", "migrations", "*.sql"))
	if err != nil || len(files) != 9 {
		t.Fatalf("drizzle migrations: %v (%d files)", err, len(files))
	}
	sort.Strings(files)
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, statement := range strings.Split(string(raw), "--> statement-breakpoint") {
			if strings.TrimSpace(statement) == "" {
				continue
			}
			if _, err := conn.Exec(ctx, statement); err != nil {
				t.Fatalf("apply %s: %v\n%s", filepath.Base(file), err, statement)
			}
		}
	}
	return f
}

var uuidToken = regexp.MustCompile(`\{u(\d+)\}`)

func fixtureUUID(n string) string {
	var value int
	fmt.Sscanf(n, "%d", &value)
	return fmt.Sprintf("00000000-0000-4000-8000-%012d", value)
}

func expandUUIDs(sql string) string {
	return uuidToken.ReplaceAllStringFunc(sql, func(token string) string {
		return fixtureUUID(uuidToken.FindStringSubmatch(token)[1])
	})
}

// newSeededPG returns a migrated database with at least one row in each of the
// 30 product tables, covering the representation edge cases of the converter.
func newSeededPG(t *testing.T) pgFixture {
	t.Helper()
	f := newPGFixture(t)
	conn := f.connect(t)
	for i, statement := range seedStatements {
		if _, err := conn.Exec(context.Background(), expandUUIDs(statement)); err != nil {
			t.Fatalf("seed statement %d: %v\n%s", i, err, statement)
		}
	}
	return f
}

const allBytes = `(select decode(string_agg(lpad(to_hex(g),2,'0'),'' order by g),'hex') from generate_series(0,255) g)`

var seedStatements = []string{
	`insert into users (id, username, password_hash, role, active, password_change_required, created_at, updated_at) values
	 ('{u1}','owner','hash-1','owner',true,false,'2026-10-07 12:34:56.123456+00','2026-10-07 12:34:56.654321+00'),
	 ('{u2}','Zoë 日本語 🚀 ''quoted''','hash-2','operator',false,true,'1969-12-31 23:59:59.999999+00','infinity')`,
	`insert into sessions (id, user_id, token_hash, expires_at, revoked_at, last_seen_at, created_at) values
	 ('{u3}','{u1}','tok-1','infinity','-infinity','2026-01-02 03:04:05.000001+00','2026-01-01 00:00:00+00'),
	 ('{u4}','{u2}','tok-2','9999-12-31 23:59:59.999999+00',null,null,'2026-01-01 00:00:00.5+00')`,
	`insert into audit_events (id, actor_user_id, action, resource_type, resource_id, before_value, after_value, correlation_id, result, created_at) values
	 ('{u5}','{u1}','update','zone','z-1','{"a":1,"b":[1,2.50,12345678901234567890]}','{"b":null}','corr-1','ok','2026-03-04 05:06:07.891011+00'),
	 ('{u6}',null,'login','session',null,null,null,'corr-2','denied','2026-03-04 05:06:08+00')`,
	`insert into installation_settings (id, ingress_ipv4, ingress_ipv6, default_resolver_pool, forwarding_rules, enrollment_hostnames, retention_max_age_days, retention_max_size_mb, current_desired_revision_id, current_applied_revision_id, created_at, updated_at) values
	 ('singleton','192.0.2.1','2001:db8::1','{"pool":"x"}','[]','["a.example","é.example"]',-2147483648,2147483647,'rev-d',null,'2026-01-01 00:00:00+00','2026-01-01 00:00:01+00')`,
	`insert into zones (id, name, enabled, created_at, updated_at) values
	 ('{u7}','example.test',true,'2026-01-01 00:00:00+00','2026-01-01 00:00:00+00'),
	 ('{u8}','disabled.test',false,'2026-01-01 00:00:00+00','2026-01-01 00:00:00+00')`,
	`insert into dns_records (id, zone_id, name, type, value, ttl, enabled, proxied, proxy_settings, comment, created_at, updated_at) values
	 ('{u9}','{u7}','txt','TXT','{"k":1,"k":2,"a":1,"a ":2,"A":3,"big":12345678901234567890,"f":1.0e100,"n":{"x":{"y":[true,false,null,0.1]}},"s":"é\n\"q\""}',300,true,false,null,'unicode ✓ comment','2026-01-01 00:00:00+00','2026-01-01 00:00:00+00'),
	 ('{u10}','{u7}','www','A','"192.0.2.9"',-1,false,true,'{"tls":true}',null,'2026-01-01 00:00:00+00','2026-01-01 00:00:00+00')`,
	`insert into resolver_pools (id, name, endpoints, is_default, created_at, updated_at) values
	 ('{u11}','default','["1.1.1.1:53"]',true,'2026-01-01 00:00:00+00','2026-01-01 00:00:00+00'),
	 ('{u12}','other','[]',false,'2026-01-01 00:00:00+00','2026-01-01 00:00:00+00')`,
	`insert into forwarding_rules (id, suffix, pool_id, created_at, updated_at) values
	 ('{u13}','corp.test','{u11}','2026-01-01 00:00:00+00','2026-01-01 00:00:00+00')`,
	`insert into stream_routes (id, enabled, protocol, listen_address, listen_port, upstream, created_at, updated_at) values
	 ('{u14}',true,'tcp','0.0.0.0',2147483647,'{"host":"h","port":1}','2026-01-01 00:00:00+00','2026-01-01 00:00:00+00'),
	 ('{u15}',false,'udp','::',0,'{}','2026-01-01 00:00:00+00','2026-01-01 00:00:00+00')`,
	`insert into secrets (id, purpose, ciphertext, created_at, updated_at) values
	 ('{u16}','provider','v1:AAECAwQFBgcICQ==:+/==','2026-01-01 00:00:00+00','2026-01-01 00:00:00+00'),
	 ('{u17}','internal-ca-key','v1:ünicode ciphertext','2026-01-01 00:00:00+00','2026-01-01 00:00:00+00'),
	 ('{u18}','enrollment-key','','2026-01-01 00:00:00+00','2026-01-01 00:00:00+00')`,
	`insert into certificates (id, hostnames, issuer, challenge, environment, status, expires_at, renew_after, key_secret_id, certificate_pem, failure_reason, created_at, updated_at) values
	 ('{u19}','["a.example"]','uploaded','none','production','active','2027-01-01 00:00:00.000001+00','2026-12-01 00:00:00+00','{u16}','-----BEGIN CERTIFICATE-----',null,'2026-01-01 00:00:00+00','2026-01-01 00:00:00+00'),
	 ('{u20}','["b.example"]','self-signed','http-01','staging','failed',null,null,null,null,'boom: ✗','2026-01-01 00:00:00+00','2026-01-01 00:00:00+00')`,
	`insert into internal_ca (id, certificate_pem, key_secret_id, enrollment_certificate_pem, enrollment_key_secret_id, created_at, updated_at) values
	 ('default','pem','{u17}','epem','{u18}','2026-01-01 00:00:00+00','2026-01-01 00:00:00+00')`,
	`insert into provider_connections (id, provider, name, secret_id, scope, enabled, created_at, updated_at) values
	 ('{u21}','cloudflare','cf','{u16}','dns',true,'2026-01-01 00:00:00+00','2026-01-01 00:00:00+00')`,
	`insert into config_revisions (id, revision_number, checksum, snapshot, actor_user_id, created_at, applied_at, source, source_primary_id, source_node_id, source_revision_id, snapshot_content_hash, snapshot_version, replication_version, leadership_generation) values
	 ('{u22}',1,'sum-1','{"zones":[{"n":"é"}],"big":9007199254740993}','{u1}','2026-01-01 00:00:00+00','2026-01-01 00:00:01+00','ordinary',null,null,null,null,null,null,null),
	 ('{u23}',2,'sum-2','{}',null,'2026-01-02 00:00:00+00',null,'sync','{u30}','{u31}','{u22}','content-hash-2',2147483647,1,9223372036854775807)`,
	`insert into apply_jobs (id, revision_id, actor_user_id, target, status, correlation_id, validation_output, apply_output, health_output, error_message, claimed_at, started_at, finished_at, created_at, source, source_primary_id, source_node_id, source_revision_id, snapshot_content_hash, snapshot_version, replication_version, leadership_generation) values
	 ('{u24}','{u22}','{u1}','nginx','applied','corr-3','{"ok":true}','{"o":[1,2]}','null','',  '2026-01-01 00:00:00+00','2026-01-01 00:00:01+00','2026-01-01 00:00:02+00','2026-01-01 00:00:00+00','ordinary',null,null,null,null,null,null,null),
	 ('{u25}','{u23}',null,'combined','queued','corr-4',null,null,null,null,null,null,null,'2026-01-01 00:00:00+00','import','{u30}','{u31}','{u22}','h',1,2,-9223372036854775808)`,
	`insert into health_observations (id, component, status, details, created_at) values
	 ('{u26}','coredns','ok','{"latency":0.25}','2026-01-01 00:00:00+00'),
	 ('{u27}','nginx','down',null,'2026-01-01 00:00:00+00')`,
	`insert into operational_artifacts (id, kind, path, size_bytes, revision_id, created_at) values
	 ('{u28}','nginx','/etc/nginx/x.conf',2147483647,'{u22}','2026-01-01 00:00:00+00'),
	 ('{u29}','coredns','/etc/coredns/Corefile',0,null,'2026-01-01 00:00:00+00')`,
	`insert into installation_identity (id, installation_id, node_id, role, leadership_generation, latest_known_generation, cluster_key_id, updated_at) values
	 ('primary','{u30}','{u31}','primary-with-nodes',9223372036854775807,9223372036854775806,'{u32}','2026-01-01 00:00:00+00')`,
	`insert into cluster_keys (id, purpose, wrapped_kek, wrapping_key_version, created_at, retired_at) values
	 ('{u32}','replication','wrapped-1',1,'2026-01-01 00:00:00+00',null),
	 ('{u33}','replication','wrapped-0',2147483647,'2025-01-01 00:00:00+00','2026-01-01 00:00:00+00')`,
	`insert into node_state (id, enrolled_at, enrollment_token_hash, enrollment_primary_id, last_seen_at, last_applied_snapshot_id, enrollment_attempt_id, primary_url, primary_installation_id, primary_tls_spki_sha256, credential_id, sync_enabled, last_attempt_at, last_success_at, consecutive_failures, next_attempt_at, last_error_code, updated_at) values
	 ('self','2026-01-01 00:00:00.000123+00','th','{u30}','2026-01-01 00:00:00+00','{u34}','{u41}','https://primary.test','{u30}','spki','{u42}',true,'2026-01-01 00:00:00+00','2026-01-01 00:00:00+00',3,'2026-01-01 00:01:00+00','E_X','2026-01-01 00:00:00+00')`,
	`insert into applied_snapshots (id, source_primary_id, leadership_generation, snapshot_version, replication_version, content_hash, revision_id, status, apply_job_id, failure_code, applied_at, discarded_at, snapshot_body) values
	 ('{u34}','{u30}',9223372036854775807,1,1,'ch-1','{u22}','applied','{u24}',null,'2026-01-01 00:00:00+00',null,` + allBytes + `),
	 ('{u35}','{u30}',1,1,1,'ch-2',null,'rejected',null,'F_1','2026-01-01 00:00:00+00','2026-01-02 00:00:00+00','\x'::bytea),
	 ('{u36}','{u30}',1,1,1,'ch-3',null,'pending',null,null,'2026-01-01 00:00:00+00',null,null)`,
	`insert into standalone_archives (id, snapshot_content_hash, revision_id, archive_blob, capture_reason, captured_at, retention_started_at, expires_at, restoration_eligible, created_at) values
	 ('{u37}','sh','{u22}','blob-é','enroll','2026-01-01 00:00:00+00','2026-01-01 00:00:00+00','infinity',true,'2026-01-01 00:00:00+00'),
	 ('{u38}',null,null,'','manual','2026-01-01 00:00:00+00',null,null,false,'2026-01-01 00:00:00+00')`,
	`insert into enrollment_tokens (id, token_selector, token_hash, hash_version, created_by_user_id, created_at, expires_at, consumed_at, consumed_by_attempt_id, revoked_at) values
	 ('{u39}','sel-1','hash','v1','{u1}','2026-01-01 00:00:00+00','2026-01-02 00:00:00+00','2026-01-01 01:00:00+00','{u90}',null),
	 ('{u40}','sel-2','hash','v1','{u1}','2026-01-01 00:00:00+00','2026-01-02 00:00:00+00',null,null,'2026-01-01 02:00:00+00')`,
	`insert into enrollment_attempts (id, state, primary_url, expected_primary_id, verified_primary_id, verified_primary_node_id, verified_leadership_generation, verified_primary_tls_spki_sha256, verified_primary_ca_fingerprint, preview_digest, local_node_ip, archive_id, ephemeral_private_key_wrapped, bootstrap_payload, node_credential_secret_id, cluster_key_id, initial_snapshot_hash, initial_snapshot_revision_id, initial_apply_job_id, failure_code, confirmed_at, created_at, updated_at) values
	 ('{u41}','committed','https://primary.test','{u30}','{u30}','{u31}',9223372036854775807,'spki','ca','digest','10.0.0.2','{u37}','wrapped','payload','{u16}','{u32}','ish','{u22}','{u24}',null,'2026-01-01 00:00:00+00','2026-01-01 00:00:00+00','2026-01-01 00:00:00+00')`,
	`insert into enrollment_grants (attempt_id, token_id, installation_id, node_id, primary_id, primary_generation, node_ephemeral_public_key, sealed_bootstrap_payload, payload_hash, created_at, expires_at, verified_preview_digest) values
	 ('{u91}','{u39}','{u30}','{u43}','{u31}',9223372036854775807,'pub','sealed','ph','2026-01-01 00:00:00+00','2026-01-02 00:00:00+00',null)`,
	`insert into node_credentials (id, node_id, credential_hash, hash_version, created_at, last_authenticated_at, revoked_at) values
	 ('{u42}','{u43}','chash','v1','2026-01-01 00:00:00+00',null,null)`,
	`insert into enrolled_nodes (node_id, installation_id, primary_id, display_name, credential_id, enrolled_at, revoked_at, created_by_attempt_id) values
	 ('{u43}','{u30}','{u31}','Nodo ñ','{u42}','2026-01-01 00:00:00+00',null,'{u92}')`,
	`insert into node_snapshot_acks (node_id, content_hash, snapshot_version, replication_version, revision_id, leadership_generation, applied_at, received_at) values
	 ('{u43}','ch-1',1,1,'{u22}',9223372036854775807,'2026-01-01 00:00:00+00','2026-01-01 00:00:01.000002+00')`,
	`insert into sync_attempts (id, node_id, trigger, status, source_primary_id, leadership_generation, snapshot_version, replication_version, content_hash, revision_id, apply_job_id, result_code, started_at, finished_at, created_at, updated_at) values
	 ('{u44}','{u43}','periodic','applied','{u30}',1,1,1,'ch-1','{u22}','{u24}','ok','2026-01-01 00:00:00+00','2026-01-01 00:00:01+00','2026-01-01 00:00:00+00','2026-01-01 00:00:01+00')`,
	`insert into internal_ca_enrollment_state (id, established_at) values ('default','2026-01-01 00:00:00+00')`,
}
