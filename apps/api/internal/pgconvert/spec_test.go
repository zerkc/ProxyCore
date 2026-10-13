package pgconvert

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

func migratedSQLite(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sqlitedb.Open(filepath.Join(t.TempDir(), "spec.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := sqlitedb.Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestSpecMatchesSQLiteSchema(t *testing.T) {
	db := migratedSQLite(t)
	specs := Tables()
	if len(specs) != 30 {
		t.Fatalf("spec tables = %d, want 30", len(specs))
	}
	position := map[string]int{}
	for i, table := range specs {
		position[table.Name] = i
	}
	var sqliteTables int
	if err := db.QueryRow(`select count(*) from sqlite_master where type='table' and name not like 'sqlite_%' and name <> 'proxycore_schema_migrations'`).Scan(&sqliteTables); err != nil {
		t.Fatal(err)
	}
	for _, table := range specs {
		rows, err := db.Query(`select name, type, "notnull" from pragma_table_info(?) order by cid`, table.Name)
		if err != nil {
			t.Fatal(err)
		}
		var index int
		for rows.Next() {
			var name, typ string
			var notNull int
			if err := rows.Scan(&name, &typ, &notNull); err != nil {
				t.Fatal(err)
			}
			if index >= len(table.Columns) {
				t.Fatalf("%s: SQLite has extra column %s", table.Name, name)
			}
			column := table.Columns[index]
			index++
			if column.Name != name {
				t.Fatalf("%s column %d: spec %s, SQLite %s", table.Name, index, column.Name, name)
			}
			if want := column.Kind.sqliteType(); want != typ {
				t.Errorf("%s.%s: SQLite type %s, spec %s", table.Name, name, typ, want)
			}
			if column.Nullable != (notNull == 0) {
				t.Errorf("%s.%s: nullable spec %v, SQLite notnull %d", table.Name, name, column.Nullable, notNull)
			}
			if column.Older && column.Default == nil && !column.Nullable {
				t.Errorf("%s.%s: tolerated-missing NOT NULL column needs a default", table.Name, name)
			}
		}
		rows.Close()
		if index != len(table.Columns) {
			t.Errorf("%s: spec has %d columns, SQLite %d", table.Name, len(table.Columns), index)
		}
		fks, err := db.Query(`select "table" from pragma_foreign_key_list(?)`, table.Name)
		if err != nil {
			t.Fatal(err)
		}
		for fks.Next() {
			var ref string
			if err := fks.Scan(&ref); err != nil {
				t.Fatal(err)
			}
			if position[ref] >= position[table.Name] {
				t.Errorf("%s is copied before its FK parent %s", table.Name, ref)
			}
		}
		fks.Close()
	}
	if sqliteTables != len(specs) {
		t.Errorf("SQLite has %d tables, spec %d", sqliteTables, len(specs))
	}
}
