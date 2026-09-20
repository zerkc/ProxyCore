package dbimport

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/zerkc/ProxyCore/apps/api/internal/backupcore/dbexport"
	"github.com/zerkc/ProxyCore/apps/api/internal/backupcore/zipextract"
)

type tablePayload struct {
	name string
	rows []map[string]any
}

type tableColumn struct {
	name string
	oid  uint32
}

var configTableNames = func() map[string]struct{} {
	result := make(map[string]struct{}, len(dbexport.DefaultConfigTables))
	for _, table := range dbexport.DefaultConfigTables {
		result[table] = struct{}{}
	}
	return result
}()

func readTablePayloads(ctx context.Context, files map[string]zipextract.File, tableNames []string) ([]tablePayload, error) {
	if len(tableNames) != len(dbexport.DefaultConfigTables) {
		return nil, fmt.Errorf("backup contains %d config tables, want %d", len(tableNames), len(dbexport.DefaultConfigTables))
	}
	seen := make(map[string]struct{}, len(tableNames))
	payloads := make([]tablePayload, 0, len(tableNames))
	for _, table := range tableNames {
		if _, ok := configTableNames[table]; !ok {
			return nil, fmt.Errorf("unsupported backup table %q", table)
		}
		if _, ok := seen[table]; ok {
			return nil, fmt.Errorf("duplicate backup table %q", table)
		}
		seen[table] = struct{}{}
		path := "db/" + table + ".json"
		file, ok := files[path]
		if !ok {
			return nil, fmt.Errorf("archive entry %q is missing", path)
		}
		data, err := readArchiveFile(ctx, file)
		if err != nil {
			return nil, fmt.Errorf("read table %q: %w", table, err)
		}
		rows, err := decodeTableRows(data)
		if err != nil {
			return nil, fmt.Errorf("decode table %q: %w", table, err)
		}
		payloads = append(payloads, tablePayload{name: table, rows: rows})
	}
	for path := range files {
		if !strings.HasPrefix(path, "db/") || !strings.HasSuffix(path, ".json") {
			continue
		}
		table := strings.TrimSuffix(strings.TrimPrefix(path, "db/"), ".json")
		if _, ok := seen[table]; !ok {
			return nil, fmt.Errorf("archive contains an unlisted config table %q", table)
		}
	}
	if len(seen) != len(dbexport.DefaultConfigTables) {
		return nil, fmt.Errorf("backup does not contain all config tables")
	}
	return payloads, nil
}

func decodeTableRows(data []byte) ([]map[string]any, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil, fmt.Errorf("table payload must be a JSON array")
	}
	decoder := json.NewDecoder(strings.NewReader(string(trimmed)))
	decoder.UseNumber()
	var rows []map[string]any
	if err := decoder.Decode(&rows); err != nil {
		return nil, err
	}
	for index, row := range rows {
		if row == nil {
			return nil, fmt.Errorf("row %d must be a JSON object", index)
		}
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("trailing JSON data")
		}
		return nil, err
	}
	return rows, nil
}

func importConfigTables(ctx context.Context, tx pgx.Tx, payloads []tablePayload) ([]TablePreview, error) {
	previews := make([]TablePreview, 0, len(payloads))
	for _, payload := range payloads {
		if err := contextError(ctx); err != nil {
			return previews, err
		}
		quoted := quoteTableIdentifier(payload.name)
		if _, err := tx.Exec(ctx, "TRUNCATE "+quoted+" RESTART IDENTITY CASCADE"); err != nil {
			return previews, fmt.Errorf("truncate table %q: %w", payload.name, err)
		}
		columns, err := tableColumns(ctx, tx, payload.name)
		if err != nil {
			return previews, fmt.Errorf("read columns for %q: %w", payload.name, err)
		}
		if err := registerImportTableTypes(ctx, tx, payload.name); err != nil {
			return previews, fmt.Errorf("register types for %q: %w", payload.name, err)
		}
		rows, err := coerceTableRows(columns, payload.rows)
		if err != nil {
			return previews, fmt.Errorf("coerce table %q: %w", payload.name, err)
		}
		if len(rows) > 0 {
			columnNames := make([]string, len(columns))
			for index, column := range columns {
				columnNames[index] = column.name
			}
			if _, err := tx.CopyFrom(ctx, pgx.Identifier{payload.name}, columnNames, pgx.CopyFromRows(rows)); err != nil {
				return previews, fmt.Errorf("insert table %q: %w", payload.name, err)
			}
		}
		previews = append(previews, TablePreview{Name: payload.name, RowCount: len(payload.rows), Action: "truncate+reinsert"})
	}
	return previews, nil
}

func registerImportTableTypes(ctx context.Context, tx pgx.Tx, table string) error {
	conn := tx.Conn()
	if conn == nil {
		return fmt.Errorf("nil transaction connection")
	}
	rows, err := tx.Query(ctx, `
		select distinct n.nspname || '.' || t.typname, t.oid, t.typtype, t.typname
		from pg_catalog.pg_attribute a
		join pg_catalog.pg_class c on c.oid = a.attrelid
		join pg_catalog.pg_namespace n on n.oid = c.relnamespace
		join pg_catalog.pg_type t on t.oid = a.atttypid
		where n.nspname = current_schema()
		  and c.relname = $1
		  and a.attnum > 0
		  and not a.attisdropped
		  and (t.typtype <> 'b' or t.typname = 'citext')
	`, table)
	if err != nil {
		return err
	}
	var derivedNames []string
	for rows.Next() {
		var qualifiedName, typeName, typeKind string
		var oid uint32
		if err := rows.Scan(&qualifiedName, &oid, &typeKind, &typeName); err != nil {
			rows.Close()
			return err
		}
		if strings.EqualFold(typeName, "citext") && typeKind == "b" {
			conn.TypeMap().RegisterType(&pgtype.Type{Name: qualifiedName, OID: oid, Codec: pgtype.TextCodec{}})
			continue
		}
		derivedNames = append(derivedNames, qualifiedName)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if len(derivedNames) == 0 {
		return nil
	}
	_, err = conn.LoadTypes(ctx, derivedNames)
	return err
}

func tableColumns(ctx context.Context, tx pgx.Tx, table string) ([]tableColumn, error) {
	rows, err := tx.Query(ctx, "select * from "+quoteTableIdentifier(table)+" limit 0")
	if err != nil {
		return nil, err
	}
	fields := rows.FieldDescriptions()
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	columns := make([]tableColumn, len(fields))
	for index, field := range fields {
		columns[index] = tableColumn{name: field.Name, oid: field.DataTypeOID}
	}
	return columns, nil
}

func coerceTableRows(columns []tableColumn, source []map[string]any) ([][]any, error) {
	known := make(map[string]struct{}, len(columns))
	for _, column := range columns {
		known[column.name] = struct{}{}
	}
	result := make([][]any, len(source))
	for rowIndex, sourceRow := range source {
		for name := range sourceRow {
			if _, ok := known[name]; !ok {
				return nil, fmt.Errorf("unknown column %q in row %d", name, rowIndex)
			}
		}
		row := make([]any, len(columns))
		for columnIndex, column := range columns {
			value := sourceRow[column.name]
			coerced, err := coerceImportValue(column.oid, value)
			if err != nil {
				return nil, fmt.Errorf("column %q in row %d: %w", column.name, rowIndex, err)
			}
			row[columnIndex] = coerced
		}
		result[rowIndex] = row
	}
	return result, nil
}

func quoteTableIdentifier(table string) string {
	return `"` + strings.ReplaceAll(table, `"`, `""`) + `"`
}
