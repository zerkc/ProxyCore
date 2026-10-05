package configuration

import (
	"context"
	"database/sql"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

func TestSQLiteEnrollmentCycle(t *testing.T) {
	db, store, path := sqliteSettingsFixture(t)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		got, err := store.GetEnrollmentHostnames(ctx)
		if err != nil || got.Configured || got.Hostnames == nil || len(got.Hostnames) != 0 {
			t.Fatal(got, err)
		}
	}
	stamp, _ := sqlitedb.EncodeTimestamp(sqlitedb.FiniteTimestamp{Seconds: 123, Nanos: 456})
	var created, updated string
	var ipv4, ipv6, raw sql.NullString
	if err := db.QueryRow("select created_at,updated_at,ingress_ipv4,ingress_ipv6,enrollment_hostnames from installation_settings").Scan(&created, &updated, &ipv4, &ipv6, &raw); err != nil {
		t.Fatal(err)
	}
	if created != *stamp || updated != created || ipv4.Valid || ipv6.Valid || raw.Valid {
		t.Fatal(created, updated, ipv4, ipv6, raw)
	}
	settingsExec(t, db, "update installation_settings set updated_at='0'")
	if _, err := store.GetEnrollmentHostnames(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("select updated_at from installation_settings").Scan(&updated); err != nil || updated != "0" {
		t.Fatal(updated, err)
	}
	input := []string{"Z.Example.", "a.example", "A.EXAMPLE.", "2001:0db8:0:0::1", "2001:db8::1", "192.0.2.1", "::ffff:192.0.2.1"}
	want := []string{"192.0.2.1", "2001:db8::1", "a.example", "z.example"}
	got, err := store.UpdateEnrollmentHostnames(ctx, input)
	if err != nil || !got.Configured || !reflect.DeepEqual(got.Hostnames, want) {
		t.Fatal(got, err)
	}
	var kind string
	if err := db.QueryRow("select enrollment_hostnames,typeof(enrollment_hostnames),created_at,updated_at,ingress_ipv4,ingress_ipv6 from installation_settings").Scan(&raw, &kind, &created, &updated, &ipv4, &ipv6); err != nil {
		t.Fatal(err)
	}
	if raw.String != `["192.0.2.1","2001:db8::1","a.example","z.example"]` || kind != "text" || created != *stamp || updated != *stamp || ipv4.Valid || ipv6.Valid {
		t.Fatal(raw, kind, created, updated, ipv4, ipv6)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := sqlitedb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	store = NewSQLiteSettingsStore(reopened, settingsWriter(reopened), domain.Ingress{IPv4: "192.0.2.99"})
	got, err = store.GetEnrollmentHostnames(ctx)
	if err != nil || !got.Configured || !reflect.DeepEqual(got.Hostnames, want) {
		t.Fatal(got, err)
	}
	for _, empty := range [][]string{nil, {}} {
		got, err = store.UpdateEnrollmentHostnames(ctx, empty)
		if err != nil || got.Configured || got.Hostnames == nil || len(got.Hostnames) != 0 {
			t.Fatal(got, err)
		}
		if err := reopened.QueryRow("select enrollment_hostnames from installation_settings").Scan(&raw); err != nil || raw.Valid {
			t.Fatal(raw, err)
		}
	}
	clearedDB, err := sqlitedb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer clearedDB.Close()
	cleared := NewSQLiteSettingsStore(clearedDB, settingsWriter(clearedDB), domain.Ingress{IPv4: "other"})
	got, err = cleared.GetEnrollmentHostnames(ctx)
	if err != nil || got.Configured || got.Hostnames == nil {
		t.Fatal(got, err)
	}
	if err := clearedDB.QueryRow("select ingress_ipv4,ingress_ipv6 from installation_settings").Scan(&ipv4, &ipv6); err != nil || ipv4.Valid || ipv6.Valid {
		t.Fatal(ipv4, ipv6, err)
	}
}

func TestSQLiteEnrollmentReadValidation(t *testing.T) {
	db, store, _ := sqliteSettingsFixture(t)
	ctx := context.Background()
	if _, err := store.GetEnrollmentHostnames(ctx); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`null`, `[]`, ` null `} {
		settingsExec(t, db, "update installation_settings set enrollment_hostnames=?", raw)
		got, err := store.GetEnrollmentHostnames(ctx)
		if err != nil || got.Configured || got.Hostnames == nil || len(got.Hostnames) != 0 {
			t.Fatal(raw, got, err)
		}
	}
	settingsExec(t, db, "update installation_settings set enrollment_hostnames=?", `["B.Example.","b.example","2001:0db8::1"]`)
	got, err := store.GetEnrollmentHostnames(ctx)
	if err != nil || !got.Configured || !reflect.DeepEqual(got.Hostnames, []string{"2001:db8::1", "b.example"}) {
		t.Fatal(got, err)
	}
	for _, raw := range []string{`{}`, `42`, `"example.org"`, `[null]`, `[1]`, `["*.example.org"]`, `["999.1.1.1"]`, `["bad\u0000name"]`} {
		t.Run(raw, func(t *testing.T) {
			settingsExec(t, db, "update installation_settings set enrollment_hostnames=?,updated_at='0'", raw)
			_, err := store.GetEnrollmentHostnames(ctx)
			if err == nil || !strings.HasPrefix(err.Error(), "read enrollment hostnames: ") {
				t.Fatal(err)
			}
			var unchanged, updated string
			if err := db.QueryRow("select enrollment_hostnames,updated_at from installation_settings").Scan(&unchanged, &updated); err != nil || unchanged != raw || updated != "0" {
				t.Fatal(unchanged, updated, err)
			}
		})
	}
}

func TestSQLiteEnrollmentInvalidUpdate(t *testing.T) {
	tooMany := make([]string, MaxEnrollmentHostnames+1)
	for i := range tooMany {
		tooMany[i] = "example.org"
	}
	for _, input := range [][]string{{"*.example.org"}, {"bad\x00name"}, {strings.Repeat("a", MaxEnrollmentHostnameLength+1)}, tooMany} {
		t.Run(strings.Join(input, ","), func(t *testing.T) {
			db, store, _ := sqliteSettingsFixture(t)
			ctx := context.Background()
			_, expected := NormalizeEnrollmentHostnames(input)
			_, err := store.UpdateEnrollmentHostnames(ctx, input)
			if expected == nil || err == nil || err.Error() != expected.Error() {
				t.Fatal(expected, err)
			}
			var count int
			if err := db.QueryRow("select count(*) from installation_settings").Scan(&count); err != nil || count != 0 {
				t.Fatal(count, err)
			}
			before, err := store.UpdateEnrollmentHostnames(ctx, []string{"keep.example"})
			if err != nil {
				t.Fatal(err)
			}
			settingsExec(t, db, "update installation_settings set updated_at='0'")
			if _, err := store.UpdateEnrollmentHostnames(ctx, input); err == nil {
				t.Fatal("invalid update accepted")
			}
			after, err := store.GetEnrollmentHostnames(ctx)
			var updated string
			if scanErr := db.QueryRow("select updated_at from installation_settings").Scan(&updated); scanErr != nil {
				t.Fatal(scanErr)
			}
			if err != nil || !reflect.DeepEqual(before, after) || updated != "0" {
				t.Fatal(before, after, updated, err)
			}
		})
	}
}

func TestSQLiteEnrollmentPreservesSettings(t *testing.T) {
	db, store, _ := sqliteSettingsFixture(t)
	ctx := context.Background()
	if _, err := store.GetEnrollmentHostnames(ctx); err != nil {
		t.Fatal(err)
	}
	pool := ` {"unknown":9007199254740993} `
	settingsExec(t, db, `update installation_settings set ingress_ipv4='present',ingress_ipv6=NULL,
		default_resolver_pool=?,forwarding_rules=NULL,retention_max_age_days=13,retention_max_size_mb=91,
		current_desired_revision_id='desired',current_applied_revision_id='applied',created_at='0'`, pool)
	for _, input := range [][]string{{"example.org"}, nil} {
		if _, err := store.UpdateEnrollmentHostnames(ctx, input); err != nil {
			t.Fatal(err)
		}
		var ipv4, raw, desired, applied, created string
		var ipv6, rules sql.NullString
		var age, size int
		if err := db.QueryRow(`select ingress_ipv4,ingress_ipv6,default_resolver_pool,forwarding_rules,
			retention_max_age_days,retention_max_size_mb,current_desired_revision_id,current_applied_revision_id,created_at
			from installation_settings`).Scan(&ipv4, &ipv6, &raw, &rules, &age, &size, &desired, &applied, &created); err != nil {
			t.Fatal(err)
		}
		if ipv4 != "present" || ipv6.Valid || raw != pool || rules.Valid || age != 13 || size != 91 || desired != "desired" || applied != "applied" || created != "0" {
			t.Fatal(ipv4, ipv6, raw, rules, age, size, desired, applied, created)
		}
	}
}

func TestSQLiteEnrollmentSQLRollback(t *testing.T) {
	db, store, _ := sqliteSettingsFixture(t)
	ctx := context.Background()
	settingsExec(t, db, `create trigger reject_enrollment before update of enrollment_hostnames
		on installation_settings begin select raise(abort,'rejected enrollment'); end`)
	if _, err := store.UpdateEnrollmentHostnames(ctx, []string{"example.org"}); err == nil {
		t.Fatal("SQL failure accepted")
	}
	var count int
	if err := db.QueryRow("select count(*) from installation_settings").Scan(&count); err != nil || count != 0 {
		t.Fatal("ensure escaped rollback", count, err)
	}
	if _, err := store.GetEnrollmentHostnames(ctx); err != nil {
		t.Fatal(err)
	}
	settingsExec(t, db, "update installation_settings set updated_at='0'")
	if _, err := store.UpdateEnrollmentHostnames(ctx, []string{"example.org"}); err == nil {
		t.Fatal("SQL failure accepted")
	}
	var raw sql.NullString
	var updated string
	if err := db.QueryRow("select enrollment_hostnames,updated_at from installation_settings").Scan(&raw, &updated); err != nil || raw.Valid || updated != "0" {
		t.Fatal(raw, updated, err)
	}
}

func TestSQLiteEnrollmentConcurrent(t *testing.T) {
	_, store, path := sqliteSettingsFixture(t)
	other, err := sqlitedb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	second := NewSQLiteSettingsStore(other, settingsWriter(other), domain.Ingress{})
	ctx := context.Background()
	var wg sync.WaitGroup
	errors := make(chan error, 3)
	for _, operation := range []func() error{
		func() error {
			_, err := store.UpdateSettings(ctx, SettingsPatch{RetentionMaxAgeDays: intPointer(11)})
			return err
		},
		func() error {
			_, err := second.UpdateSettings(ctx, SettingsPatch{RetentionMaxSizeMb: intPointer(99)})
			return err
		},
		func() error { _, err := second.UpdateEnrollmentHostnames(ctx, []string{"EXAMPLE.org."}); return err },
	} {
		wg.Add(1)
		go func(operation func() error) {
			defer wg.Done()
			errors <- operation()
		}(operation)
	}
	wg.Wait()
	for i := 0; i < 3; i++ {
		if err := <-errors; err != nil {
			t.Fatal(err)
		}
	}
	settings, err := store.GetSettings(ctx)
	if err != nil || settings.RetentionMaxAgeDays != 11 || settings.RetentionMaxSizeMb != 99 {
		t.Fatal(settings, err)
	}
	got, err := store.GetEnrollmentHostnames(ctx)
	if err != nil || !got.Configured || !reflect.DeepEqual(got.Hostnames, []string{"example.org"}) {
		t.Fatal(got, err)
	}
}
