package dbexport

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zerkc/ProxyCore/apps/api/internal/auth"
	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
)

func openExporterTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url, ok := testDatabaseURL()
	if !ok {
		t.Skip("no test database URL is set")
	}
	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse test database URL: %v", err)
	}
	config.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatalf("create test pool: %v", err)
	}
	t.Cleanup(pool.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("ping test database: %v", err)
	}
	if err := auth.NewPostgresStore(pool).EnsureSchema(ctx); err != nil {
		t.Fatalf("ensure auth schema: %v", err)
	}
	if err := configuration.EnsureSchema(ctx, pool); err != nil {
		t.Fatalf("ensure configuration schema: %v", err)
	}
	return pool
}

func TestExporterPostgresFixtureRoundTrip(t *testing.T) {
	pool := openExporterTestPool(t)
	ctx := context.Background()
	suffix := time.Now().UTC().UnixNano()
	fixture := fmt.Sprintf("dbexport_fixture_%d", suffix)
	nulls := fmt.Sprintf("dbexport_nulls_%d", suffix)
	empty := fmt.Sprintf("dbexport_empty_%d", suffix)
	for _, statement := range []string{
		"create table " + quoteTestIdentifier(fixture) + ` (
			id uuid primary key,
			bytes bytea not null,
			text_value text not null,
			varchar_value varchar not null,
			char_value char(4) not null,
			timestamptz_value timestamptz not null,
			timestamp_value timestamp not null,
			jsonb_value jsonb not null,
			json_value json not null,
			bool_value boolean not null,
			int2_value smallint not null,
			int4_value integer not null,
			int8_value bigint not null,
			numeric_value numeric not null,
			float4_value real not null,
			float8_value double precision not null,
			inet_value inet not null,
			cidr_value cidr not null,
			macaddr_value macaddr not null,
			enum_value proxycore_record_type not null,
			enrollment_primary_id uuid,
			retired_at timestamptz
		)`,
		"create table " + quoteTestIdentifier(nulls) + " (nullable_text text, nullable_json jsonb, nullable_uuid uuid, nullable_time timestamptz, nullable_bytes bytea)",
		"create table " + quoteTestIdentifier(empty) + " (id integer primary key)",
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("create exporter fixture: %v", err)
		}
	}
	t.Cleanup(func() {
		for _, table := range []string{fixture, nulls, empty} {
			_, _ = pool.Exec(ctx, "drop table if exists "+quoteTestIdentifier(table))
		}
	})

	insert := "insert into " + quoteTestIdentifier(fixture) + ` (
		id, bytes, text_value, varchar_value, char_value, timestamptz_value,
		timestamp_value, jsonb_value, json_value, bool_value, int2_value, int4_value,
		int8_value, numeric_value, float4_value, float8_value, inet_value, cidr_value,
		macaddr_value, enum_value, enrollment_primary_id, retired_at
	) values (
		'11111111-1111-4111-8111-111111111111', decode('0001ff', 'hex'), 'hello ✓', 'varchar', 'char',
		'2026-01-02T03:04:05.678900Z', '2026-01-02 03:04:05.678900',
		'{"nested":[1,true,null],"message":"unicode ✓"}', '{"plain": ["json", 2]}', true,
		7, 8, 9007199254740991, 123.45, 1.5, 2.5, '192.0.2.1', '192.0.2.0/24',
		'08:00:2b:01:02:03', 'A', '22222222-2222-4222-8222-222222222222', null
	)`
	if _, err := pool.Exec(ctx, insert); err != nil {
		t.Fatalf("insert exporter fixture: %v", err)
	}
	if _, err := pool.Exec(ctx, "insert into "+quoteTestIdentifier(nulls)+" default values"); err != nil {
		t.Fatalf("insert all-null fixture: %v", err)
	}

	exporter := New(pool, ExporterOptions{
		Tables: []string{fixture, nulls, empty},
		Now:    func() time.Time { return time.Date(2026, time.January, 2, 4, 0, 0, 0, time.UTC) },
	})
	writer := newMemWriter()
	report, err := exporter.Export(ctx, writer)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if len(report.Tables) != 3 {
		t.Fatalf("report tables = %d, want 3", len(report.Tables))
	}
	for _, stats := range report.Tables {
		if stats.Error != nil {
			t.Fatalf("table %s reported error: %v", stats.Name, stats.Error)
		}
	}

	var gotFixture []map[string]any
	if err := json.Unmarshal(writer.files["db/"+fixture+".json"], &gotFixture); err != nil {
		t.Fatalf("unmarshal fixture JSON: %v", err)
	}
	wantFixture := []map[string]any{{
		"id":                    "11111111-1111-4111-8111-111111111111",
		"bytes":                 "AAH_",
		"text_value":            "hello ✓",
		"varchar_value":         "varchar",
		"char_value":            "char",
		"timestamptz_value":     "2026-01-02T03:04:05.6789Z",
		"timestamp_value":       "2026-01-02T03:04:05.6789Z",
		"jsonb_value":           map[string]any{"nested": []any{float64(1), true, nil}, "message": "unicode ✓"},
		"json_value":            map[string]any{"plain": []any{"json", float64(2)}},
		"bool_value":            true,
		"int2_value":            float64(7),
		"int4_value":            float64(8),
		"int8_value":            float64(9007199254740991),
		"numeric_value":         float64(123.45),
		"float4_value":          float64(1.5),
		"float8_value":          float64(2.5),
		"inet_value":            netip.MustParsePrefix("192.0.2.1/32").String(),
		"cidr_value":            "192.0.2.0/24",
		"macaddr_value":         "08:00:2b:01:02:03",
		"enum_value":            "A",
		"enrollment_primary_id": "22222222-2222-4222-8222-222222222222",
		"retired_at":            nil,
	}}
	if !reflect.DeepEqual(gotFixture, wantFixture) {
		t.Fatalf("fixture JSON = %#v, want %#v", gotFixture, wantFixture)
	}

	var gotNulls []map[string]any
	if err := json.Unmarshal(writer.files["db/"+nulls+".json"], &gotNulls); err != nil {
		t.Fatalf("unmarshal all-null JSON: %v", err)
	}
	if !reflect.DeepEqual(gotNulls, []map[string]any{{
		"nullable_text":  nil,
		"nullable_json":  nil,
		"nullable_uuid":  nil,
		"nullable_time":  nil,
		"nullable_bytes": nil,
	}}) {
		t.Fatalf("all-null JSON = %#v", gotNulls)
	}
	if got := string(writer.files["db/"+empty+".json"]); got != "[]" {
		t.Fatalf("empty table JSON = %q, want []", got)
	}
}

func TestExporterPostgresLargeTableStreamsRows(t *testing.T) {
	pool := openExporterTestPool(t)
	ctx := context.Background()
	table := fmt.Sprintf("dbexport_big_%d", time.Now().UTC().UnixNano())
	if _, err := pool.Exec(ctx, "create table "+quoteTestIdentifier(table)+" (id integer primary key, value text not null)"); err != nil {
		t.Fatalf("create big fixture: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, "drop table if exists "+quoteTestIdentifier(table)) })

	rows := make([][]any, 10000)
	for i := range rows {
		rows[i] = []any{i, fmt.Sprintf("value-%d", i)}
	}
	if _, err := pool.CopyFrom(ctx, pgx.Identifier{table}, []string{"id", "value"}, pgx.CopyFromRows(rows)); err != nil {
		t.Fatalf("insert big fixture: %v", err)
	}

	writer := newMemWriter()
	report, err := New(pool, ExporterOptions{Tables: []string{table}}).Export(ctx, writer)
	if err != nil {
		t.Fatalf("Export big table: %v", err)
	}
	if len(report.Tables) != 1 || report.Tables[0].RowCount != 10000 {
		t.Fatalf("big report = %#v, want one table with 10000 rows", report.Tables)
	}
}
