package configuration

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/secrets"
	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

// Synthetic fixture values only; the master key is created here, never real.
const (
	recordZoneID   = "aaaaaaaa-0000-4000-8000-000000000001"
	recordZoneID2  = "aaaaaaaa-0000-4000-8000-000000000002"
	recordCertID   = "cccccccc-0000-4000-8000-000000000001"
	recordExistID  = "eeeeeeee-0000-4000-8000-000000000001"
	recordExistID2 = "eeeeeeee-0000-4000-8000-000000000002"
	recordPassword = "synthetic-password-1"
)

var recordMasterKey = base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32)))

type recordFixture struct {
	db      *sql.DB
	path    string
	secrets *secrets.SQLiteStore
	store   *SQLiteRecordWriteStore
}

func newRecordFixture(t *testing.T) *recordFixture {
	t.Helper()
	db, path := enqueueFixture(t)
	settingsExec(t, db, "update installation_settings set ingress_ipv4='192.0.2.1'")
	settingsExec(t, db, `insert into zones (id,name,enabled,created_at,updated_at) values (?,?,1,'0','0')`, recordZoneID, "example.test")
	settingsExec(t, db, `insert into zones (id,name,enabled,created_at,updated_at) values (?,?,1,'0','0')`, recordZoneID2, "other.test")
	settingsExec(t, db, `insert into certificates (id,hostnames,issuer,challenge,environment,status,certificate_pem,created_at,updated_at)
		values (?,'["*.example.test"]','uploaded','none','custom','issued','fixture PEM','0','2')`, recordCertID)
	store := secrets.NewSQLiteStore(db, recordMasterKey)
	return &recordFixture{db: db, path: path, secrets: store, store: NewSQLiteRecordWriteStore(settingsWriter(db), store)}
}

func (f *recordFixture) seedRecord(t *testing.T, id, zone, name, value string) {
	t.Helper()
	settingsExec(t, f.db, `insert into dns_records (id,zone_id,name,type,value,ttl,enabled,proxied,created_at,updated_at)
		values (?,?,?,'A',?,300,1,0,'0','0')`, id, zone, name, `"`+value+`"`)
}

func recordA(name, value string) domain.DNSRecordInput {
	return domain.DNSRecordInput{Name: name, Type: "A", Value: value, Enabled: true}
}

func recordProxied(basicAuth any) domain.DNSRecordInput {
	proxy := map[string]any{
		"origin":        map[string]any{"ip": "192.0.2.10", "port": 8080, "protocol": "http"},
		"tlsEnabled":    true,
		"certificateId": recordCertID,
		"websocket":     true,
	}
	if basicAuth != nil {
		proxy["basicAuth"] = basicAuth
	}
	input := recordA("www", "192.0.2.10")
	input.Proxied, input.Proxy = true, proxy
	return input
}

// recordState captures every table AddRecord may touch so rollback is total.
func recordState(t *testing.T, db *sql.DB) string {
	t.Helper()
	var out strings.Builder
	for _, query := range []string{
		`select json_group_array(json_array(id,zone_id,name,type,value,ttl,enabled,proxied,proxy_settings,comment,created_at,updated_at)) from (select * from dns_records order by id)`,
		`select json_group_array(json_array(id,purpose,ciphertext,created_at,updated_at)) from (select * from secrets order by id)`,
		`select json_group_array(json_array(id,name,enabled,created_at,updated_at)) from (select * from zones order by id)`,
		`select json_group_array(json_array(id,revision_number,checksum,snapshot,created_at)) from (select * from config_revisions order by id)`,
		`select json_group_array(json_array(id,revision_id,status,created_at)) from (select * from apply_jobs order by id)`,
	} {
		var raw string
		if err := db.QueryRow(query).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		out.WriteString(raw)
	}
	return out.String() + snapshotSettingsBytes(t, db)
}

func recordZero(t *testing.T, got AutoApplyResult[domain.DNSRecord], err error) {
	t.Helper()
	if err == nil || !reflect.DeepEqual(got, AutoApplyResult[domain.DNSRecord]{}) {
		t.Fatalf("expected error and zero result: %+v, %v", got, err)
	}
}

func recordExpectRollback(t *testing.T, f *recordFixture, before string, input domain.DNSRecordInput, ctx context.Context) error {
	t.Helper()
	got, err := f.store.AddRecord(ctx, recordZoneID, input, "")
	recordZero(t, got, err)
	if recordState(t, f.db) != before {
		t.Fatal("state changed across a failed AddRecord")
	}
	return err
}

func TestSQLiteAddRecordValidationPriority(t *testing.T) {
	longPassword := recordProxied(map[string]any{"username": "alice", "password": "short"})
	longPassword.Type = "BAD"
	blankNoSecret := recordProxied(map[string]any{"username": "alice"})
	blankNoSecret.Type = "BAD"
	passwordBadType := recordProxied(map[string]any{"username": "alice", "password": recordPassword})
	passwordBadType.Type = "BAD"
	ttl := 1
	badTTL := recordA("www", "192.0.2.5")
	badTTL.TTL = &ttl
	proxiedTXT := recordProxied(nil)
	proxiedTXT.Type, proxiedTXT.Value = "TXT", "text"
	proxiedNoProxy := recordA("www", "192.0.2.5")
	proxiedNoProxy.Proxied = true

	cases := []struct {
		name    string
		zone    string
		input   domain.DNSRecordInput
		prepare func(*testing.T, *recordFixture)
		want    string
	}{
		{"zone not found beats invalid type", "aaaaaaaa-0000-4000-8000-0000000000ff", passwordBadType, nil, "Zone not found"},
		{"zone not found beats password error", "aaaaaaaa-0000-4000-8000-0000000000ff", longPassword, nil, "Zone not found"},
		{"malformed zone id", "not-a-uuid", recordA("www", "192.0.2.5"), nil, "invalid UUID lookup text"},
		{"hash error beats invalid type", recordZoneID, longPassword, nil, "Basic Auth password must contain at least 8 characters"},
		{"missing password beats invalid type", recordZoneID, blankNoSecret, nil, "Basic Auth password is required"},
		{"invalid type", recordZoneID, domain.DNSRecordInput{Name: "www", Type: "BAD", Value: "x"}, nil, "Unsupported record type: BAD"},
		{"invalid ttl", recordZoneID, badTTL, nil, fmt.Sprintf("TTL must be between %d and %d", domain.MinTTL, domain.MaxTTL)},
		{"proxied txt", recordZoneID, proxiedTXT, nil, "TXT records cannot be proxied"},
		{"proxied without proxy settings", recordZoneID, proxiedNoProxy, nil, "Proxied records require proxy settings"},
		{"proxied without ingress", recordZoneID, recordProxied(nil), func(t *testing.T, f *recordFixture) {
			settingsExec(t, f.db, "update installation_settings set ingress_ipv4=NULL")
		}, "IPv4 proxy ingress is required"},
		{"invalid record beats missing pool", recordZoneID, domain.DNSRecordInput{Name: "www", Type: "BAD", Value: "x"}, func(t *testing.T, f *recordFixture) {
			settingsExec(t, f.db, "update installation_settings set default_resolver_pool=NULL")
		}, "Unsupported record type: BAD"},
		{"valid record with missing pool", recordZoneID, recordA("www", "192.0.2.5"), func(t *testing.T, f *recordFixture) {
			settingsExec(t, f.db, "update installation_settings set default_resolver_pool=NULL")
		}, "Configure a default resolver pool before apply"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newRecordFixture(t)
			if tc.prepare != nil {
				tc.prepare(t, f)
			}
			before := recordState(t, f.db)
			got, err := f.store.AddRecord(createZoneContext(t), tc.zone, tc.input, "")
			recordZero(t, got, err)
			if err.Error() != tc.want {
				t.Fatalf("error = %q, want %q", err, tc.want)
			}
			if recordState(t, f.db) != before {
				t.Fatal("failed AddRecord changed state (including secrets)")
			}
		})
	}
}

func TestSQLiteAddRecordCommittedState(t *testing.T) {
	f := newRecordFixture(t)
	comment := "note"
	input := recordA("WWW", "192.0.2.5")
	input.Comment = &comment
	got, err := f.store.AddRecord(createZoneContext(t), recordZoneID, input, "")
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := sqlitedb.ParseUUIDLookupText(got.Value.ID)
	if err != nil || canonical != got.Value.ID || got.Value.ID[14] != '4' {
		t.Fatal("generated id", got.Value.ID, err)
	}
	want := domain.DNSRecord{ID: got.Value.ID, Name: "www.example.test", Type: "A", Value: "192.0.2.5", TTL: domain.DefaultTTL, Enabled: true, Comment: &comment}
	if !reflect.DeepEqual(got.Value, want) {
		t.Fatalf("value = %+v", got.Value)
	}
	reopened, err := sqlitedb.Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	stored, err := NewSQLiteZoneReadStore(reopened).GetZone(createZoneContext(t), recordZoneID)
	if err != nil || len(stored.Records) != 1 || domain.StableStringify(stored.Records[0]) != domain.StableStringify(got.Value) {
		t.Fatal("stored zone", stored, err)
	}
	stamp, _ := sqlitedb.EncodeTimestamp(sqlitedb.FiniteTimestamp{Seconds: 123, Nanos: 456})
	var created, updated, jobRevision string
	if err := reopened.QueryRow(`select created_at,updated_at from dns_records where id=?`, got.Value.ID).Scan(&created, &updated); err != nil {
		t.Fatal(err)
	}
	if err := reopened.QueryRow(`select revision_id from apply_jobs where id=?`, got.Apply.Job.ID).Scan(&jobRevision); err != nil {
		t.Fatal(err)
	}
	var desired string
	if err := reopened.QueryRow(`select current_desired_revision_id from installation_settings`).Scan(&desired); err != nil {
		t.Fatal(err)
	}
	if created != *stamp || updated != *stamp || jobRevision != got.Apply.RevisionID || desired != got.Apply.RevisionID || got.Apply.Job.Status != "queued" {
		t.Fatal("clock or apply linkage", created, updated, jobRevision, desired)
	}
	snapshot, err := NewSQLiteSnapshotStore(reopened, settingsWriter(reopened)).Snapshot(createZoneContext(t))
	var raw string
	reopened.QueryRow(`select snapshot from config_revisions where id=?`, got.Apply.RevisionID).Scan(&raw)
	if err != nil || raw != domain.StableStringify(snapshot) {
		t.Fatal("revision snapshot does not contain the new record", err)
	}
	for _, table := range []string{"config_revisions", "apply_jobs"} {
		if enqueueCount(t, reopened, table) != 1 {
			t.Fatal("expected exactly one", table)
		}
	}
	if enqueueCount(t, reopened, "secrets") != 0 {
		t.Fatal("no secret expected without basicAuth")
	}
}

func TestSQLiteAddRecordPatchRewritesZoneAndKeepsCreatedAt(t *testing.T) {
	f := newRecordFixture(t)
	f.seedRecord(t, recordExistID, recordZoneID, "a.example.test", "192.0.2.1")
	f.seedRecord(t, recordExistID2, recordZoneID, "b.example.test", "192.0.2.2")
	f.seedRecord(t, "eeeeeeee-0000-4000-8000-000000000003", recordZoneID2, "c.other.test", "192.0.2.3")
	input := recordA("a", "192.0.2.99")
	input.ID = recordExistID
	got, err := f.store.AddRecord(createZoneContext(t), recordZoneID, input, "")
	if err != nil || got.Value.ID != recordExistID || got.Value.Value != "192.0.2.99" {
		t.Fatal(got, err)
	}
	stamp, _ := sqlitedb.EncodeTimestamp(sqlitedb.FiniteTimestamp{Seconds: 123, Nanos: 456})
	rows, err := f.db.Query(`select id,value,created_at,updated_at from dns_records order by id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type row struct{ id, value, created, updated string }
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.value, &r.created, &r.updated); err != nil {
			t.Fatal(err)
		}
		all = append(all, r)
	}
	want := []row{
		{recordExistID, `"192.0.2.99"`, "0", *stamp},
		{recordExistID2, `"192.0.2.2"`, "0", *stamp},
		{"eeeeeeee-0000-4000-8000-000000000003", `"192.0.2.3"`, "0", "0"},
	}
	if !reflect.DeepEqual(all, want) {
		t.Fatalf("rows = %+v", all)
	}
}

func TestSQLiteAddRecordSameIDInOtherZoneKeepsItsZone(t *testing.T) {
	// PG parity: the conflict update never rewrites zone_id, so a caller that
	// reuses an id from another zone updates that zone's row in place.
	f := newRecordFixture(t)
	f.seedRecord(t, recordExistID, recordZoneID2, "c.other.test", "192.0.2.3")
	input := recordA("moved", "192.0.2.77")
	input.ID = recordExistID
	if _, err := f.store.AddRecord(createZoneContext(t), recordZoneID, input, ""); err != nil {
		t.Fatal(err)
	}
	var zone, name string
	if err := f.db.QueryRow(`select zone_id,name from dns_records where id=?`, recordExistID).Scan(&zone, &name); err != nil {
		t.Fatal(err)
	}
	if zone != recordZoneID2 || name != "moved.example.test" {
		t.Fatal(zone, name)
	}
}

func TestSQLiteAddRecordUppercaseZoneAliasMirrorsCallerSpelledGrouping(t *testing.T) {
	// PG groups records by canonical zone_id::text and then indexes the map with
	// the caller's spelling, so an uppercase alias sees no sibling records.
	f := newRecordFixture(t)
	f.seedRecord(t, recordExistID, recordZoneID, "a.example.test", "192.0.2.1")
	if _, err := f.store.AddRecord(createZoneContext(t), strings.ToUpper(recordZoneID), recordA("b", "192.0.2.2"), ""); err != nil {
		t.Fatal(err)
	}
	var updated, zone string
	if err := f.db.QueryRow(`select updated_at,zone_id from dns_records where id=?`, recordExistID).Scan(&updated, &zone); err != nil {
		t.Fatal(err)
	}
	var added string
	if err := f.db.QueryRow(`select zone_id from dns_records where id<>?`, recordExistID).Scan(&added); err != nil {
		t.Fatal(err)
	}
	if updated != "0" || zone != recordZoneID || added != recordZoneID {
		t.Fatal("alias must canonicalize the stored zone and skip sibling rewrite", updated, zone, added)
	}
}

func TestSQLiteAddRecordColumnEncodings(t *testing.T) {
	f := newRecordFixture(t)
	empty := ""
	for i, tc := range []struct {
		enabled, proxied bool
		comment          *string
	}{{true, false, nil}, {false, false, &empty}, {true, true, nil}, {false, true, &empty}} {
		input := recordA(fmt.Sprintf("r%d", i), "192.0.2.5")
		if tc.proxied {
			input = recordProxied(nil)
			input.Name = fmt.Sprintf("r%d", i)
		}
		input.Enabled, input.Comment = tc.enabled, tc.comment
		got, err := f.store.AddRecord(createZoneContext(t), recordZoneID, input, "")
		if err != nil {
			t.Fatal(err)
		}
		var enabled, proxied int
		var comment, proxy *string
		var typ string
		if err := f.db.QueryRow(`select enabled,proxied,comment,proxy_settings,typeof(enabled)||typeof(proxied) from dns_records where id=?`, got.Value.ID).
			Scan(&enabled, &proxied, &comment, &proxy, &typ); err != nil {
			t.Fatal(err)
		}
		if (enabled == 1) != tc.enabled || (proxied == 1) != tc.proxied || typ != "integerinteger" || (comment == nil) != (tc.comment == nil) ||
			(proxy != nil) != tc.proxied || (comment != nil && *comment != "") {
			t.Fatal("encoding", tc, enabled, proxied, comment, proxy)
		}
	}
	// PATCH may clear a comment back to NULL.
	set := "set"
	input := recordA("patched", "192.0.2.5")
	input.Comment = &set
	first, err := f.store.AddRecord(createZoneContext(t), recordZoneID, input, "")
	if err != nil {
		t.Fatal(err)
	}
	input.ID, input.Comment = first.Value.ID, nil
	if _, err := f.store.AddRecord(createZoneContext(t), recordZoneID, input, ""); err != nil {
		t.Fatal(err)
	}
	var comment *string
	if err := f.db.QueryRow(`select comment from dns_records where id=?`, first.Value.ID).Scan(&comment); err != nil || comment != nil {
		t.Fatal("comment not cleared", comment, err)
	}
}

func TestSQLiteAddRecordBasicAuthSecret(t *testing.T) {
	ctx := createZoneContext(t)
	f := newRecordFixture(t)
	auth := map[string]any{"username": "alice", "password": recordPassword}
	input := recordProxied(auth)
	got, err := f.store.AddRecord(ctx, recordZoneID, input, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, stillThere := auth["password"]; !stillThere || input.Proxy["basicAuth"].(map[string]any)["password"] != recordPassword {
		t.Fatal("caller input was mutated")
	}
	resolved := got.Value.Proxy["basicAuth"].(map[string]any)
	secretID, _ := resolved["passwordSecretId"].(string)
	if _, hasPassword := resolved["password"]; hasPassword || resolved["username"] != "alice" || secretID == "" || got.Value.Proxy["websocket"] != true {
		t.Fatalf("resolved proxy = %+v", got.Value.Proxy)
	}
	hash, err := secrets.HashBasicAuthPassword(recordPassword)
	if err != nil {
		t.Fatal(err)
	}
	var purpose, ciphertext string
	if err := f.db.QueryRow(`select purpose,ciphertext from secrets where id=?`, secretID).Scan(&purpose, &ciphertext); err != nil {
		t.Fatal(err)
	}
	if purpose != "basic-auth-password" || !strings.HasPrefix(ciphertext, "v1:") || enqueueCount(t, f.db, "secrets") != 1 {
		t.Fatal("secret row", purpose)
	}
	if plain, err := f.secrets.Get(ctx, secretID); err != nil || plain != hash {
		t.Fatal("stored secret is not the basic-auth hash", err)
	}
	// No password text, hash, or plaintext anywhere at rest or in the result.
	for _, query := range []string{`select group_concat(proxy_settings) from dns_records`, `select group_concat(ciphertext) from secrets`, `select group_concat(snapshot) from config_revisions`} {
		var blob string
		if err := f.db.QueryRow(query).Scan(&blob); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(blob, recordPassword) || strings.Contains(blob, hash) || strings.Contains(blob, "{SHA}") {
			t.Fatal("password material at rest in", query)
		}
	}
	if strings.Contains(fmt.Sprintf("%+v", got), recordPassword) || strings.Contains(fmt.Sprintf("%+v", got), hash) {
		t.Fatal("password material in returned value")
	}

	t.Run("blank password reuses the id from input", func(t *testing.T) {
		in := recordProxied(map[string]any{"username": "bob", "passwordSecretId": secretID})
		in.ID = got.Value.ID
		res, err := f.store.AddRecord(ctx, recordZoneID, in, "")
		if err != nil || res.Value.Proxy["basicAuth"].(map[string]any)["passwordSecretId"] != secretID || enqueueCount(t, f.db, "secrets") != 1 {
			t.Fatal(res, err)
		}
	})
	t.Run("blank password reuses the existing record secret on PATCH", func(t *testing.T) {
		in := recordProxied(map[string]any{"username": "carol"})
		in.ID = got.Value.ID
		res, err := f.store.AddRecord(ctx, recordZoneID, in, "")
		auth := res.Value.Proxy["basicAuth"].(map[string]any)
		if err != nil || auth["passwordSecretId"] != secretID || auth["username"] != "carol" || enqueueCount(t, f.db, "secrets") != 1 {
			t.Fatal(res, err)
		}
	})
	t.Run("new password creates a new secret and keeps the old one", func(t *testing.T) {
		in := recordProxied(map[string]any{"username": "alice", "password": "synthetic-password-2"})
		in.ID = got.Value.ID
		res, err := f.store.AddRecord(ctx, recordZoneID, in, "")
		newID := res.Value.Proxy["basicAuth"].(map[string]any)["passwordSecretId"]
		if err != nil || newID == secretID || enqueueCount(t, f.db, "secrets") != 2 {
			t.Fatal(res, err)
		}
		if plain, err := f.secrets.Get(ctx, secretID); err != nil || plain != hash {
			t.Fatal("old secret was disturbed", err)
		}
	})
	t.Run("unknown secret id", func(t *testing.T) {
		const id = "bbbbbbbb-0000-4000-8000-000000000009"
		before := recordState(t, f.db)
		err := recordExpectRollback(t, f, before, recordProxied(map[string]any{"username": "alice", "passwordSecretId": id}), ctx)
		if err.Error() != "Basic Auth secret not found: "+id {
			t.Fatal(err)
		}
	})
	t.Run("malformed secret id", func(t *testing.T) {
		before := recordState(t, f.db)
		err := recordExpectRollback(t, f, before, recordProxied(map[string]any{"username": "alice", "passwordSecretId": "not-a-uuid"}), ctx)
		if !strings.Contains(err.Error(), "invalid input syntax for type uuid") {
			t.Fatal(err)
		}
	})
	t.Run("non-map basicAuth is dropped", func(t *testing.T) {
		in := recordProxied("hunter2-synthetic")
		in.Name = "dropped"
		res, err := f.store.AddRecord(ctx, recordZoneID, in, "")
		if err != nil {
			t.Fatal(err)
		}
		if _, has := res.Value.Proxy["basicAuth"]; has || res.Value.Proxy["websocket"] != true {
			t.Fatalf("proxy = %+v", res.Value.Proxy)
		}
	})
}

func TestSQLiteAddRecordBasicAuthNilSecretStore(t *testing.T) {
	ctx := createZoneContext(t)
	f := newRecordFixture(t)
	nilStore := NewSQLiteRecordWriteStore(settingsWriter(f.db), nil)
	before := recordState(t, f.db)
	got, err := nilStore.AddRecord(ctx, recordZoneID, recordProxied(map[string]any{"username": "alice", "password": recordPassword}), "")
	recordZero(t, got, err)
	if err.Error() != "Basic Auth requires a configured master key" || recordState(t, f.db) != before || strings.Contains(err.Error(), recordPassword) {
		t.Fatal(err)
	}
	// PG parity: without a store, a blank-password reference is not verified.
	const id = "bbbbbbbb-0000-4000-8000-000000000009"
	res, err := nilStore.AddRecord(ctx, recordZoneID, recordProxied(map[string]any{"username": "alice", "passwordSecretId": id}), "")
	if err != nil || res.Value.Proxy["basicAuth"].(map[string]any)["passwordSecretId"] != id {
		t.Fatal(res, err)
	}
	// A typed-nil store fails closed instead of panicking.
	var typedNil *secrets.SQLiteStore
	typed := NewSQLiteRecordWriteStore(settingsWriter(f.db), typedNil)
	before = recordState(t, f.db)
	got, err = typed.AddRecord(ctx, recordZoneID, recordProxied(map[string]any{"username": "alice", "password": recordPassword}), "")
	recordZero(t, got, err)
	if recordState(t, f.db) != before {
		t.Fatal("typed-nil secret store changed state")
	}
}

func TestSQLiteAddRecordStagedRollbackIncludesSecret(t *testing.T) {
	for _, stage := range []string{"secret", "record", "revision", "pointer", "job", "scan", "cancel"} {
		t.Run(stage, func(t *testing.T) {
			f := newRecordFixture(t)
			f.db.SetMaxOpenConns(1)
			f.seedRecord(t, recordExistID, recordZoneID, "a.example.test", "192.0.2.1")
			before := recordState(t, f.db)
			ctx, cancel := context.WithCancel(createZoneContext(t))
			defer cancel()
			store := f.store
			trigger := map[string]string{
				"secret": "after insert on secrets", "record": "after insert on dns_records",
				"revision": "after insert on config_revisions", "job": "after insert on apply_jobs",
				"pointer": "after update of current_desired_revision_id on installation_settings",
			}[stage]
			if trigger != "" {
				settingsExec(t, f.db, "create temp trigger add_record_failure "+trigger+" begin select raise(ABORT,'injected failure'); end")
			}
			if stage == "scan" || stage == "cancel" {
				calls := 0
				writer := sqlitedb.NewWriter(f.db, func() sqlitedb.FiniteTimestamp {
					calls++
					if stage == "cancel" {
						cancel()
						return sqlitedb.FiniteTimestamp{}
					}
					return sqlitedb.FiniteTimestamp{Seconds: math.MaxInt64, Nanos: 99}
				})
				store = NewSQLiteRecordWriteStore(writer, f.secrets)
			}
			got, err := store.AddRecord(ctx, recordZoneID, recordProxied(map[string]any{"username": "alice", "password": recordPassword}), "")
			recordZero(t, got, err)
			if stage == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if stage == "secret" && !strings.Contains(err.Error(), "injected failure") {
				t.Fatal("secret insert did not fail", err)
			}
			if recordState(t, f.db) != before {
				t.Fatal("state (including secrets) changed across rollback")
			}
			for _, table := range []string{"secrets", "config_revisions", "apply_jobs"} {
				if enqueueCount(t, f.db, table) != 0 {
					t.Fatal("rollback left rows in", table)
				}
			}
			if trigger != "" {
				settingsExec(t, f.db, "drop trigger add_record_failure")
			}
			if _, err := f.store.AddRecord(createZoneContext(t), recordZoneID, recordA("ok", "192.0.2.5"), ""); err != nil {
				t.Fatal("connection did not recover", err)
			}
		})
	}
}

func TestSQLiteAddRecordCanceledAndClosed(t *testing.T) {
	f := newRecordFixture(t)
	before := recordState(t, f.db)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	err := recordExpectRollback(t, f, before, recordA("www", "192.0.2.5"), canceled)
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := f.db.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := f.store.AddRecord(createZoneContext(t), recordZoneID, recordA("www", "192.0.2.5"), "")
	recordZero(t, got, err)
}

func TestSQLiteAddRecordIndependentContention(t *testing.T) {
	f := newRecordFixture(t)
	other, err := sqlitedb.Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	f.db.SetMaxOpenConns(1)
	other.SetMaxOpenConns(1)
	stores := []*SQLiteRecordWriteStore{f.store, NewSQLiteRecordWriteStore(settingsWriter(other), secrets.NewSQLiteStore(other, recordMasterKey))}
	ctx := createZoneContext(t)
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make([]AutoApplyResult[domain.DNSRecord], 2)
	errs := make([]error, 2)
	for i := range stores {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], errs[i] = stores[i].AddRecord(ctx, recordZoneID, recordA(fmt.Sprintf("host%d", i), fmt.Sprintf("192.0.2.%d", 10+i)), "")
		}(i)
	}
	close(start)
	wg.Wait()
	if errs[0] != nil || errs[1] != nil || results[0].Value.ID == results[1].Value.ID {
		t.Fatal("serialized writers must both commit", errs)
	}
	if enqueueCount(t, other, "dns_records") != 2 || enqueueCount(t, other, "config_revisions") != 2 || enqueueCount(t, other, "apply_jobs") != 2 {
		t.Fatal("expected both records, revisions, and jobs")
	}
}
