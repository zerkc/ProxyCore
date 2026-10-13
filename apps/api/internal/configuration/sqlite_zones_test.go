package configuration

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

var _ interface {
	GetZone(context.Context, string) (domain.ZoneState, error)
} = (*SQLiteZoneReadStore)(nil)

const sqliteZoneID = "abcdef01-2345-0678-0123-456789abcdef"

func seedSQLiteZone(t *testing.T, db *sql.DB, id, name string, enabled int) {
	t.Helper()
	settingsExec(t, db, "insert into zones values (?,?,?,'0','2')", id, name, enabled)
}

func seedSQLiteRecord(t *testing.T, db *sql.DB, n int, zone, name, typ, value string, enabled, proxied int, proxy, comment any) string {
	t.Helper()
	id := fmt.Sprintf("00000000-0000-0000-0000-%012x", n)
	settingsExec(t, db, "insert into dns_records values (?,?,?,?,?,-1,?,?,?,?,'2','0')", id, zone, name, typ, value, enabled, proxied, proxy, comment)
	return id
}

func TestSQLiteZonesLookup(t *testing.T) {
	db, _, _ := sqliteSettingsFixture(t)
	ctx := context.Background()
	store := NewSQLiteZoneReadStore(db)
	seedSQLiteZone(t, db, sqliteZoneID, "Unnormalized Zone.", 0)
	got, err := store.GetZone(ctx, sqliteZoneID)
	want := domain.ZoneState{ID: sqliteZoneID, Name: "Unnormalized Zone.", Records: []domain.DNSRecord{}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatal(got, err)
	}
	seedSQLiteRecord(t, db, 1, sqliteZoneID, "record", "A", `"not an IP"`, 0, 1, nil, nil)
	for _, id := range []string{sqliteZoneID, strings.ToUpper(sqliteZoneID), strings.ReplaceAll(sqliteZoneID, "-", ""), "{" + sqliteZoneID + "}", "abcd-ef01234506780123456789abcdef"} {
		t.Run(id, func(t *testing.T) {
			got, err := store.GetZone(ctx, id)
			if err != nil || got.ID != want.ID || got.Name != want.Name || got.Enabled || got.Records == nil {
				t.Fatal(got, err)
			}
			// Every accepted spelling sees the zone's records, like the canonical one.
			if len(got.Records) != 1 {
				t.Fatal(got.Records)
			}
		})
	}
	for _, id := range []string{"00000000-0000-0000-ffff-000000000000", "{0000000000000000FFFF000000000000}"} {
		t.Run("missing "+id, func(t *testing.T) {
			if _, err := store.GetZone(ctx, id); err == nil || err.Error() != "Zone not found" {
				t.Fatal(err)
			}
		})
	}
	for _, id := range []string{"", "bad", " " + sqliteZoneID, sqliteZoneID + "-", "{" + sqliteZoneID} {
		t.Run("invalid "+id, func(t *testing.T) {
			if _, err := store.GetZone(ctx, id); err == nil || err.Error() == "Zone not found" {
				t.Fatal(err)
			}
		})
	}
}

func TestSQLiteZonesProjection(t *testing.T) {
	db, _, _ := sqliteSettingsFixture(t)
	seedSQLiteZone(t, db, sqliteZoneID, "example.test", 1)
	other := "00000000-0000-0000-ffff-000000000001"
	seedSQLiteZone(t, db, other, "other", 0)
	seedSQLiteRecord(t, db, 99, other, "a", "A", `null`, 1, 0, `[]`, nil)
	cases := []struct {
		name, typ, raw string
		value          any
		proxy, comment any
	}{
		{"z", "A", `"bad IP"`, "bad IP", nil, nil},
		{"a", "AAAA", `null`, nil, `null`, ""},
		{"m", "CNAME", `"UPPER.invalid."`, "UPPER.invalid.", `{"n":9007199254740993}`, "keep"},
		{"b", "TXT", `[1,"raw",null]`, []any{float64(1), "raw", nil}, nil, nil},
		{"x", "MX", `{"priority":12,"host":"mx"}`, map[string]any{"priority": float64(12), "host": "mx"}, nil, nil},
		{"c", "SRV", `42`, float64(42), nil, nil},
	}
	want := make(map[string]domain.DNSRecord)
	for i, tc := range cases {
		id := seedSQLiteRecord(t, db, i+1, sqliteZoneID, tc.name, tc.typ, tc.raw, i%2, (i+1)%2, tc.proxy, tc.comment)
		r := domain.DNSRecord{ID: id, Name: tc.name, Type: tc.typ, Value: tc.value, TTL: -1, Enabled: i%2 == 1, Proxied: i%2 == 0}
		if tc.comment != nil {
			comment := tc.comment.(string)
			r.Comment = &comment
		}
		if tc.typ == "CNAME" {
			r.Proxy = map[string]any{"n": float64(9007199254740993)}
		}
		want[tc.name] = r
	}
	got, err := NewSQLiteZoneReadStore(db).GetZone(context.Background(), sqliteZoneID)
	if err != nil || !got.Enabled || got.ID != sqliteZoneID || got.Name != "example.test" || len(got.Records) != len(cases) {
		t.Fatal(got, err)
	}
	// ASCII order is not proof of PostgreSQL collation equivalence.
	for i, name := range []string{"a", "b", "c", "m", "x", "z"} {
		if !reflect.DeepEqual(got.Records[i], want[name]) {
			t.Fatalf("record %s: %#v != %#v", name, got.Records[i], want[name])
		}
	}
	// Equal-name ties have no promised secondary ordering.
	seedSQLiteRecord(t, db, 7, sqliteZoneID, "a", "TXT", `true`, 1, 1, `{}`, nil)
	got, err = NewSQLiteZoneReadStore(db).GetZone(context.Background(), sqliteZoneID)
	if err != nil || len(got.Records) != 7 || got.Records[0].Name != "a" || got.Records[1].Name != "a" {
		t.Fatal(got, err)
	}
}

func TestSQLiteZonesDecodeRecovery(t *testing.T) {
	db, _, _ := sqliteSettingsFixture(t)
	db.SetMaxOpenConns(1)
	seedSQLiteZone(t, db, sqliteZoneID, "zone", 1)
	seedSQLiteRecord(t, db, 1, sqliteZoneID, "one", "A", `null`, 1, 0, nil, nil)
	store := NewSQLiteZoneReadStore(db)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, proxy := range []string{`[]`, `[{}]`, `42`, `"scalar"`, `true`} {
		t.Run(proxy, func(t *testing.T) {
			settingsExec(t, db, "update dns_records set proxy_settings=?", proxy)
			for _, id := range []string{sqliteZoneID, strings.ToUpper(sqliteZoneID)} {
				if _, err := store.GetZone(ctx, id); err == nil {
					t.Fatal("proxy shape accepted", proxy, id)
				}
			}
			settingsExec(t, db, "update dns_records set proxy_settings=NULL")
			if got, err := store.GetZone(ctx, sqliteZoneID); err != nil || len(got.Records) != 1 {
				t.Fatal(got, err)
			}
			if db.Stats().InUse != 0 {
				t.Fatal("reader retained connection")
			}
		})
	}
	// Synthetic projections exercise impossible stored bool/JSON values without
	// disabling STRICT, CHECK constraints or foreign keys in the real schema.
	for _, projection := range []string{"2,0,'null','null'", "1,-1,'null','null'", "1,0,'{','null'", "1,0,'null','{'"} {
		t.Run(projection, func(t *testing.T) {
			parts := strings.Split(projection, ",")
			query := "select 'id','zone','name','A'," + parts[2] + ",1," + parts[0] + "," + parts[1] + "," + parts[3] + ",NULL"
			if _, _, err := scanRecord(db.QueryRowContext(ctx, query)); err == nil {
				t.Fatal("invalid projection accepted")
			}
		})
	}
	cancel()
	if _, err := store.GetZone(ctx, sqliteZoneID); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := store.GetZone(context.Background(), sqliteZoneID); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetZone(context.Background(), sqliteZoneID); err == nil {
		t.Fatal("closed database accepted")
	}
}

func TestSQLiteZonesStorageAndVisibility(t *testing.T) {
	db, _, path := sqliteSettingsFixture(t)
	seedSQLiteZone(t, db, sqliteZoneID, "Stored Zone.", 0)
	raw := ` {"number":9007199254740993,"keep":[null]} `
	id := seedSQLiteRecord(t, db, 1, sqliteZoneID, " BAD Name. ", "MX", raw, 0, 1, ` {"raw":1} `, "")
	ctx := context.Background()
	store := NewSQLiteZoneReadStore(db)
	before, err := store.GetZone(ctx, sqliteZoneID)
	if err != nil || before.Records[0].Name != " BAD Name. " || before.Records[0].TTL != -1 {
		t.Fatal(before, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := sqlitedb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	store = NewSQLiteZoneReadStore(reopened)
	after, err := store.GetZone(ctx, sqliteZoneID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal(after, err)
	}
	var rid, zid, value, proxy, rc, ru, zc, zu string
	if err := reopened.QueryRow(`select r.id,r.zone_id,r.value,r.proxy_settings,r.created_at,r.updated_at,z.created_at,z.updated_at from dns_records r join zones z on z.id=r.zone_id`).Scan(&rid, &zid, &value, &proxy, &rc, &ru, &zc, &zu); err != nil {
		t.Fatal(err)
	}
	if rid != id || zid != sqliteZoneID || value != raw || proxy != ` {"raw":1} ` || rc != "2" || ru != "0" || zc != "0" || zu != "2" {
		t.Fatal("read mutated storage", rid, zid, value, proxy, rc, ru, zc, zu)
	}
	other, err := sqlitedb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	settingsExec(t, other, "update zones set name='changed',enabled=1 where id=?", sqliteZoneID)
	settingsExec(t, other, "update dns_records set value='null',enabled=1,proxied=0 where id=?", id)
	after, err = store.GetZone(ctx, sqliteZoneID)
	if err != nil || after.Name != "changed" || !after.Enabled || !after.Records[0].Enabled || after.Records[0].Proxied || after.Records[0].Value != nil {
		t.Fatal(after, err)
	}
	settingsExec(t, other, "delete from dns_records where id=?", id)
	after, err = store.GetZone(ctx, sqliteZoneID)
	if err != nil || after.Records == nil || len(after.Records) != 0 {
		t.Fatal(after, err)
	}
	seedSQLiteRecord(t, other, 2, sqliteZoneID, "cascade", "TXT", `null`, 1, 0, nil, nil)
	settingsExec(t, other, "delete from zones where id=?", sqliteZoneID)
	if _, err := store.GetZone(ctx, sqliteZoneID); err == nil || err.Error() != "Zone not found" {
		t.Fatal(err)
	}
	var count int
	if err := reopened.QueryRow("select count(*) from dns_records").Scan(&count); err != nil || count != 0 {
		t.Fatal("cascade missing", count, err)
	}
	if _, err := other.Exec("insert into dns_records values (?,?, 'orphan','A','null',1,1,0,NULL,NULL,'0','0')", id, sqliteZoneID); err == nil {
		t.Fatal("foreign key accepted orphan")
	}
}

func TestSQLiteZonesBooleanAndUUIDGrid(t *testing.T) {
	db, _, _ := sqliteSettingsFixture(t)
	ctx := context.Background()
	store := NewSQLiteZoneReadStore(db)
	ids := []string{
		"00000000-0000-0000-0000-000000000000",
		"ffffffff-ffff-ffff-ffff-ffffffffffff",
		"01234567-89ab-1cde-7fab-0123456789ab",
		"01234567-89ab-6cde-cfab-0123456789ab",
	}
	for i, id := range ids {
		t.Run(id, func(t *testing.T) {
			enabled, proxied := i/2, i%2
			seedSQLiteZone(t, db, id, fmt.Sprintf("zone%d", i), enabled)
			seedSQLiteRecord(t, db, i+1, id, "record", "TXT", `null`, enabled, proxied, nil, nil)
			got, err := store.GetZone(ctx, id)
			if err != nil || got.ID != id || got.Enabled != (enabled == 1) || len(got.Records) != 1 {
				t.Fatal(got, err)
			}
			if r := got.Records[0]; r.Enabled != (enabled == 1) || r.Proxied != (proxied == 1) {
				t.Fatal(r)
			}
		})
	}
	if _, err := db.Exec("insert into zones values (?, 'alias',1,'0','0')", strings.ToUpper(ids[1])); err == nil {
		t.Fatal("schema allowed noncanonical stored UUID")
	}
	for _, table := range []string{"installation_settings", "apply_jobs", "audit_events"} {
		var count int
		if err := db.QueryRow("select count(*) from " + table).Scan(&count); err != nil || count != 0 {
			t.Fatal("reader created unrelated state", table, count, err)
		}
	}
}

var _ interface {
	ListZones(context.Context) ([]domain.ZoneState, error)
} = (*SQLiteZoneReadStore)(nil)

func TestSQLiteZonesListEmpty(t *testing.T) {
	db, _, _ := sqliteSettingsFixture(t)
	got, err := NewSQLiteZoneReadStore(db).ListZones(context.Background())
	if err != nil || got == nil || len(got) != 0 {
		t.Fatal(got, err)
	}
}

func TestSQLiteZonesListProjection(t *testing.T) {
	db, _, _ := sqliteSettingsFixture(t)
	db.SetMaxOpenConns(1)
	other := "00000000-0000-0000-ffff-000000000001"
	empty := "ffffffff-ffff-ffff-ffff-ffffffffffff"
	seedSQLiteZone(t, db, sqliteZoneID, "z Stored Zone.", 0)
	seedSQLiteZone(t, db, other, "a other", 1)
	seedSQLiteZone(t, db, empty, "m empty", 0)
	comment := "keep"
	cases := []struct {
		zone, name, typ, raw string
		enabled, proxied     int
		proxy, comment       any
		value                any
	}{
		{sqliteZoneID, "z BAD Name.", "A", `"not an IP"`, 0, 1, nil, nil, "not an IP"},
		{other, "shared", "MX", ` {"n":9007199254740993} `, 1, 0, ` {"n":9007199254740993} `, comment, map[string]any{"n": float64(9007199254740993)}},
		{sqliteZoneID, "shared", "TXT", `null`, 0, 0, `null`, "", nil},
		{other, "a scalar", "SRV", `42`, 0, 1, nil, nil, float64(42)},
	}
	wantRecords := make(map[string]domain.DNSRecord)
	for i, tc := range cases {
		id := seedSQLiteRecord(t, db, i+1, tc.zone, tc.name, tc.typ, tc.raw, tc.enabled, tc.proxied, tc.proxy, tc.comment)
		r := domain.DNSRecord{ID: id, Name: tc.name, Type: tc.typ, Value: tc.value, TTL: -1, Enabled: tc.enabled == 1, Proxied: tc.proxied == 1}
		if tc.comment != nil {
			text := tc.comment.(string)
			r.Comment = &text
		}
		if tc.zone == other && tc.name == "shared" {
			r.Proxy = map[string]any{"n": float64(9007199254740993)}
		}
		wantRecords[tc.zone+tc.name] = r
	}
	want := []domain.ZoneState{
		{ID: other, Name: "a other", Enabled: true, Records: []domain.DNSRecord{wantRecords[other+"a scalar"], wantRecords[other+"shared"]}},
		{ID: empty, Name: "m empty", Records: []domain.DNSRecord{}},
		{ID: sqliteZoneID, Name: "z Stored Zone.", Records: []domain.DNSRecord{wantRecords[sqliteZoneID+"shared"], wantRecords[sqliteZoneID+"z BAD Name."]}},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	store := NewSQLiteZoneReadStore(db)
	// ASCII ordering is source-query parity, not PostgreSQL collation proof.
	for i := 0; i < 2; i++ {
		got, err := store.ListZones(ctx)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("got %#v, want %#v: %v", got, want, err)
		}
		if db.Stats().InUse != 0 {
			t.Fatal("reader retained connection")
		}
	}
	// Reads must preserve stored IDs, clocks and raw JSON, not reserialize it.
	for i, tc := range cases {
		var id, zid, raw, created, updated, zcreated, zupdated string
		var proxy, storedComment *string
		err := db.QueryRow(`select r.id,r.zone_id,r.value,r.proxy_settings,r.comment,r.created_at,r.updated_at,z.created_at,z.updated_at
			from dns_records r join zones z on z.id=r.zone_id where r.id=?`, fmt.Sprintf("00000000-0000-0000-0000-%012x", i+1)).Scan(
			&id, &zid, &raw, &proxy, &storedComment, &created, &updated, &zcreated, &zupdated)
		if err != nil || id != wantRecords[tc.zone+tc.name].ID || zid != tc.zone || raw != tc.raw || created != "2" || updated != "0" || zcreated != "0" || zupdated != "2" {
			t.Fatal("storage changed", id, zid, raw, err)
		}
		if (proxy == nil) != (tc.proxy == nil) || (proxy != nil && *proxy != tc.proxy.(string)) || !reflect.DeepEqual(storedComment, wantRecords[tc.zone+tc.name].Comment) {
			t.Fatal("nullable storage changed", proxy, storedComment)
		}
	}
	for _, table := range []string{"installation_settings", "apply_jobs", "audit_events"} {
		var count int
		if err := db.QueryRow("select count(*) from " + table).Scan(&count); err != nil || count != 0 {
			t.Fatal("list created unrelated state", table, count, err)
		}
	}
}

func TestSQLiteZonesListDecodeRecovery(t *testing.T) {
	db, _, _ := sqliteSettingsFixture(t)
	db.SetMaxOpenConns(1)
	other := "00000000-0000-0000-ffff-000000000001"
	seedSQLiteZone(t, db, sqliteZoneID, "first", 1)
	seedSQLiteZone(t, db, other, "second", 0)
	seedSQLiteRecord(t, db, 1, sqliteZoneID, "a valid", "TXT", `null`, 1, 0, nil, nil)
	badID := seedSQLiteRecord(t, db, 2, other, "z bad", "A", `null`, 0, 0, nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	store := NewSQLiteZoneReadStore(db)
	for _, proxy := range []string{`[]`, `[{}]`, `42`, `"scalar"`, `true`} {
		t.Run(proxy, func(t *testing.T) {
			settingsExec(t, db, "update dns_records set proxy_settings=? where id=?", proxy, badID)
			got, err := store.ListZones(ctx)
			if err == nil || got != nil {
				t.Fatal("bad record did not fail entire list", got, err)
			}
			if db.Stats().InUse != 0 {
				t.Fatal("error retained record cursor")
			}
			settingsExec(t, db, "update dns_records set proxy_settings=NULL where id=?", badID)
			got, err = store.ListZones(ctx)
			if err != nil || len(got) != 2 || len(got[0].Records) != 1 || len(got[1].Records) != 1 {
				t.Fatal("failed read leaked into recovery", got, err)
			}
		})
	}
	cancel()
	if got, err := store.ListZones(ctx); !errors.Is(err, context.Canceled) || got != nil {
		t.Fatal(got, err)
	}
	if _, err := store.ListZones(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if got, err := store.ListZones(context.Background()); err == nil || got != nil {
		t.Fatal("closed database accepted", got, err)
	}
}

func TestSQLiteZonesListCommittedVisibility(t *testing.T) {
	db, _, path := sqliteSettingsFixture(t)
	seedSQLiteZone(t, db, sqliteZoneID, "before", 0)
	seedSQLiteRecord(t, db, 1, sqliteZoneID, "before", "TXT", `null`, 0, 0, nil, nil)
	store := NewSQLiteZoneReadStore(db)
	ctx := context.Background()
	before, err := store.ListZones(ctx)
	if err != nil || len(before) != 1 || len(before[0].Records) != 1 {
		t.Fatal(before, err)
	}
	other, err := sqlitedb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	settingsExec(t, other, "update zones set name='after',enabled=1 where id=?", sqliteZoneID)
	settingsExec(t, other, "update dns_records set name='after',value='42',enabled=1,proxied=1")
	after, err := store.ListZones(ctx)
	if err != nil || len(after) != 1 || after[0].Name != "after" || !after[0].Enabled || len(after[0].Records) != 1 {
		t.Fatal(after, err)
	}
	r := after[0].Records[0]
	if r.Name != "after" || !r.Enabled || !r.Proxied || r.Value != float64(42) {
		t.Fatal(r)
	}
	settingsExec(t, other, "delete from zones where id=?", sqliteZoneID)
	after, err = store.ListZones(ctx)
	if err != nil || after == nil || len(after) != 0 {
		t.Fatal(after, err)
	}
	var count int
	if err := db.QueryRow("select count(*) from dns_records").Scan(&count); err != nil || count != 0 {
		t.Fatal("cascade missing", count, err)
	}
}

func TestSQLiteZonesSuppliedConnection(t *testing.T) {
	db, _, _ := sqliteSettingsFixture(t)
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	sentinel := errors.New("rollback helper fixture")
	err := settingsWriter(db).Write(ctx, func(v sqlitedb.WriteValue) error {
		if _, err := v.Conn().ExecContext(v.Context(), "insert into zones values (?, 'pending', 0, '0', '2')", sqliteZoneID); err != nil {
			return err
		}
		if _, err := v.Conn().ExecContext(v.Context(), `insert into dns_records values (?,?,'r','A','"raw"',-1,0,1,NULL,NULL,'0','2')`, "00000000-0000-0000-0000-000000000001", sqliteZoneID); err != nil {
			return err
		}
		got, err := sqliteListZones(v.Context(), v.Conn())
		if err != nil {
			return err
		}
		if len(got) != 1 || got[0].Name != "pending" || got[0].Enabled || len(got[0].Records) != 1 || got[0].Records[0].Value != "raw" {
			t.Fatal(got)
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) || ctx.Err() != nil {
		t.Fatal(err, ctx.Err())
	}
	got, err := NewSQLiteZoneReadStore(db).ListZones(ctx)
	if err != nil || got == nil || len(got) != 0 {
		t.Fatal(got, err)
	}
}
