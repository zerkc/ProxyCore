package pgconvert

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

func convertTo(t *testing.T, f pgFixture) (*Report, string) {
	t.Helper()
	ctx := context.Background()
	src, err := OpenSource(ctx, f.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close(ctx)
	dest := filepath.Join(t.TempDir(), "out.db")
	report, err := Convert(ctx, src, dest)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	return report, dest
}

func openDest(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sqlitedb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func queryString(t *testing.T, db *sql.DB, query string, args ...any) string {
	t.Helper()
	var out sql.NullString
	if err := db.QueryRow(query, args...).Scan(&out); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return out.String
}

func TestConvertCopiesEveryTableWithMatchingHashes(t *testing.T) {
	f := newSeededPG(t)
	report, dest := convertTo(t, f)
	db := openDest(t, dest)

	if len(report.Tables) != 30 {
		t.Fatalf("report has %d tables", len(report.Tables))
	}
	ctx := context.Background()
	src, err := OpenSource(ctx, f.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close(ctx)
	plan, err := src.Plan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, spec := range Tables() {
		got := report.Tables[spec.Name]
		if got.SourceRows == 0 || got.SourceRows != got.DestRows || got.Hash == "" || got.Hash != got.DestHash {
			t.Errorf("%s: report %+v", spec.Name, got)
		}
		// Independent re-computation on both sides, as the validator will do.
		srcRows, srcHash, err := HashPGTable(ctx, src, plan, spec.Name)
		if err != nil {
			t.Fatal(err)
		}
		dstRows, dstHash, err := HashSQLiteTable(ctx, db, spec)
		if err != nil {
			t.Fatal(err)
		}
		if srcRows != dstRows || srcHash != dstHash || srcHash != got.Hash {
			t.Errorf("%s: source %d/%s dest %d/%s report %s", spec.Name, srcRows, srcHash, dstRows, dstHash, got.Hash)
		}
	}
	if got := queryString(t, db, `pragma integrity_check`); got != "ok" {
		t.Errorf("integrity_check = %s", got)
	}
	var violations int
	rows, err := db.Query(`pragma foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		violations++
	}
	rows.Close()
	if violations != 0 {
		t.Errorf("foreign_key_check reported %d violations", violations)
	}
}

func TestConvertPreservesRepresentations(t *testing.T) {
	f := newSeededPG(t)
	_, dest := convertTo(t, f)
	db := openDest(t, dest)

	if got := queryString(t, db, `select username from users where id = ?`, fixtureUUID("2")); got != "Zoë 日本語 🚀 'quoted'" {
		t.Errorf("unicode = %q", got)
	}
	if got := queryString(t, db, `select expires_at || '/' || revoked_at from sessions where id = ?`, fixtureUUID("3")); got != "2/0" {
		t.Errorf("infinities = %q", got)
	}
	if got := queryString(t, db, `select cast(leadership_generation as text) from installation_identity`); got != "9223372036854775807" {
		t.Errorf("bigint = %q", got)
	}
	if got := queryString(t, db, `select cast(leadership_generation as text) from apply_jobs where id = ?`, fixtureUUID("25")); got != "-9223372036854775808" {
		t.Errorf("min bigint = %q", got)
	}
	var body []byte
	if err := db.QueryRow(`select snapshot_body from applied_snapshots where id = ?`, fixtureUUID("34")).Scan(&body); err != nil || len(body) != 256 {
		t.Fatalf("blob: %v len %d", err, len(body))
	}
	for i, b := range body {
		if int(b) != i {
			t.Fatalf("blob byte %d = %d", i, b)
		}
	}
	if got := queryString(t, db, `select typeof(snapshot_body) || length(snapshot_body) from applied_snapshots where id = ?`, fixtureUUID("35")); got != "blob0" {
		t.Errorf("empty bytea = %q, want blob0", got)
	}
	if got := queryString(t, db, `select typeof(snapshot_body) from applied_snapshots where id = ?`, fixtureUUID("36")); got != "null" {
		t.Errorf("NULL bytea = %q", got)
	}
	var want string
	if err := f.connect(t).QueryRow(context.Background(), `select value::text from dns_records where id = $1`, fixtureUUID("9")).Scan(&want); err != nil {
		t.Fatal(err)
	}
	if got := queryString(t, db, `select value from dns_records where id = ?`, fixtureUUID("9")); got != want {
		t.Errorf("jsonb = %q, want %q", got, want)
	}
	if got := queryString(t, db, `select ciphertext from secrets where id = ?`, fixtureUUID("17")); got != "v1:ünicode ciphertext" {
		t.Errorf("ciphertext = %q", got)
	}
	if got := queryString(t, db, `select active || password_change_required from users where id = ?`, fixtureUUID("2")); got != "01" {
		t.Errorf("booleans = %q", got)
	}
}

func TestConvertReportsDanglingAttemptReferences(t *testing.T) {
	f := newSeededPG(t)
	report, dest := convertTo(t, f)
	want := map[string]string{
		"enrollment_tokens.consumed_by_attempt_id": fixtureUUID("90"),
		"enrollment_grants.attempt_id":             fixtureUUID("91"),
		"enrolled_nodes.created_by_attempt_id":     fixtureUUID("92"),
	}
	if len(report.Warnings) != len(want) {
		t.Fatalf("warnings = %+v", report.Warnings)
	}
	for _, w := range report.Warnings {
		sample, ok := want[w.Table+"."+w.Column]
		if !ok || w.Code != "dangling_reference" || w.Count != 1 || len(w.Samples) != 1 || w.Samples[0] != sample {
			t.Errorf("unexpected warning %+v", w)
		}
	}
	// The dangling values are copied as-is.
	db := openDest(t, dest)
	if got := queryString(t, db, `select consumed_by_attempt_id from enrollment_tokens where id = ?`, fixtureUUID("39")); got != fixtureUUID("90") {
		t.Errorf("dangling value = %q", got)
	}
}

func TestConvertEmptySchemaProducesEmptyReport(t *testing.T) {
	report, dest := convertTo(t, newPGFixture(t))
	if len(report.Warnings) != 0 || len(report.Tables) != 30 {
		t.Fatalf("report = %+v", report)
	}
	for name, table := range report.Tables {
		if table.SourceRows != 0 || table.DestRows != 0 || table.Hash != table.DestHash {
			t.Errorf("%s: %+v", name, table)
		}
	}
	if _, err := os.Stat(dest); err != nil {
		t.Fatal(err)
	}
}

func TestConvertOlderVariantAppliesDefaults(t *testing.T) {
	f := newSeededPG(t)
	f.exec(t, `alter table applied_snapshots drop column status, drop column snapshot_body`)
	f.exec(t, `alter table node_state drop column sync_enabled, drop column consecutive_failures`)
	f.exec(t, `alter table users drop column password_change_required`)
	report, dest := convertTo(t, f)
	db := openDest(t, dest)
	if got := queryString(t, db, `select status from applied_snapshots where id = ?`, fixtureUUID("36")); got != "applied" {
		t.Errorf("status default = %q", got)
	}
	if got := queryString(t, db, `select sync_enabled || consecutive_failures || (select count(*) from users where password_change_required = 0) from node_state`); got != "002" {
		t.Errorf("defaults = %q", got)
	}
	if len(report.Gaps) != 5 {
		t.Errorf("gaps = %+v", report.Gaps)
	}
	for name, table := range report.Tables {
		if table.Hash != table.DestHash {
			t.Errorf("%s hash mismatch", name)
		}
	}
}

func TestConvertRefusesExistingDestination(t *testing.T) {
	f := newSeededPG(t)
	ctx := context.Background()
	src, err := OpenSource(ctx, f.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close(ctx)
	dest := filepath.Join(t.TempDir(), "existing.db")
	if err := os.WriteFile(dest, []byte("precious"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Convert(ctx, src, dest); !errors.Is(err, ErrDestinationExists) {
		t.Fatalf("err = %v, want ErrDestinationExists", err)
	}
	if raw, _ := os.ReadFile(dest); string(raw) != "precious" {
		t.Errorf("existing destination modified: %q", raw)
	}
}

func TestConvertFailureLeavesNoDestination(t *testing.T) {
	cases := map[string]func(*testing.T, pgFixture){
		"schema drift": func(t *testing.T, f pgFixture) { f.exec(t, `create table surprise (id int)`) },
		"foreign key violation": func(t *testing.T, f pgFixture) {
			f.exec(t, `alter table dns_records drop constraint dns_records_zone_id_zones_id_fk`)
			f.exec(t, expandUUIDs(`insert into dns_records (id, zone_id, name, type, value) values ('{u96}','{u95}','orphan','A','"x"')`))
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := newSeededPG(t)
			mutate(t, f)
			ctx := context.Background()
			src, err := OpenSource(ctx, f.URL)
			if err != nil {
				t.Fatal(err)
			}
			defer src.Close(ctx)
			dir := t.TempDir()
			dest := filepath.Join(dir, "out.db")
			if _, err := Convert(ctx, src, dest); err == nil {
				t.Fatal("Convert succeeded")
			}
			entries, _ := os.ReadDir(dir)
			if len(entries) != 0 {
				t.Errorf("leftover files: %v", entries)
			}
		})
	}
}
