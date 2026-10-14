package configuration

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	replicationsnapshot "github.com/zerkc/ProxyCore/apps/api/internal/snapshot"
)

const (
	materializeNodeIPv4 = "198.51.100.77"
	materializeNodeIPv6 = "2001:db8::77"
)

// materializeConfig is the primary's replicated configuration. Zones, records,
// streams and certificates are already in the readers' ordering (name, name,
// listen port, id) so a read-back can be compared structurally.
func materializeConfig() domain.ConfigurationSnapshot {
	comment := "primary comment"
	expires := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	renew := time.Date(2029, 12, 1, 0, 0, 0, 0, time.UTC)
	return domain.ConfigurationSnapshot{
		Settings: domain.Settings{
			Ingress: domain.Ingress{IPv4: "203.0.113.1", IPv6: "2001:db8::1"},
			DefaultPool: &domain.ResolverPool{ID: "pool-1", Endpoints: []domain.ResolverEndpoint{
				{Host: "9.9.9.9", Port: 53},
			}},
			ForwardingRules:     []domain.ForwardingRule{},
			RetentionMaxAgeDays: 14,
			RetentionMaxSizeMb:  100,
		},
		Zones: []domain.ZoneState{
			{
				ID: "11111111-1111-4111-8111-111111111111", Name: "alpha.example", Enabled: true,
				Records: []domain.DNSRecord{
					{ID: "aaaaaaaa-0000-4000-8000-000000000001", Name: "app", Type: "A", Value: "192.0.2.10", TTL: 120,
						Enabled: true, Proxied: true, Comment: &comment,
						Proxy: map[string]any{"origin": map[string]any{"scheme": "http", "host": "192.0.2.10", "port": float64(8080)}}},
					{ID: "aaaaaaaa-0000-4000-8000-000000000002", Name: "mail", Type: "TXT", Value: "v=spf1 -all", TTL: 300, Enabled: true},
				},
			},
			{
				ID: "22222222-2222-4222-8222-222222222222", Name: "beta.example", Enabled: false,
				Records: []domain.DNSRecord{
					{ID: "bbbbbbbb-0000-4000-8000-000000000001", Name: "www", Type: "A", Value: "192.0.2.20", TTL: 60, Enabled: true},
				},
			},
		},
		Streams: []domain.StreamRoute{{
			ID: "33333333-3333-4333-8333-333333333333", Enabled: true, Protocol: "tcp",
			ListenAddress: "0.0.0.0", ListenPort: 2222,
			Upstream: domain.UpstreamTarget{IP: "192.0.2.30", Port: 22, Protocol: "tcp"},
		}},
		Certificates: []domain.CertificateStatus{{
			ID: "44444444-4444-4444-8444-444444444444", Hostnames: []string{"app.alpha.example"},
			Issuer: "letsencrypt", Challenge: "http-01", Environment: "production", Status: "active",
			ExpiresAt: &expires, RenewAfter: &renew, CertificatePEM: "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----",
		}},
	}
}

func materializeEnvelopeBody(t *testing.T, primaryID string, cfg domain.ConfigurationSnapshot, mutate func(*replicationsnapshot.Envelope)) ([]byte, string) {
	t.Helper()
	env := importTestEnvelope(t)
	env.Transient.SourcePrimaryID = uuid.MustParse(primaryID)
	env.NodeLocal.Ingress = domain.Ingress{IPv4: materializeNodeIPv4, IPv6: materializeNodeIPv6}
	env.Replicated.Configuration = cfg
	if mutate != nil {
		mutate(&env)
	}
	var err error
	env.ContentHash, err = env.ExpectedHash()
	if err != nil {
		t.Fatal(err)
	}
	body, err := replicationsnapshot.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	return body, env.ContentHash
}

func readNodeConfiguration(t *testing.T, f *sqlitePublicationFixture) domain.ConfigurationSnapshot {
	t.Helper()
	got, err := NewSQLiteSnapshotStore(f.db, f.writer).Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for i := range got.Certificates {
		if got.Certificates[i].ExpiresAt != nil {
			utc := got.Certificates[i].ExpiresAt.UTC()
			got.Certificates[i].ExpiresAt = &utc
		}
		if got.Certificates[i].RenewAfter != nil {
			utc := got.Certificates[i].RenewAfter.UTC()
			got.Certificates[i].RenewAfter = &utc
		}
	}
	return got
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// seedLocalConfiguration is the node's pre-enrollment standalone configuration.
func seedLocalConfiguration(t *testing.T, f *sqlitePublicationFixture) {
	t.Helper()
	settingsExec(t, f.db, `insert into zones(id,name,enabled,created_at,updated_at) values('99999999-9999-4999-8999-999999999999','local.example',1,'0','0')`)
	seedSQLiteRecord(t, f.db, 99, "99999999-9999-4999-8999-999999999999", "old", "A", `"192.0.2.99"`, 1, 0, nil, nil)
	settingsExec(t, f.db, `insert into stream_routes(id,enabled,protocol,listen_address,listen_port,upstream,created_at,updated_at)
		values('88888888-8888-4888-8888-888888888888',1,'udp','0.0.0.0',5353,'{"ip":"192.0.2.88","port":53,"protocol":"udp"}','0','0')`)
}

func TestSQLiteImportMaterializesConfigurationIntoEditableTables(t *testing.T) {
	f, store := newSQLitePhase2Fixture(t)
	seedLocalConfiguration(t, f)
	cfg := materializeConfig()
	body, _ := materializeEnvelopeBody(t, f.primaryID, cfg, nil)

	if _, err := store.CreateApplyJobFromSnapshot(context.Background(), "import", body); err != nil {
		t.Fatal(err)
	}

	got := readNodeConfiguration(t, f)
	want := cfg
	want.Settings.Ingress = domain.Ingress{IPv4: materializeNodeIPv4, IPv6: materializeNodeIPv6}
	if gotJSON, wantJSON := mustJSON(t, got), mustJSON(t, want); gotJSON != wantJSON {
		t.Fatalf("node tables diverge from the primary configuration\n got: %s\nwant: %s", gotJSON, wantJSON)
	}
	// The pre-enrollment local rows were replaced, not merged.
	if n := phase2Count(t, f, `select count(*) from zones where name = 'local.example'`); n != 0 {
		t.Fatalf("pre-enrollment zone survived: %d", n)
	}
	if n := phase2Count(t, f, `select count(*) from stream_routes where listen_port = 5353`); n != 0 {
		t.Fatalf("pre-enrollment stream survived: %d", n)
	}
}

func TestSQLiteImportNodeIngressOverridesSettingsAndKeepsPrimaryHash(t *testing.T) {
	f, store := newSQLitePhase2Fixture(t)
	cfg := materializeConfig()
	body, primaryHash := materializeEnvelopeBody(t, f.primaryID, cfg, nil)

	if _, err := store.CreateApplyJobFromSnapshot(context.Background(), "import", body); err != nil {
		t.Fatal(err)
	}
	if got := readNodeConfiguration(t, f).Settings.Ingress; got != (domain.Ingress{IPv4: materializeNodeIPv4, IPv6: materializeNodeIPv6}) {
		t.Fatalf("settings ingress = %+v, want the node's own", got)
	}
	// D11: the stored hash is the primary's contentHash exactly as received.
	if n := phase2Count(t, f, `select count(*) from config_revisions where snapshot_content_hash = ?`, primaryHash); n != 1 {
		t.Fatalf("revision with the primary content hash = %d", n)
	}
	if n := phase2Count(t, f, `select count(*) from apply_jobs where snapshot_content_hash = ?`, primaryHash); n != 1 {
		t.Fatalf("job with the primary content hash = %d", n)
	}
	// The stored envelope is byte-identical to the one that was verified, so its
	// declared hash still verifies and the node-local ingress travels in
	// nodeLocal (the worker renders from it).
	var stored string
	f.db.QueryRow(`select snapshot from config_revisions`).Scan(&stored)
	if stored != string(body) {
		t.Fatal("stored revision envelope must stay byte-identical to the verified envelope")
	}
	env, err := replicationsnapshot.Unmarshal([]byte(stored))
	if err != nil || env.VerifyHash() != nil || env.ContentHash != primaryHash {
		t.Fatalf("stored envelope hash invalid: %v %v", err, env.VerifyHash())
	}
}

func TestSQLiteImportMaterializationFailureRollsBackRevision(t *testing.T) {
	f, store := newSQLitePhase2Fixture(t)
	seedLocalConfiguration(t, f)
	body, _ := materializeEnvelopeBody(t, f.primaryID, materializeConfig(), nil)
	settingsExec(t, f.db, `create trigger materialize_stream_fail before insert on stream_routes begin select raise(abort, 'stream insert refused'); end`)

	_, err := store.CreateApplyJobFromSnapshot(context.Background(), "import", body)
	if err == nil || !strings.Contains(err.Error(), "stream insert refused") {
		t.Fatalf("err = %v", err)
	}
	for _, query := range []string{
		`select count(*) from config_revisions`,
		`select count(*) from apply_jobs`,
		`select count(*) from zones where name like '%.example' and name != 'local.example'`,
	} {
		if n := phase2Count(t, f, query); n != 0 {
			t.Fatalf("%s = %d after a failed materialization", query, n)
		}
	}
	if n := phase2Count(t, f, `select count(*) from zones where name = 'local.example'`); n != 1 {
		t.Fatalf("local zone lost on rollback: %d", n)
	}
}

func TestSQLiteImportSecondSnapshotReplacesContent(t *testing.T) {
	f, store := newSQLitePhase2Fixture(t)
	ctx := context.Background()
	first, _ := materializeEnvelopeBody(t, f.primaryID, materializeConfig(), nil)
	if _, err := store.CreateApplyJobFromSnapshot(ctx, "import", first); err != nil {
		t.Fatal(err)
	}
	next := domain.ConfigurationSnapshot{
		Settings: domain.Settings{
			Ingress:             domain.Ingress{IPv4: "203.0.113.9"},
			ForwardingRules:     []domain.ForwardingRule{},
			RetentionMaxAgeDays: 7, RetentionMaxSizeMb: 50,
		},
		Zones: []domain.ZoneState{{
			ID: "55555555-5555-4555-8555-555555555555", Name: "gamma.example", Enabled: true,
			Records: []domain.DNSRecord{{ID: "cccccccc-0000-4000-8000-000000000001", Name: "x", Type: "A", Value: "192.0.2.50", TTL: 300, Enabled: true}},
		}},
		Streams:      []domain.StreamRoute{},
		Certificates: []domain.CertificateStatus{},
	}
	second, _ := materializeEnvelopeBody(t, f.primaryID, next, nil)
	if _, err := store.CreateApplyJobFromSnapshot(ctx, "import", second); err != nil {
		t.Fatal(err)
	}

	got := readNodeConfiguration(t, f)
	want := next
	want.Settings.Ingress = domain.Ingress{IPv4: materializeNodeIPv4, IPv6: materializeNodeIPv6}
	if gotJSON, wantJSON := mustJSON(t, got), mustJSON(t, want); gotJSON != wantJSON {
		t.Fatalf("second import must replace, not merge\n got: %s\nwant: %s", gotJSON, wantJSON)
	}
	if n := phase2Count(t, f, `select count(*) from config_revisions`); n != 2 {
		t.Fatalf("revisions = %d", n)
	}
}

// T13 seam: secrets and owners are replicated by the envelope but are NOT
// materialized yet. This test documents that boundary; T13 replaces it with
// positive assertions once secret unwrap/re-wrap and owner import exist.
func TestSQLiteImportLeavesSecretsAndOwnersUntouched(t *testing.T) {
	f, store := newSQLitePhase2Fixture(t)
	usersBefore := phase2Count(t, f, `select count(*) from users`)
	secretsBefore := phase2Count(t, f, `select count(*) from secrets`)
	cfg := materializeConfig()
	cfg.Certificates[0].SecretID = uuid.NewString()
	body, _ := materializeEnvelopeBody(t, f.primaryID, cfg, func(env *replicationsnapshot.Envelope) {
		env.Replicated.Secrets = []replicationsnapshot.ReplicatedSecret{{ID: uuid.New(), Purpose: "basic-auth-password", Envelope: "sealed"}}
		env.Replicated.Owners = []replicationsnapshot.ReplicatedOwner{{UserID: uuid.New(), Username: "primary-owner", PasswordHash: "scrypt$x", Role: "owner"}}
	})
	if _, err := store.CreateApplyJobFromSnapshot(context.Background(), "import", body); err != nil {
		t.Fatal(err)
	}
	if n := phase2Count(t, f, `select count(*) from users`); n != usersBefore {
		t.Fatalf("users changed: %d -> %d", usersBefore, n)
	}
	if n := phase2Count(t, f, `select count(*) from secrets`); n != secretsBefore {
		t.Fatalf("secrets changed: %d -> %d", secretsBefore, n)
	}
	// A certificate whose key secret is not present locally yet is stored
	// without the reference instead of failing the foreign key.
	if n := phase2Count(t, f, `select count(*) from certificates where key_secret_id is null`); n != 1 {
		t.Fatalf("certificates without key reference = %d", n)
	}
}

func TestSQLiteImportArchivesTheReplacedLocalConfiguration(t *testing.T) {
	f, store := newSQLitePhase2Fixture(t)
	body, _ := materializeEnvelopeBody(t, f.primaryID, materializeConfig(), nil)

	// A pristine installation has nothing to preserve.
	if _, err := store.CreateApplyJobFromSnapshot(context.Background(), "import", body); err != nil {
		t.Fatal(err)
	}
	if n := phase2Count(t, f, `select count(*) from standalone_archives where capture_reason = ?`, preImportArchiveReason); n != 0 {
		t.Fatalf("pristine import archived %d rows", n)
	}

	// The second import replaces the first import's content, which is archived.
	next := materializeConfig()
	next.Zones = next.Zones[:1]
	other, _ := materializeEnvelopeBody(t, f.primaryID, next, nil)
	if _, err := store.CreateApplyJobFromSnapshot(context.Background(), "import", other); err != nil {
		t.Fatal(err)
	}
	var blob string
	if err := f.db.QueryRow(`select archive_blob from standalone_archives where capture_reason = ?`, preImportArchiveReason).Scan(&blob); err != nil {
		t.Fatalf("local configuration was not archived: %v", err)
	}
	if !strings.Contains(blob, "beta.example") || !strings.Contains(blob, "alpha.example") {
		t.Fatalf("archive does not hold the replaced configuration: %s", blob)
	}
}
