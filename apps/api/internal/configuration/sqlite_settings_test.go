package configuration

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

func sqliteSettingsFixture(t *testing.T) (*sql.DB, *SQLiteSettingsStore, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "settings.db")
	db, err := sqlitedb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := sqlitedb.Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	return db, NewSQLiteSettingsStore(db, settingsWriter(db), domain.Ingress{IPv4: "192.0.2.1"}), path
}

func settingsWriter(db *sql.DB) *sqlitedb.Writer {
	return sqlitedb.NewWriter(db, func() sqlitedb.FiniteTimestamp { return sqlitedb.FiniteTimestamp{Seconds: 123, Nanos: 456} })
}

func settingsExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

func TestSQLiteSettingsCycle(t *testing.T) {
	db, store, path := sqliteSettingsFixture(t)
	ctx := context.Background()
	first, err := store.GetSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if first.Ingress.IPv4 != "192.0.2.1" || first.RetentionMaxAgeDays != 7 || first.RetentionMaxSizeMb != 50 || first.DefaultPool != nil || first.ForwardingRules == nil || len(first.ForwardingRules) != 0 {
		t.Fatalf("defaults: %+v", first)
	}
	var created, updated, kind string
	if err := db.QueryRow("select created_at,updated_at,typeof(forwarding_rules) from installation_settings where id='default'").Scan(&created, &updated, &kind); err != nil {
		t.Fatal(err)
	}
	stamp, _ := sqlitedb.EncodeTimestamp(sqlitedb.FiniteTimestamp{Seconds: 123, Nanos: 456})
	if created != *stamp || updated != created || kind != "text" {
		t.Fatal(created, updated, kind)
	}
	settingsExec(t, db, "update installation_settings set updated_at='0',enrollment_hostnames=?,current_desired_revision_id='desired',current_applied_revision_id='applied'", `["keep"]`)
	if _, err := store.GetSettings(ctx); err != nil {
		t.Fatal(err)
	}
	var untouched string
	if err := db.QueryRow("select updated_at from installation_settings").Scan(&untouched); err != nil || untouched != "0" {
		t.Fatal(untouched, err)
	}
	pool := domain.ResolverPool{ID: "main", Endpoints: []domain.ResolverEndpoint{{Host: "192.0.2.53", Port: 53}}}
	next, err := store.UpdateSettings(ctx, SettingsPatch{DefaultPool: &pool, ForwardingRules: []domain.ForwardingRule{{Suffix: "example.org", Pool: pool}}})
	if err != nil || next.DefaultPool == nil || len(next.ForwardingRules) != 1 {
		t.Fatal(next, err)
	}
	next, err = store.UpdateSettings(ctx, SettingsPatch{Ingress: &domain.Ingress{}, ForwardingRules: []domain.ForwardingRule{}})
	if err != nil || next.Ingress != first.Ingress || next.DefaultPool == nil || next.ForwardingRules == nil || len(next.ForwardingRules) != 0 {
		t.Fatal(next, err)
	}
	settingsExec(t, db, "update installation_settings set default_resolver_pool=?,forwarding_rules=NULL", ` {"unknown":9007199254740993} `)
	if _, err := store.UpdateSettings(ctx, SettingsPatch{}); err != nil {
		t.Fatal(err)
	}
	var raw, enrollment, desired, applied string
	var rules sql.NullString
	if err := db.QueryRow("select default_resolver_pool,forwarding_rules,enrollment_hostnames,current_desired_revision_id,current_applied_revision_id,created_at from installation_settings").Scan(&raw, &rules, &enrollment, &desired, &applied, &untouched); err != nil {
		t.Fatal(err)
	}
	if raw != ` {"unknown":9007199254740993} ` || rules.Valid || enrollment != `["keep"]` || desired != "desired" || applied != "applied" || untouched != created {
		t.Fatal(raw, rules, enrollment, desired, applied, untouched)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := sqlitedb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got, err := NewSQLiteSettingsStore(reopened, settingsWriter(reopened), domain.Ingress{IPv4: "other"}).GetSettings(ctx)
	if err != nil || got.Ingress != first.Ingress || got.DefaultPool != nil || len(got.ForwardingRules) != 0 {
		t.Fatal(got, err)
	}
}

func TestSQLiteSettingsIngress(t *testing.T) {
	for _, stored := range []string{"NULL,NULL", "'present',NULL", "NULL,'present'", "'',NULL"} {
		t.Run(stored, func(t *testing.T) {
			db, store, _ := sqliteSettingsFixture(t)
			ctx := context.Background()
			ok, err := store.InitializeIngress(ctx, domain.Ingress{})
			var count int
			if err != nil || ok {
				t.Fatal(ok, err)
			}
			if err := db.QueryRow("select count(*) from installation_settings").Scan(&count); err != nil || count != 0 {
				t.Fatal(count, err)
			}
			if _, err := store.GetSettings(ctx); err != nil {
				t.Fatal(err)
			}
			settingsExec(t, db, "update installation_settings set (ingress_ipv4,ingress_ipv6)=("+stored+"),updated_at='0'")
			ok, err = store.InitializeIngress(ctx, domain.Ingress{IPv6: "2001:db8::1"})
			if err != nil || ok != (stored == "NULL,NULL") {
				t.Fatal(ok, err)
			}
			if !ok {
				var stamp string
				if err := db.QueryRow("select updated_at from installation_settings").Scan(&stamp); err != nil || stamp != "0" {
					t.Fatal(stamp, err)
				}
			}
		})
	}
}

func TestSQLiteSettingsRollback(t *testing.T) {
	for _, patch := range []SettingsPatch{
		{RetentionMaxAgeDays: intPointer(0)}, {RetentionMaxSizeMb: intPointer(-1)},
		{DefaultPool: &domain.ResolverPool{}}, {ForwardingRules: []domain.ForwardingRule{{}}},
		{Ingress: &domain.Ingress{IPv4: "bad\x00value"}},
	} {
		db, store, _ := sqliteSettingsFixture(t)
		if _, err := store.UpdateSettings(context.Background(), patch); err == nil {
			t.Fatal("invalid patch accepted")
		}
		var count int
		if err := db.QueryRow("select count(*) from installation_settings").Scan(&count); err != nil || count != 0 {
			t.Fatal("ensure/init escaped rollback", count, err)
		}
		before, err := store.GetSettings(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		settingsExec(t, db, "update installation_settings set updated_at='0'")
		_, patchErr := store.UpdateSettings(context.Background(), patch)
		after, err := store.GetSettings(context.Background())
		var stamp string
		if scanErr := db.QueryRow("select updated_at from installation_settings").Scan(&stamp); scanErr != nil {
			t.Fatal(scanErr)
		}
		if patchErr == nil || err != nil || !reflect.DeepEqual(before, after) || stamp != "0" {
			t.Fatal(before, after, patchErr, err, stamp)
		}
		if patch.RetentionMaxAgeDays != nil || patch.RetentionMaxSizeMb != nil {
			if patchErr.Error() != "Retention limits must be positive" {
				t.Fatal(patchErr)
			}
		}
	}
}

func intPointer(v int) *int { return &v }

func TestSQLiteSettingsJSONFallback(t *testing.T) {
	db, store, _ := sqliteSettingsFixture(t)
	if _, err := store.GetSettings(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`{}`, `42`, `[{"suffix":"bad","pool":{}}]`, `null`} {
		settingsExec(t, db, "update installation_settings set default_resolver_pool=?,forwarding_rules=?", raw, raw)
		got, err := store.GetSettings(context.Background())
		if err != nil || got.DefaultPool != nil || got.ForwardingRules == nil || len(got.ForwardingRules) != 0 {
			t.Fatal(raw, got, err)
		}
	}
	// Authority rejects malformed syntax; exercise the shared read mapping directly.
	got, err := toSettings(settingsRow{defaultResolverPool: []byte("{"), forwardingRules: []byte("{")})
	if err != nil || got.DefaultPool != nil || len(got.ForwardingRules) != 0 {
		t.Fatal(got, err)
	}
	if _, err := db.Exec("update installation_settings set forwarding_rules=?", "{"); err == nil {
		t.Fatal("malformed JSON accepted")
	}
}

func TestSQLiteSettingsConcurrent(t *testing.T) {
	db, store, path := sqliteSettingsFixture(t)
	other, err := sqlitedb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	stores := []*SQLiteSettingsStore{store, NewSQLiteSettingsStore(other, settingsWriter(other), domain.Ingress{})}
	var wg sync.WaitGroup
	winners := make(chan bool, 2)
	errors := make(chan error, 2)
	for i, s := range stores {
		wg.Add(1)
		go func(i int, s *SQLiteSettingsStore) {
			defer wg.Done()
			ok, err := s.InitializeIngress(context.Background(), domain.Ingress{IPv4: []string{"one", "two"}[i]})
			winners <- ok
			errors <- err
		}(i, s)
	}
	wg.Wait()
	if <-errors != nil || <-errors != nil {
		t.Fatal("initialization failed")
	}
	if a, b := <-winners, <-winners; a == b {
		t.Fatal("expected exactly one winner", a, b)
	}
	before, err := store.GetSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for i, s := range stores {
		wg.Add(1)
		go func(i int, s *SQLiteSettingsStore) {
			defer wg.Done()
			patch := SettingsPatch{RetentionMaxAgeDays: intPointer(11)}
			if i == 1 {
				patch = SettingsPatch{RetentionMaxSizeMb: intPointer(99)}
			}
			_, err := s.UpdateSettings(context.Background(), patch)
			errors <- err
		}(i, s)
	}
	wg.Wait()
	if <-errors != nil || <-errors != nil {
		t.Fatal("patch failed")
	}
	got, err := store.GetSettings(context.Background())
	if err != nil || got.Ingress != before.Ingress || got.RetentionMaxAgeDays != 11 || got.RetentionMaxSizeMb != 99 {
		t.Fatal(got, err)
	}
	_ = db
}
