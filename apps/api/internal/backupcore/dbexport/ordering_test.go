package dbexport

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func openOrderingTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("PGX_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("PGX_TEST_DATABASE_URL is not set")
	}
	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse PGX_TEST_DATABASE_URL: %v", err)
	}
	config.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatalf("create test pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(context.Background()); err != nil {
		t.Fatalf("ping test database: %v", err)
	}
	return pool
}

func quoteTestIdentifier(name string) string {
	return `"` + name + `"`
}

func TestExportOrderPlacesIndependentTablesAlphabeticallyBeforeDependents(t *testing.T) {
	pool := openOrderingTestPool(t)
	ctx := context.Background()
	suffix := time.Now().UTC().UnixNano()
	a := fmt.Sprintf("dbexport_order_a_%d", suffix)
	b := fmt.Sprintf("dbexport_order_b_%d", suffix)
	c := fmt.Sprintf("dbexport_order_c_%d", suffix)
	for _, statement := range []string{
		"create table " + quoteTestIdentifier(a) + " (id integer primary key)",
		"create table " + quoteTestIdentifier(b) + " (id integer primary key, a_id integer not null references " + quoteTestIdentifier(a) + "(id))",
		"create table " + quoteTestIdentifier(c) + " (id integer primary key)",
	} {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatalf("create ordering fixture: %v", err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "drop table if exists "+quoteTestIdentifier(b))
		_, _ = pool.Exec(ctx, "drop table if exists "+quoteTestIdentifier(c))
		_, _ = pool.Exec(ctx, "drop table if exists "+quoteTestIdentifier(a))
	})

	exporter := &Exporter{pool: pool, tables: []string{a, b, c}}
	got, err := exporter.exportOrder(ctx)
	if err != nil {
		t.Fatalf("exportOrder: %v", err)
	}
	want := []string{a, c, b}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("exportOrder = %v, want %v", got, want)
	}
}

func TestExportOrderReportsMissingConfiguredTables(t *testing.T) {
	pool := openOrderingTestPool(t)
	exporter := &Exporter{pool: pool, tables: []string{"dbexport_table_that_does_not_exist"}}
	_, err := exporter.exportOrder(context.Background())
	if err == nil {
		t.Fatal("exportOrder accepted a missing configured table")
	}
	var missing *MissingTablesError
	if !errors.As(err, &missing) {
		t.Fatalf("error = %v, want MissingTablesError", err)
	}
}

func TestTopologicalOrderReportsCyclePath(t *testing.T) {
	_, err := topologicalOrder(
		[]string{"a", "b"},
		[]tableEdge{{parent: "a", child: "b"}, {parent: "b", child: "a"}},
	)
	if err == nil {
		t.Fatal("topologicalOrder accepted a cycle")
	}
	var cycle *ForeignKeyCycleError
	if !errors.As(err, &cycle) {
		t.Fatalf("error = %v, want ForeignKeyCycleError", err)
	}
	if len(cycle.Path) < 3 || cycle.Path[0] != cycle.Path[len(cycle.Path)-1] {
		t.Fatalf("cycle path = %v, want closed path", cycle.Path)
	}
}
