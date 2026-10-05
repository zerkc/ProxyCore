package configuration

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

func TestSQLiteSnapshotEmpty(t *testing.T) {
	db, _, _ := sqliteSettingsFixture(t)
	got, err := NewSQLiteSnapshotStore(db, settingsWriter(db)).Snapshot(context.Background())
	want := domain.ConfigurationSnapshot{
		Settings: domain.Settings{ForwardingRules: []domain.ForwardingRule{}, RetentionMaxAgeDays: 7, RetentionMaxSizeMb: 50},
		Zones:    []domain.ZoneState{}, Streams: []domain.StreamRoute{}, Certificates: []domain.CertificateStatus{},
	}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshot: %+v, %v", got, err)
	}
	var created, updated string
	var ipv4, ipv6 *string
	if err := db.QueryRow("select created_at,updated_at,ingress_ipv4,ingress_ipv6 from installation_settings").Scan(&created, &updated, &ipv4, &ipv6); err != nil {
		t.Fatal(err)
	}
	stamp, _ := sqlitedb.EncodeTimestamp(sqlitedb.FiniteTimestamp{Seconds: 123, Nanos: 456})
	if created != *stamp || created != updated || ipv4 != nil || ipv6 != nil {
		t.Fatal(created, updated, ipv4, ipv6)
	}
	for _, table := range []string{"config_revisions", "apply_jobs"} {
		var n int
		if err := db.QueryRow("select count(*) from " + table).Scan(&n); err != nil || n != 0 {
			t.Fatal(table, n, err)
		}
	}
}

func snapshotCertificateInsert(ctx context.Context, q interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, id, hostnames string, expires, renew any) error {
	_, err := q.ExecContext(ctx, `insert into certificates
  (id,hostnames,issuer,challenge,environment,status,expires_at,renew_after,certificate_pem,failure_reason,created_at,updated_at)
  values (?,?,'uploaded','none','custom','failed',?,?,'fixture PEM','reason','0','2')`, id, hostnames, expires, renew)
	return err
}

const snapshotCertID = "00000000-0000-0000-0000-000000000001"

func TestSQLiteSnapshotCollections(t *testing.T) {
	db, settings, _ := sqliteSettingsFixture(t)
	ctx := context.Background()
	pool := domain.ResolverPool{ID: "main", Endpoints: []domain.ResolverEndpoint{{Host: "192.0.2.53", Port: 53}}}
	wantSettings, err := settings.UpdateSettings(ctx, SettingsPatch{DefaultPool: &pool, ForwardingRules: []domain.ForwardingRule{{Suffix: "example.org", Pool: pool}}})
	if err != nil {
		t.Fatal(err)
	}
	seedSQLiteZone(t, db, sqliteZoneID, "INVALID Zone.", 0)
	other := "00000000-0000-0000-0000-000000000002"
	seedSQLiteZone(t, db, other, "a", 1)
	seedSQLiteRecord(t, db, 1, sqliteZoneID, "z", "A", `"invalid IP"`, 0, 1, nil, nil)
	seedSQLiteRecord(t, db, 2, sqliteZoneID, "a", "TXT", `{"n":9007199254740993}`, 1, 0, `null`, "")
	seedSQLiteRecord(t, db, 3, sqliteZoneID, "m", "AAAA", `null`, 0, 0, `{"raw":true}`, "retained")
	streams := NewSQLiteStreamStore(db, settingsWriter(db))
	for _, port := range []int{8053, 53} {
		route := streamRoute(port, "tcp")
		route.ID = fmt.Sprintf("00000000-0000-0000-0000-%012x", port)
		if _, err := streams.AddStream(ctx, route); err != nil {
			t.Fatal(err)
		}
	}
	expires, _ := sqlitedb.EncodeTimestamp(sqlitedb.FiniteTimestamp{Seconds: -1, Nanos: 123456789})
	if err := snapshotCertificateInsert(ctx, db, snapshotCertID, `["Raw.EXAMPLE."]`, expires, nil); err != nil {
		t.Fatal(err)
	}
	if err := snapshotCertificateInsert(ctx, db, other, `null`, nil, expires); err != nil {
		t.Fatal(err)
	}
	// Opposite creation order protects the snapshot's ID ordering.
	settingsExec(t, db, "update certificates set created_at='2',certificate_pem=NULL,failure_reason=NULL where id=?", other)
	got, err := NewSQLiteSnapshotStore(db, settingsWriter(db)).Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	zones, err := NewSQLiteZoneReadStore(db).ListZones(ctx)
	if err != nil {
		t.Fatal(err)
	}
	routes, err := streams.ListStreams(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Settings, wantSettings) || !reflect.DeepEqual(got.Zones, zones) || !reflect.DeepEqual(got.Streams, routes) {
		t.Fatalf("snapshot: %+v", got)
	}
	if len(got.Zones) != 2 || got.Zones[0].Enabled || got.Zones[0].Name != "INVALID Zone." || len(got.Zones[0].Records) != 3 || len(got.Zones[1].Records) != 0 {
		t.Fatal(got.Zones)
	}
	records := got.Zones[0].Records
	if records[0].Comment == nil || *records[0].Comment != "" || records[0].Proxy != nil || records[1].Value != nil || records[2].Value != "invalid IP" || records[2].Enabled || !records[2].Proxied {
		t.Fatal(records)
	}
	if !reflect.DeepEqual(records[0].Value, map[string]any{"n": float64(9007199254740993)}) || !reflect.DeepEqual(records[1].Proxy, map[string]any{"raw": true}) || records[1].Comment == nil || *records[1].Comment != "retained" || records[2].Comment != nil {
		t.Fatal("raw decoding and NULL distinctions", records)
	}
	when := time.Unix(-1, 123456789)
	wantCerts := []domain.CertificateStatus{
		{ID: snapshotCertID, Hostnames: []string{"Raw.EXAMPLE."}, Issuer: "uploaded", Challenge: "none", Environment: "custom", Status: "failed", ExpiresAt: &when, CertificatePEM: "fixture PEM", FailureReason: "reason"},
		{ID: other, Issuer: "uploaded", Challenge: "none", Environment: "custom", Status: "failed", RenewAfter: &when},
	}
	if !reflect.DeepEqual(got.Certificates, wantCerts) || got.Streams[0].ListenPort != 53 {
		t.Fatal(got.Certificates, got.Streams)
	}
}

func snapshotSettingsBytes(t *testing.T, db *sql.DB) string {
	t.Helper()
	var raw string
	err := db.QueryRow(`select json_array(id,ingress_ipv4,ingress_ipv6,default_resolver_pool,forwarding_rules,
  enrollment_hostnames,retention_max_age_days,retention_max_size_mb,current_desired_revision_id,
  current_applied_revision_id,created_at,updated_at) from installation_settings`).Scan(&raw)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestSQLiteSnapshotSettingsPreserved(t *testing.T) {
	for _, raw := range []any{nil, "null", `{}`, `[]`, `{"id":"broken","endpoints":[]}`} {
		t.Run(fmt.Sprint(raw), func(t *testing.T) {
			db, _, _ := sqliteSettingsFixture(t)
			settingsExec(t, db, `insert into installation_settings
    (id,default_resolver_pool,forwarding_rules,enrollment_hostnames,retention_max_age_days,retention_max_size_mb,current_desired_revision_id,current_applied_revision_id,created_at,updated_at)
    values ('default',?,?,'["untouched"]',0,-1,'desired','applied','0','2')`, raw, raw)
			before := snapshotSettingsBytes(t, db)
			got, err := NewSQLiteSnapshotStore(db, settingsWriter(db)).Snapshot(context.Background())
			if err != nil || got.Settings.DefaultPool != nil || got.Settings.ForwardingRules == nil || len(got.Settings.ForwardingRules) != 0 || got.Settings.RetentionMaxAgeDays != 0 || got.Settings.RetentionMaxSizeMb != -1 || got.Settings.Ingress != (domain.Ingress{}) {
				t.Fatal(got, err)
			}
			if after := snapshotSettingsBytes(t, db); after != before {
				t.Fatal(before, after)
			}
		})
	}
}

func TestSQLiteSnapshotFailureBoundaries(t *testing.T) {
	cases := []struct {
		name         string
		fail, repair func(*testing.T, *sql.DB)
	}{
		{"zone query", func(t *testing.T, db *sql.DB) { settingsExec(t, db, "alter table zones rename to hidden_zones") }, func(t *testing.T, db *sql.DB) { settingsExec(t, db, "alter table hidden_zones rename to zones") }},
		{"record decode", func(t *testing.T, db *sql.DB) {
			seedSQLiteZone(t, db, sqliteZoneID, "z", 1)
			seedSQLiteRecord(t, db, 1, sqliteZoneID, "r", "A", `null`, 1, 0, `[]`, nil)
		}, func(t *testing.T, db *sql.DB) { settingsExec(t, db, "update dns_records set proxy_settings=NULL") }},
		{"stream validation", func(t *testing.T, db *sql.DB) {
			settingsExec(t, db, `insert into stream_routes values (?,1,'tcp','not an address',53,'{"ip":"192.0.2.1","port":53,"protocol":"tcp"}','0','2')`, sqliteZoneID)
		}, func(t *testing.T, db *sql.DB) {
			settingsExec(t, db, "update stream_routes set listen_address='0.0.0.0'")
		}},
		{"stream decode", func(t *testing.T, db *sql.DB) {
			settingsExec(t, db, `insert into stream_routes values (?,1,'tcp','0.0.0.0',53,'[]','0','2')`, sqliteZoneID)
		}, func(t *testing.T, db *sql.DB) {
			settingsExec(t, db, `update stream_routes set upstream='{"ip":"192.0.2.1","port":53,"protocol":"tcp"}'`)
		}},
		{"certificate clock", func(t *testing.T, db *sql.DB) {
			if err := snapshotCertificateInsert(context.Background(), db, snapshotCertID, `[]`, "2", nil); err != nil {
				t.Fatal(err)
			}
		}, func(t *testing.T, db *sql.DB) { settingsExec(t, db, "update certificates set expires_at=NULL") }},
		{"certificate JSON", func(t *testing.T, db *sql.DB) {
			if err := snapshotCertificateInsert(context.Background(), db, snapshotCertID, `{}`, nil, nil); err != nil {
				t.Fatal(err)
			}
		}, func(t *testing.T, db *sql.DB) { settingsExec(t, db, "update certificates set hostnames='[]'") }},
		{"certificate query", func(t *testing.T, db *sql.DB) {
			settingsExec(t, db, "alter table certificates rename to hidden_certificates")
		}, func(t *testing.T, db *sql.DB) {
			settingsExec(t, db, "alter table hidden_certificates rename to certificates")
		}},
	}
	for _, tc := range cases {
		for _, internal := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/internal=%v", tc.name, internal), func(t *testing.T) {
				db, _, _ := sqliteSettingsFixture(t)
				db.SetMaxOpenConns(1)
				tc.fail(t, db)
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				writer := settingsWriter(db)
				store := NewSQLiteSnapshotStore(db, writer)
				var got domain.ConfigurationSnapshot
				var err error
				if internal {
					err = writer.Write(ctx, func(v sqlitedb.WriteValue) error {
						if _, err := v.Conn().ExecContext(v.Context(), `insert into stream_routes values (?,0,'tcp','0.0.0.0',1053,'{"ip":"192.0.2.1","port":53,"protocol":"tcp"}','0','2')`, snapshotCertID); err != nil {
							return err
						}
						var e error
						got, e = store.snapshot(v)
						return e
					})
				} else {
					got, err = store.Snapshot(ctx)
				}
				if err == nil || !reflect.DeepEqual(got, domain.ConfigurationSnapshot{}) || ctx.Err() != nil {
					t.Fatal(got, err, ctx.Err())
				}
				var n int
				if err := db.QueryRow("select count(*) from installation_settings").Scan(&n); err != nil {
					t.Fatal(err)
				}
				want := 1
				if internal {
					want = 0
				}
				if n != want {
					t.Fatal("settings initialization", n, want)
				}
				if internal {
					if err := db.QueryRow("select count(*) from stream_routes where id=?", snapshotCertID).Scan(&n); err != nil || n != 0 {
						t.Fatal("pending mutation rollback", n, err)
					}
				}
				tc.repair(t, db)
				if _, err := store.Snapshot(ctx); err != nil {
					t.Fatal("cursor recovery", err)
				}
			})
		}
	}
}

func TestSQLiteSnapshotCallerTransaction(t *testing.T) {
	db, _, _ := sqliteSettingsFixture(t)
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	calls := 0
	writer := sqlitedb.NewWriter(db, func() sqlitedb.FiniteTimestamp { calls++; return sqlitedb.FiniteTimestamp{Seconds: 123, Nanos: 456} })
	store := NewSQLiteSnapshotStore(db, writer)
	sentinel := errors.New("caller abort")
	err := writer.Write(ctx, func(v sqlitedb.WriteValue) error {
		statements := []string{
			`insert into zones values ('` + sqliteZoneID + `','uncommitted',0,'0','2')`,
			`insert into dns_records values ('` + snapshotCertID + `','` + sqliteZoneID + `','r','A','"raw"',1,0,0,NULL,NULL,'0','2')`,
			`insert into stream_routes values ('` + snapshotCertID + `',1,'tcp','0.0.0.0',53,'{"ip":"192.0.2.1","port":53,"protocol":"tcp"}','0','2')`,
		}
		for _, query := range statements {
			if _, err := v.Conn().ExecContext(v.Context(), query); err != nil {
				return err
			}
		}
		if err := snapshotCertificateInsert(v.Context(), v.Conn(), snapshotCertID, `[]`, nil, nil); err != nil {
			return err
		}
		got, err := store.snapshot(v)
		if err != nil {
			return err
		}
		if len(got.Zones) != 1 || len(got.Zones[0].Records) != 1 || len(got.Streams) != 1 || len(got.Certificates) != 1 || got.Settings.Ingress != (domain.Ingress{}) {
			t.Fatal(got)
		}
		var created, updated string
		if err := v.Conn().QueryRowContext(v.Context(), "select created_at,updated_at from installation_settings").Scan(&created, &updated); err != nil {
			return err
		}
		if created != v.Timestamp() || updated != created {
			t.Fatal(created, updated, v.Timestamp())
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) || calls != 1 || ctx.Err() != nil {
		t.Fatal(err, calls, ctx.Err())
	}
	for _, table := range []string{"installation_settings", "zones", "dns_records", "stream_routes", "certificates"} {
		var n int
		if err := db.QueryRow("select count(*) from " + table).Scan(&n); err != nil || n != 0 {
			t.Fatal(table, n, err)
		}
	}
	// The same helper also allows a successful caller commit.
	if err := writer.Write(ctx, func(v sqlitedb.WriteValue) error { _, err := store.snapshot(v); return err }); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatal(calls)
	}
}

func TestSQLiteSnapshotCancellationAndClosedDB(t *testing.T) {
	db, _, _ := sqliteSettingsFixture(t)
	store := NewSQLiteSnapshotStore(db, settingsWriter(db))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err := store.Snapshot(ctx)
	if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(got, domain.ConfigurationSnapshot{}) {
		t.Fatal(got, err)
	}
	var n int
	if err := db.QueryRow("select count(*) from installation_settings").Scan(&n); err != nil || n != 0 {
		t.Fatal(n, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if got, err := store.Snapshot(context.Background()); err == nil || !reflect.DeepEqual(got, domain.ConfigurationSnapshot{}) {
		t.Fatal(got, err)
	}
}

func TestSQLiteSnapshotClockBridge(t *testing.T) {
	cases := []struct {
		name    string
		seconds int64
		nanos   int32
		reject  bool
	}{
		{"epoch", 0, 0, false}, {"negative fraction", -1, 999999999, false}, {"microseconds", 123, 123456000, false},
		{"full nanos", 123, 123456789, false}, {"lower inclusive", -210866803200, 0, false},
		{"lower adjacent", -210866803201, 999999999, true}, {"upper adjacent", 9224318015999, 0, false},
		{"final nanosecond", 9224318015999, 999999999, false}, {"upper exclusive", 9224318016000, 0, true},
		{"minimum int64", math.MinInt64, 0, true}, {"maximum int64", math.MaxInt64, 999999999, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key, err := sqlitedb.EncodeTimestamp(sqlitedb.FiniteTimestamp{Seconds: tc.seconds, Nanos: tc.nanos})
			if err != nil {
				t.Fatal(err)
			}
			got, err := sqliteCertificateTime(key)
			if tc.reject {
				if err == nil || got != nil {
					t.Fatal(got, err)
				}
				return
			}
			if err != nil || got == nil || got.Unix() != tc.seconds || got.Nanosecond() != int(tc.nanos) || got.Location() != time.Local {
				t.Fatal(got, err)
			}
		})
	}
	for _, key := range []string{"0", "2", "", "1", " 2", "118446744073709551616.000000000", "109223372036854775808.1000000000", "109223372036854775808.00000000x"} {
		t.Run("invalid "+key, func(t *testing.T) {
			if got, err := sqliteCertificateTime(&key); err == nil || got != nil {
				t.Fatal(got, err)
			}
		})
	}
	if got, err := sqliteCertificateTime(nil); err != nil || got != nil {
		t.Fatal(got, err)
	}
}

func TestSQLiteSnapshotClockLocal(t *testing.T) {
	// No parallel tests: the default pgx scan location is the process time.Local.
	previous := time.Local
	time.Local = time.FixedZone("fixture", -3*60*60)
	defer func() { time.Local = previous }()
	key, _ := sqlitedb.EncodeTimestamp(sqlitedb.FiniteTimestamp{Seconds: 0, Nanos: 123456789})
	got, err := sqliteCertificateTime(key)
	if err != nil || got == nil {
		t.Fatal(got, err)
	}
	_, offset := got.Zone()
	encoded, err := json.Marshal(got)
	if err != nil || got.Unix() != 0 || got.Nanosecond() != 123456789 || offset != -10800 || string(encoded) != `"1969-12-31T21:00:00.123456789-03:00"` {
		t.Fatal(got, string(encoded), err)
	}
}

func TestSQLiteSnapshotFiniteYearSerialization(t *testing.T) {
	for _, year := range []int{-1, 0, 9999, 10000, 294276} {
		t.Run(fmt.Sprint(year), func(t *testing.T) {
			instant := time.Date(year, 7, 1, 0, 0, 0, 999999999, time.UTC)
			key, _ := sqlitedb.EncodeTimestamp(sqlitedb.FiniteTimestamp{Seconds: instant.Unix(), Nanos: 999999999})
			got, err := sqliteCertificateTime(key)
			if err != nil || !got.Equal(instant) {
				t.Fatal(got, err)
			}
			snap := domain.ConfigurationSnapshot{Certificates: []domain.CertificateStatus{{ExpiresAt: got}}}
			_, err = json.Marshal(snap)
			if year < 0 || year > 9999 {
				if err == nil || domain.StableStringify(snap) != "null" || domain.ChecksumSnapshot(snap) != domain.ChecksumSnapshot(nil) {
					t.Fatal(err, domain.StableStringify(snap))
				}
			} else if err != nil || domain.StableStringify(snap) == "null" {
				t.Fatal(err)
			}
		})
	}
}

// A synthetic scanner reaches malformed clocks without weakening STRICT checks.
type snapshotClockScanner struct {
	expires, renew *string
	err            error
}

func (s snapshotClockScanner) Scan(dest ...any) error {
	if s.err != nil {
		return s.err
	}
	*dest[0].(*string) = snapshotCertID
	*dest[1].(*[]byte) = []byte(`[]`)
	*dest[6].(**string) = s.expires
	*dest[7].(**string) = s.renew
	return nil
}

func TestSQLiteSnapshotCertificateScanner(t *testing.T) {
	valid, _ := sqlitedb.EncodeTimestamp(sqlitedb.FiniteTimestamp{Seconds: 123, Nanos: 999999999})
	malformed := "malformed"
	infinity := "0"
	for _, tc := range []struct {
		name           string
		expires, renew *string
		reject         bool
	}{
		{"both NULL", nil, nil, false}, {"expires only", valid, nil, false}, {"renew only", nil, valid, false},
		{"both", valid, valid, false}, {"expires malformed", &malformed, nil, true}, {"renew malformed", valid, &malformed, true},
		{"expires infinity", &infinity, nil, true}, {"renew infinity", nil, &infinity, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := scanCertificate(sqliteCertificateScanner{snapshotClockScanner{expires: tc.expires, renew: tc.renew}})
			if tc.reject {
				if err == nil || !reflect.DeepEqual(got, domain.CertificateStatus{}) {
					t.Fatal(got, err)
				}
				return
			}
			if err != nil || (got.ExpiresAt == nil) != (tc.expires == nil) || (got.RenewAfter == nil) != (tc.renew == nil) || got.Hostnames == nil {
				t.Fatal(got, err)
			}
		})
	}
	sentinel := errors.New("scan failure")
	if _, err := scanCertificate(sqliteCertificateScanner{snapshotClockScanner{err: sentinel}}); !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
}

// Real SQLite rows allow scan and mid-iteration failures without weakening the
// product schema. Queries not targeted by the fixture still use the real tables.
type snapshotFaultQuerier struct {
	db                 *sql.DB
	match, replacement string
	calls              []string
	fail               bool
}

func (q *snapshotFaultQuerier) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	q.calls = append(q.calls, query)
	if strings.Contains(query, q.match) {
		if q.fail {
			return nil, errors.New("fixture query failure")
		}
		query = q.replacement
	}
	return q.db.QueryContext(ctx, query, args...)
}

func TestSQLiteSnapshotRowFailureCleanup(t *testing.T) {
	cases := []struct{ name, match, query string }{
		{"zone scan", "from zones", `select NULL,'z',1`},
		{"zone iteration", "from zones", `select 'z','z',1 union all select json_extract('broken','$'),'z',1`},
		{"record scan", "from dns_records", `select NULL,'z','r','A','null',1,1,0,NULL,NULL`},
		{"record iteration", "from dns_records", `select 'r','z','r','A','null',1,1,0,NULL,NULL union all select json_extract('broken','$'),'z','r','A','null',1,1,0,NULL,NULL`},
		{"stream boolean", "from stream_routes", `select 'r',2,'tcp','0.0.0.0',53,'{"ip":"192.0.2.1","port":53,"protocol":"tcp"}'`},
		{"stream iteration", "from stream_routes", `select 'r',1,'tcp','0.0.0.0',53,'{"ip":"192.0.2.1","port":53,"protocol":"tcp"}' union all select json_extract('broken','$'),1,'tcp','0.0.0.0',53,'{}'`},
		{"certificate scan", "from certificates", `select NULL,'[]','uploaded','none','custom','pending',NULL,NULL,NULL,NULL,NULL`},
		{"certificate malformed clock", "from certificates", `select 'c','[]','uploaded','none','custom','pending','malformed',NULL,NULL,NULL,NULL`},
		{"certificate iteration", "from certificates", `select 'c','[]','uploaded','none','custom','pending',NULL,NULL,NULL,NULL,NULL union all select json_extract('broken','$'),'[]','uploaded','none','custom','pending',NULL,NULL,NULL,NULL,NULL`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, _, _ := sqliteSettingsFixture(t)
			db.SetMaxOpenConns(1)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if strings.Contains(tc.name, "iteration") {
				probe, err := db.QueryContext(ctx, tc.query)
				if err != nil {
					t.Fatal("expected iteration, not query failure", err)
				}
				n := 0
				for probe.Next() {
					n++
				}
				iterationErr := probe.Err()
				probe.Close()
				if n != 1 || iterationErr == nil {
					t.Fatal("mid-iteration fixture", n, iterationErr)
				}
			}
			q := &snapshotFaultQuerier{db: db, match: tc.match, replacement: tc.query}
			got, err := sqliteAssembleSnapshot(ctx, q, settingsRow{})
			if err == nil || !reflect.DeepEqual(got, domain.ConfigurationSnapshot{}) || ctx.Err() != nil {
				t.Fatal(got, err, ctx.Err())
			}
			if _, err := NewSQLiteSnapshotStore(db, settingsWriter(db)).Snapshot(ctx); err != nil {
				t.Fatal("cursor not released", err)
			}
		})
	}
}

func TestSQLiteSnapshotAssemblyOrderAndCapturedSettings(t *testing.T) {
	db, _, _ := sqliteSettingsFixture(t)
	ctx := context.Background()
	settingsExec(t, db, "insert into installation_settings(id,created_at,updated_at,ingress_ipv4) values ('default','0','2','new')")
	old := "captured"
	row := settingsRow{ingressIPv4: &old, forwardingRules: []byte("broken"), defaultResolverPool: []byte("broken"), retentionMaxAgeDays: 7, retentionMaxSizeMb: 50}
	q := &snapshotFaultQuerier{db: db, match: "not a query"}
	got, err := sqliteAssembleSnapshot(ctx, q, row)
	if err != nil || got.Settings.Ingress.IPv4 != "captured" || got.Settings.DefaultPool != nil || len(got.Settings.ForwardingRules) != 0 {
		t.Fatal(got, err)
	}
	expected := []string{"from zones", "from dns_records", "from stream_routes", "from certificates"}
	if len(q.calls) != len(expected) {
		t.Fatal(q.calls)
	}
	for i, query := range q.calls {
		if !strings.Contains(query, expected[i]) {
			t.Fatal(q.calls)
		}
	}
	// All components are unconditional, even when the preceding list is empty.
	for i, match := range expected {
		q := &snapshotFaultQuerier{db: db, match: match, fail: true}
		got, err := sqliteAssembleSnapshot(ctx, q, row)
		if err == nil || !reflect.DeepEqual(got, domain.ConfigurationSnapshot{}) || len(q.calls) != i+1 {
			t.Fatal(got, err, q.calls)
		}
	}
}

func TestSQLiteSnapshotNullableCertificateHostnames(t *testing.T) {
	db, _, _ := sqliteSettingsFixture(t)
	for _, raw := range []string{"NULL", `'null'`, `'[]'`, `'["Raw."]'`, `'{}'`, `'42'`} {
		t.Run(raw, func(t *testing.T) {
			q := &snapshotFaultQuerier{db: db, match: "from certificates", replacement: `select 'c',` + raw + `,'uploaded','none','custom','failed',NULL,NULL,'reference-only','fixture PEM','reason'`}
			got, err := sqliteAssembleSnapshot(context.Background(), q, settingsRow{})
			if raw == `'{}'` || raw == `'42'` {
				if err == nil || !reflect.DeepEqual(got, domain.ConfigurationSnapshot{}) {
					t.Fatal(got, err)
				}
				return
			}
			if err != nil || len(got.Certificates) != 1 {
				t.Fatal(got, err)
			}
			cert := got.Certificates[0]
			if cert.SecretID != "reference-only" || cert.CertificatePEM != "fixture PEM" || cert.FailureReason != "reason" || cert.ExpiresAt != nil || cert.RenewAfter != nil {
				t.Fatal(cert)
			}
			if (cert.Hostnames == nil) != (raw == "NULL" || raw == `'null'`) {
				t.Fatal(cert.Hostnames)
			}
			for _, query := range q.calls {
				if strings.Contains(query, "from secrets") {
					t.Fatal("unexpected secret lookup", query)
				}
			}
		})
	}
}
