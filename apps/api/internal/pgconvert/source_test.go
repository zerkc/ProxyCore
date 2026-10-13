package pgconvert

import (
	"context"
	"errors"
	"testing"

	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

func openSource(t *testing.T, f pgFixture) (*Source, *Plan) {
	t.Helper()
	ctx := context.Background()
	src, err := OpenSource(ctx, f.URL)
	if err != nil {
		t.Fatalf("OpenSource: %v", err)
	}
	t.Cleanup(func() { src.Close(ctx) })
	plan, err := src.Plan(ctx)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	return src, plan
}

func readAll(t *testing.T, src *Source, plan *Plan, table string) [][]any {
	t.Helper()
	var rows [][]any
	if err := src.ReadTable(context.Background(), plan, table, func(row []any) error {
		rows = append(rows, row)
		return nil
	}); err != nil {
		t.Fatalf("ReadTable %s: %v", table, err)
	}
	return rows
}

func column(t *testing.T, table, name string) int {
	t.Helper()
	for _, spec := range Tables() {
		if spec.Name != table {
			continue
		}
		for i, c := range spec.Columns {
			if c.Name == name {
				return i
			}
		}
	}
	t.Fatalf("no column %s.%s", table, name)
	return -1
}

func findRow(t *testing.T, rows [][]any, idColumn int, id string) []any {
	t.Helper()
	for _, row := range rows {
		if row[idColumn] == id {
			return row
		}
	}
	t.Fatalf("row %s not found", id)
	return nil
}

func TestSourceReadsEveryProductTable(t *testing.T) {
	f := newSeededPG(t)
	src, plan := openSource(t, f)
	for _, spec := range Tables() {
		if rows := readAll(t, src, plan, spec.Name); len(rows) == 0 {
			t.Errorf("%s: no rows read", spec.Name)
		}
		count, err := src.CountRows(context.Background(), spec.Name)
		if err != nil || count != int64(len(readAll(t, src, plan, spec.Name))) {
			t.Errorf("%s: CountRows = %d, %v", spec.Name, count, err)
		}
	}
}

func TestSourceValueFidelity(t *testing.T) {
	f := newSeededPG(t)
	src, plan := openSource(t, f)

	users := readAll(t, src, plan, "users")
	u2 := findRow(t, users, 0, fixtureUUID("2"))
	if got := u2[column(t, "users", "username")]; got != "Zoë 日本語 🚀 'quoted'" {
		t.Errorf("unicode username = %q", got)
	}
	if u2[column(t, "users", "active")] != int64(0) || u2[column(t, "users", "password_change_required")] != int64(1) {
		t.Errorf("booleans = %v %v", u2[column(t, "users", "active")], u2[column(t, "users", "password_change_required")])
	}
	wantFinite := func(seconds int64, nanos int32) any {
		key, err := sqlitedb.EncodeTimestamp(sqlitedb.FiniteTimestamp{Seconds: seconds, Nanos: nanos})
		if err != nil {
			t.Fatal(err)
		}
		return *key
	}
	if got := u2[column(t, "users", "created_at")]; got != wantFinite(-1, 999999000) {
		t.Errorf("pre-1970 microsecond timestamp = %v", got)
	}
	if got := u2[column(t, "users", "updated_at")]; got != "2" {
		t.Errorf("infinity = %v, want tag 2", got)
	}
	u1 := findRow(t, users, 0, fixtureUUID("1"))
	if got := u1[column(t, "users", "created_at")]; got != wantFinite(1791376496, 123456000) {
		t.Errorf("microsecond timestamp = %v", got)
	}

	sessions := readAll(t, src, plan, "sessions")
	s3 := findRow(t, sessions, 0, fixtureUUID("3"))
	if s3[column(t, "sessions", "expires_at")] != "2" || s3[column(t, "sessions", "revoked_at")] != "0" {
		t.Errorf("infinities = %v %v", s3[column(t, "sessions", "expires_at")], s3[column(t, "sessions", "revoked_at")])
	}
	s4 := findRow(t, sessions, 0, fixtureUUID("4"))
	if s4[column(t, "sessions", "revoked_at")] != nil || s4[column(t, "sessions", "last_seen_at")] != nil {
		t.Errorf("NULL timestamps not preserved: %v", s4)
	}

	identity := readAll(t, src, plan, "installation_identity")[0]
	if identity[column(t, "installation_identity", "leadership_generation")] != int64(9223372036854775807) {
		t.Errorf("bigint = %v", identity[column(t, "installation_identity", "leadership_generation")])
	}
	jobs := readAll(t, src, plan, "apply_jobs")
	j25 := findRow(t, jobs, 0, fixtureUUID("25"))
	if j25[column(t, "apply_jobs", "leadership_generation")] != int64(-9223372036854775808) {
		t.Errorf("min bigint = %v", j25[column(t, "apply_jobs", "leadership_generation")])
	}

	snapshots := readAll(t, src, plan, "applied_snapshots")
	body := findRow(t, snapshots, 0, fixtureUUID("34"))[column(t, "applied_snapshots", "snapshot_body")]
	blob, ok := body.([]byte)
	if !ok || len(blob) != 256 {
		t.Fatalf("snapshot_body = %T len %d", body, len(blob))
	}
	for i, b := range blob {
		if int(b) != i {
			t.Fatalf("byte %d = %d", i, b)
		}
	}
	empty := findRow(t, snapshots, 0, fixtureUUID("35"))[column(t, "applied_snapshots", "snapshot_body")]
	if b, ok := empty.([]byte); !ok || b == nil || len(b) != 0 {
		t.Errorf("empty bytea = %#v, want empty non-nil []byte", empty)
	}
	if got := findRow(t, snapshots, 0, fixtureUUID("36"))[column(t, "applied_snapshots", "snapshot_body")]; got != nil {
		t.Errorf("NULL bytea = %#v", got)
	}

	// jsonb is copied as the raw PostgreSQL text, never through float64.
	records := readAll(t, src, plan, "dns_records")
	value := findRow(t, records, 0, fixtureUUID("9"))[column(t, "dns_records", "value")]
	conn := f.connect(t)
	var want string
	if err := conn.QueryRow(context.Background(), `select value::text from dns_records where id = $1`, fixtureUUID("9")).Scan(&want); err != nil {
		t.Fatal(err)
	}
	if value != want {
		t.Errorf("jsonb = %v, want %v", value, want)
	}
	for _, fragment := range []string{"12345678901234567890", `"k": 2`} {
		if s, _ := value.(string); !contains(s, fragment) {
			t.Errorf("jsonb %q lost fragment %q", s, fragment)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestSourceIsReadOnly(t *testing.T) {
	f := newSeededPG(t)
	src, _ := openSource(t, f)
	if _, err := src.tx.Exec(context.Background(), `create table forbidden (id int)`); err == nil {
		t.Fatal("write succeeded on the source transaction")
	}
}

func TestSourceSnapshotIsFixedAtOpen(t *testing.T) {
	f := newSeededPG(t)
	src, plan := openSource(t, f)
	before := len(readAll(t, src, plan, "zones"))
	f.exec(t, expandUUIDs(`insert into zones (id, name) values ('{u98}','later.test')`))
	if after := len(readAll(t, src, plan, "zones")); after != before {
		t.Errorf("snapshot moved: %d -> %d", before, after)
	}
}

func schemaKind(t *testing.T, err error) (SchemaErrorKind, string, string) {
	t.Helper()
	var schemaErr *SchemaError
	if !errors.As(err, &schemaErr) {
		t.Fatalf("error %v is not a *SchemaError", err)
	}
	return schemaErr.Kind, schemaErr.Table, schemaErr.Column
}

func planErr(t *testing.T, f pgFixture) error {
	t.Helper()
	src, err := OpenSource(context.Background(), f.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close(context.Background())
	_, err = src.Plan(context.Background())
	return err
}

func TestSourceFailsClosedOnSchemaDrift(t *testing.T) {
	cases := []struct {
		name   string
		sql    string
		kind   SchemaErrorKind
		table  string
		column string
	}{
		{"unknown table", `create table surprise (id int)`, ErrUnknownTable, "surprise", ""},
		{"missing table", `drop table internal_ca_enrollment_state`, ErrMissingTable, "internal_ca_enrollment_state", ""},
		{"unknown column", `alter table zones add column extra text`, ErrUnknownColumn, "zones", "extra"},
		{"missing required column", `alter table zones drop column name`, ErrMissingColumn, "zones", "name"},
		{"column type changed", `alter table zones alter column enabled drop default, alter column enabled type integer using enabled::int`, ErrTypeMismatch, "zones", "enabled"},
		{"unknown enum value", `alter type proxycore_role add value 'admin'`, ErrUnknownEnumValue, "users", "role"},
		{"wrong enum type", `alter table dns_records alter column type type text using type::text`, ErrTypeMismatch, "dns_records", "type"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newPGFixture(t)
			f.exec(t, tc.sql)
			kind, table, column := schemaKind(t, planErr(t, f))
			if kind != tc.kind || table != tc.table || column != tc.column {
				t.Errorf("got %s %s.%s, want %s %s.%s", kind, table, column, tc.kind, tc.table, tc.column)
			}
		})
	}
}

func TestSourceToleratesOlderVariantGaps(t *testing.T) {
	f := newSeededPG(t)
	for _, sql := range []string{
		`alter table certificates drop column failure_reason`,
		`alter table users drop column password_change_required`,
		`alter table applied_snapshots drop column status, drop column apply_job_id, drop column failure_code, drop column snapshot_body`,
		`alter table node_state drop column enrollment_attempt_id, drop column primary_url, drop column primary_installation_id, drop column primary_tls_spki_sha256, drop column credential_id, drop column sync_enabled, drop column last_attempt_at, drop column last_success_at, drop column consecutive_failures, drop column next_attempt_at, drop column last_error_code`,
		`alter table enrollment_grants drop column verified_preview_digest`,
		`alter table installation_settings drop column enrollment_hostnames`,
	} {
		f.exec(t, sql)
	}
	src, plan := openSource(t, f)
	snapshots := readAll(t, src, plan, "applied_snapshots")
	if got := snapshots[0][column(t, "applied_snapshots", "status")]; got != "applied" {
		t.Errorf("default status = %v", got)
	}
	if got := snapshots[0][column(t, "applied_snapshots", "snapshot_body")]; got != nil {
		t.Errorf("missing snapshot_body = %v, want NULL", got)
	}
	state := readAll(t, src, plan, "node_state")[0]
	if state[column(t, "node_state", "sync_enabled")] != int64(0) || state[column(t, "node_state", "consecutive_failures")] != int64(0) {
		t.Errorf("node_state defaults = %v", state)
	}
	if got := readAll(t, src, plan, "users")[0][column(t, "users", "password_change_required")]; got != int64(0) {
		t.Errorf("password_change_required default = %v", got)
	}
	if len(plan.Gaps()) == 0 {
		t.Error("plan reports no tolerated gaps")
	}
}

func TestSourceRejectsNullInRequiredColumn(t *testing.T) {
	f := newSeededPG(t)
	f.exec(t, `alter table users alter column username drop not null`)
	f.exec(t, expandUUIDs(`insert into users (id, username, password_hash, role) values ('{u97}', null, 'h', 'owner')`))
	src, plan := openSource(t, f)
	err := src.ReadTable(context.Background(), plan, "users", func([]any) error { return nil })
	var dataErr *DataError
	if !errors.As(err, &dataErr) || dataErr.Table != "users" || dataErr.Column != "username" {
		t.Fatalf("err = %v, want DataError users.username", err)
	}
}
