package configuration

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
)

// PG-backed AddRecord checks (skipped without a database URL, like the other
// Postgres fixtures). Follow-up 3: every accepted spelling of the zone id and of
// the record id is canonicalized once at entry, so aliases behave exactly like the
// canonical spelling, and a CNAME next to another record is a validation error
// instead of a panic. Synthetic ids only.
const (
	pgAliasZoneID  = "aaaaaaaa-0000-4000-8000-000000000001"
	pgAliasCertID  = "cccccccc-0000-4000-8000-000000000001"
	pgAliasRecordA = "eeeeeeee-0000-4000-8000-000000000001"
	pgAliasRecordW = "eeeeeeee-0000-4000-8000-000000000002"
)

func pgAliasFixture(t *testing.T) *importPostgresFixture {
	t.Helper()
	f := newImportPostgresFixture(t)
	ctx := context.Background()
	for _, stmt := range []string{
		`insert into installation_settings (id, ingress_ipv4, forwarding_rules, default_resolver_pool)
			values ('default', '192.0.2.1', '[]'::jsonb, '{"id":"main","endpoints":[{"host":"192.0.2.53","port":53}]}'::jsonb)`,
		`insert into zones (id, name) values ('` + pgAliasZoneID + `', 'example.test')`,
		`insert into certificates (id, hostnames, issuer, challenge, environment, status, certificate_pem)
			values ('` + pgAliasCertID + `', '["*.example.test"]'::jsonb, 'uploaded', 'none', 'custom', 'issued', 'fixture PEM')`,
		`insert into dns_records (id, zone_id, name, type, value, ttl, enabled, proxied, updated_at)
			values ('` + pgAliasRecordA + `', '` + pgAliasZoneID + `', 'a.example.test', 'A', '"192.0.2.1"', 300, true, false, '2000-01-01')`,
		`insert into dns_records (id, zone_id, name, type, value, ttl, enabled, proxied, proxy_settings, updated_at)
			values ('` + pgAliasRecordW + `', '` + pgAliasZoneID + `', 'www.example.test', 'A', '"192.0.2.2"', 300, true, true,
			'{"origin":{"ip":"192.0.2.10","port":8080,"protocol":"http"},"tlsEnabled":true,"certificateId":"` + pgAliasCertID + `","websocket":true}'::jsonb, '2000-01-01')`,
	} {
		if _, err := f.pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	return f
}

func pgAliasRows(t *testing.T, f *importPostgresFixture) []string {
	t.Helper()
	rows, err := f.pool.Query(context.Background(), `select case when id in ($1::uuid, $2::uuid) then right(id::text, 2) else 'NEW' end,
		right(zone_id::text, 2), name, type::text, value::text, proxied, updated_at > '2000-01-01' from dns_records order by name, type, id`,
		pgAliasRecordA, pgAliasRecordW)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id, zone, name, typ, value string
		var proxied, rewritten bool
		if err := rows.Scan(&id, &zone, &name, &typ, &value, &proxied, &rewritten); err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("%s %s %s %s %s proxied=%v rewritten=%v", id, zone, name, typ, value, proxied, rewritten))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

func pgCount(t *testing.T, f *importPostgresFixture, table string) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(), "select count(*) from "+table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestPostgresAddRecordZoneAliasBehavesLikeCanonical(t *testing.T) {
	const (
		aSame  = `01 01 a.example.test A "192.0.2.1" proxied=false rewritten=true`
		aPatch = `01 01 a.example.test A "192.0.2.77" proxied=false rewritten=true`
		aOld   = `01 01 a.example.test A "192.0.2.1" proxied=false rewritten=false`
		wOld   = `02 01 www.example.test A "192.0.2.2" proxied=true rewritten=false`
		wNew   = `02 01 www.example.test A "192.0.2.2" proxied=true rewritten=true`
		cNew   = `NEW 01 c.example.test A "192.0.2.9" proxied=false rewritten=true`
	)
	patch := func(id string) domain.DNSRecordInput {
		return domain.DNSRecordInput{ID: id, Name: "a", Type: "A", Value: "192.0.2.77", Enabled: true}
	}
	proxied := domain.DNSRecordInput{Name: "www", Type: "A", Value: "192.0.2.10", Enabled: true, Proxied: true, Proxy: map[string]any{
		"origin": map[string]any{"ip": "192.0.2.10", "port": 8080, "protocol": "http"}, "tlsEnabled": true, "certificateId": pgAliasCertID, "websocket": true}}
	samePxy := proxied
	samePxy.ID = strings.ToUpper(pgAliasRecordW)
	spellings := []struct{ name, zone string }{
		{"canonical", pgAliasZoneID},
		{"uppercase", strings.ToUpper(pgAliasZoneID)},
		{"braced", "{" + pgAliasZoneID + "}"},
		{"hyphen-less", strings.ReplaceAll(pgAliasZoneID, "-", "")},
		{"4-digit-groups", "aaaa-aaaa-0000-4000-8000-0000-0000-0001"},
	}
	ops := []struct {
		name  string
		input domain.DNSRecordInput
		want  []string
		err   string
		id    string
	}{
		{name: "new record", input: domain.DNSRecordInput{Name: "c", Type: "A", Value: "192.0.2.9", Enabled: true}, want: []string{aSame, wNew, cNew}},
		{name: "second proxied record for the same name", input: proxied, err: "Only one record may be proxied for www.example.test"},
		{name: "patch sibling by canonical id", input: patch(pgAliasRecordA), want: []string{aPatch, wNew}, id: pgAliasRecordA},
		{name: "patch sibling by uppercase id", input: patch(strings.ToUpper(pgAliasRecordA)), want: []string{aPatch, wNew}, id: pgAliasRecordA},
		{name: "same proxied record by uppercase id is not a conflict", input: samePxy,
			want: []string{aSame, `02 01 www.example.test A "192.0.2.10" proxied=true rewritten=true`}, id: pgAliasRecordW},
		{name: "CNAME next to an A record", input: domain.DNSRecordInput{Name: "a", Type: "CNAME", Value: "x.example.test", Enabled: true},
			err: "CNAME cannot coexist with other records for a.example.test"},
	}
	for _, sp := range spellings {
		for _, op := range ops {
			t.Run(sp.name+"/"+op.name, func(t *testing.T) {
				f := pgAliasFixture(t)
				got, err := f.store.AddRecord(context.Background(), sp.zone, op.input, "")
				want, jobs := op.want, 1
				if op.err != "" {
					var ve *domain.ValidationError
					if !errors.As(err, &ve) || err.Error() != op.err || !reflect.DeepEqual(got, AutoApplyResult[domain.DNSRecord]{}) {
						t.Fatalf("error = %T %v", err, err)
					}
					want, jobs = []string{aOld, wOld}, 0
				} else if err != nil {
					t.Fatal(err)
				} else if op.id != "" && got.Value.ID != op.id {
					t.Fatalf("returned id = %q, want canonical %q", got.Value.ID, op.id)
				}
				if rows := pgAliasRows(t, f); !reflect.DeepEqual(rows, sortedCopy(want)) {
					t.Fatalf("rows =\n%s\nwant\n%s", strings.Join(rows, "\n"), strings.Join(sortedCopy(want), "\n"))
				}
				if pgCount(t, f, "apply_jobs") != jobs || pgCount(t, f, "config_revisions") != jobs {
					t.Fatalf("want %d revision and apply job", jobs)
				}
			})
		}
	}
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func TestPostgresAddRecordMalformedZoneIDKeepsCastError(t *testing.T) {
	f := pgAliasFixture(t)
	_, err := f.store.AddRecord(context.Background(), "not-a-zone", domain.DNSRecordInput{Name: "c", Type: "A", Value: "192.0.2.9", Enabled: true}, "")
	if err == nil || !strings.Contains(err.Error(), `invalid input syntax for type uuid: "not-a-zone"`) {
		t.Fatalf("error = %v", err)
	}
	if pgCount(t, f, "apply_jobs") != 0 || pgCount(t, f, "config_revisions") != 0 {
		t.Fatal("state changed across a malformed zone id")
	}
}

const pgAliasEmptyZoneID = "aaaaaaaa-0000-4000-8000-000000000002"

func pgZoneSpellings() []struct{ name, id string } {
	return []struct{ name, id string }{
		{"canonical", pgAliasZoneID},
		{"uppercase", strings.ToUpper(pgAliasZoneID)},
		{"braced", "{" + pgAliasZoneID + "}"},
		{"hyphen-less", strings.ReplaceAll(pgAliasZoneID, "-", "")},
		{"4-digit-groups", "aaaa-aaaa-0000-4000-8000-0000-0000-0001"},
	}
}

// Store.GetZone selects the zone's records by the stored (canonical) id, so every
// spelling the database accepts returns the canonical id and the zone's records.
func TestPostgresGetZoneAcceptsEveryUUIDSpelling(t *testing.T) {
	for _, sp := range pgZoneSpellings() {
		t.Run(sp.name, func(t *testing.T) {
			f := pgAliasFixture(t)
			zone, err := f.store.GetZone(context.Background(), sp.id)
			if err != nil {
				t.Fatal(err)
			}
			var ids []string
			for _, r := range zone.Records {
				ids = append(ids, r.ID)
			}
			sort.Strings(ids)
			if zone.ID != pgAliasZoneID || zone.Name != "example.test" || !reflect.DeepEqual(ids, []string{pgAliasRecordA, pgAliasRecordW}) {
				t.Fatalf("zone = id %q name %q records %v", zone.ID, zone.Name, ids)
			}
		})
	}
}

func TestPostgresGetZoneEdgeCases(t *testing.T) {
	f := pgAliasFixture(t)
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, `insert into zones (id, name) values ('`+pgAliasEmptyZoneID+`', 'empty.test')`); err != nil {
		t.Fatal(err)
	}
	t.Run("zone without records has a non-nil empty slice", func(t *testing.T) {
		for _, id := range []string{pgAliasEmptyZoneID, strings.ToUpper(pgAliasEmptyZoneID)} {
			zone, err := f.store.GetZone(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if zone.ID != pgAliasEmptyZoneID || zone.Records == nil || len(zone.Records) != 0 {
				t.Fatalf("zone = %q records = %#v", zone.ID, zone.Records)
			}
		}
	})
	t.Run("missing zone", func(t *testing.T) {
		_, err := f.store.GetZone(ctx, "aaaaaaaa-0000-4000-8000-0000000000ff")
		if err == nil || err.Error() != "Zone not found" {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("malformed id keeps the cast error", func(t *testing.T) {
		_, err := f.store.GetZone(ctx, "not-a-zone")
		if err == nil || !strings.Contains(err.Error(), `invalid input syntax for type uuid: "not-a-zone"`) {
			t.Fatalf("error = %v", err)
		}
	})
}

// A basicAuth record updated through any spelling of its id (a PATCH with a blank
// password) is matched as the same record, so it inherits the stored password
// secret instead of failing or creating a second one.
func TestPostgresAddRecordAliasRecordIDInheritsPasswordSecret(t *testing.T) {
	spellings := []struct {
		name string
		fn   func(string) string
	}{
		{"canonical", func(id string) string { return id }},
		{"uppercase", strings.ToUpper},
		{"braced", func(id string) string { return "{" + strings.ToUpper(id) + "}" }},
		{"hyphen-less", func(id string) string { return strings.ReplaceAll(id, "-", "") }},
	}
	proxied := func(basicAuth map[string]any) domain.DNSRecordInput {
		return domain.DNSRecordInput{Name: "app", Type: "A", Value: "192.0.2.10", Enabled: true, Proxied: true, Proxy: map[string]any{
			"origin": map[string]any{"ip": "192.0.2.10", "port": 8080, "protocol": "http"}, "tlsEnabled": true,
			"certificateId": pgAliasCertID, "websocket": true, "basicAuth": basicAuth}}
	}
	for _, sp := range spellings {
		t.Run(sp.name, func(t *testing.T) {
			// Synthetic key, never a real one.
			t.Setenv("PROXYCORE_MASTER_KEY_BASE64", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x37}, 32)))
			f := pgAliasFixture(t)
			ctx := context.Background()
			created, err := f.store.AddRecord(ctx, pgAliasZoneID, proxied(map[string]any{"username": "alice", "password": "synthetic-pass"}), "")
			if err != nil {
				t.Fatal(err)
			}
			want, _ := created.Value.Proxy["basicAuth"].(map[string]any)["passwordSecretId"].(string)
			if want == "" || pgCount(t, f, "secrets") != 1 {
				t.Fatalf("setup: secret id %q, secrets %d", want, pgCount(t, f, "secrets"))
			}
			input := proxied(map[string]any{"username": "alice"})
			input.ID = sp.fn(created.Value.ID)
			got, err := f.store.AddRecord(ctx, pgAliasZoneID, input, "")
			if err != nil {
				t.Fatal(err)
			}
			if gotID, _ := got.Value.Proxy["basicAuth"].(map[string]any)["passwordSecretId"].(string); gotID != want {
				t.Fatalf("passwordSecretId = %q, want inherited %q", gotID, want)
			}
			if got.Value.ID != created.Value.ID || pgCount(t, f, "secrets") != 1 {
				t.Fatalf("id %q (want %q), secrets %d (want 1)", got.Value.ID, created.Value.ID, pgCount(t, f, "secrets"))
			}
		})
	}
}
