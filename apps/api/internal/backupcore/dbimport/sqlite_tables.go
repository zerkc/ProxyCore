package dbimport

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/zerkc/ProxyCore/apps/api/internal/backupcore/dbexport"
	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

// sqliteDependentDeletes lists, children first, the tables OUTSIDE the 16
// bundle tables that reference them (directly or transitively) through a
// foreign key of the migrated schema, plus the delete predicate applied to each.
//
// PostgreSQL replaces the bundle with TRUNCATE ... CASCADE, which also empties
// every table with a foreign key into a truncated table. SQLite has no
// TRUNCATE CASCADE and its foreign keys are NO ACTION, so the importer deletes
// these tables itself, in this order, before it deletes the 16 tables in
// reverse foreign-key order:
//
//   - sessions are cleared completely: every user is logged out after an
//     import, including the actor who triggered it.
//   - apply_jobs, applied_snapshots, standalone_archives, operational_artifacts,
//     sync_attempts, node_snapshot_acks, enrollment_attempts, enrollment_tokens
//     and enrollment_grants held references to replaced revisions, users,
//     secrets or cluster keys and cannot outlive them.
//   - audit_events rows that name an actor are removed (their actor is
//     replaced); actor-less rows (for example failed logins) are kept.
//
// TestSQLiteDependentTablesMatchSchema recomputes the closure from
// PRAGMA foreign_key_list and fails when a new dependent table is added to the
// schema without being handled here.
var sqliteDependentDeletes = []struct{ table, where string }{
	{"enrollment_attempts", ""},
	{"sync_attempts", ""},
	{"node_snapshot_acks", ""},
	{"enrollment_grants", ""},
	{"enrollment_tokens", ""},
	{"applied_snapshots", ""},
	{"standalone_archives", ""},
	{"operational_artifacts", ""},
	{"apply_jobs", ""},
	{"sessions", ""},
	{"audit_events", "actor_user_id IS NOT NULL"},
}

// SQLiteDependentTables returns the dependent table names in deletion order.
func SQLiteDependentTables() []string {
	names := make([]string, len(sqliteDependentDeletes))
	for index, entry := range sqliteDependentDeletes {
		names[index] = entry.table
	}
	return names
}

// sqliteImportLocks holds one in-process single-flight mutex per database
// handle; BEGIN IMMEDIATE serializes writers across processes.
var sqliteImportLocks sync.Map // *sql.DB -> *sync.Mutex

// SQLiteTableImporter is the SQLite TableImporter: the whole replace happens in
// one sqlitedb.Writer transaction (BEGIN IMMEDIATE), so a failure at any table
// rolls every change back.
type SQLiteTableImporter struct {
	db     *sql.DB
	writer *sqlitedb.Writer
}

var _ TableImporter = (*SQLiteTableImporter)(nil)

// NewSQLiteTableImporter builds the importer. A nil writer is created from db.
func NewSQLiteTableImporter(db *sql.DB, writer *sqlitedb.Writer) *SQLiteTableImporter {
	if writer == nil && db != nil {
		writer = sqlitedb.NewWriter(db, nil)
	}
	return &SQLiteTableImporter{db: db, writer: writer}
}

type sqliteTablePlan struct {
	table   string
	columns []dbexport.SQLiteColumnSpec
	rows    [][]any
}

func (i *SQLiteTableImporter) ReplaceTables(ctx context.Context, payloads []tablePayload) ([]TablePreview, error) {
	if i == nil || i.db == nil || i.writer == nil {
		return nil, errors.New("backup import: nil sqlite database")
	}
	lockValue, _ := sqliteImportLocks.LoadOrStore(i.db, &sync.Mutex{})
	lock := lockValue.(*sync.Mutex)
	if !lock.TryLock() {
		return nil, ErrImportAlreadyInProgress
	}
	defer lock.Unlock()

	// Insert in foreign-key order regardless of the bundle's table order.
	position := make(map[string]int, len(dbexport.SQLiteExportOrder))
	for index, table := range dbexport.SQLiteExportOrder {
		position[table] = index
	}
	ordered := append([]tablePayload(nil), payloads...)
	for _, payload := range ordered {
		if _, ok := position[payload.name]; !ok {
			return nil, fmt.Errorf("unsupported backup table %q", payload.name)
		}
	}
	sort.SliceStable(ordered, func(a, b int) bool { return position[ordered[a].name] < position[ordered[b].name] })

	err := i.writer.Write(ctx, func(v sqlitedb.WriteValue) error {
		conn, txCtx := v.Conn(), v.Context()
		// Describe and coerce everything first so a bad bundle never deletes.
		plans := make([]sqliteTablePlan, 0, len(ordered))
		for _, payload := range ordered {
			if err := contextError(txCtx); err != nil {
				return err
			}
			columns, err := dbexport.DescribeSQLiteColumns(txCtx, conn, payload.name)
			if err != nil {
				return fmt.Errorf("read columns for %q: %w", payload.name, err)
			}
			if len(columns) == 0 {
				return fmt.Errorf("read columns for %q: table does not exist", payload.name)
			}
			rows, err := coerceSQLiteRows(payload.name, columns, payload.rows)
			if err != nil {
				return fmt.Errorf("coerce table %q: %w", payload.name, err)
			}
			plans = append(plans, sqliteTablePlan{table: payload.name, columns: columns, rows: rows})
		}
		for _, entry := range sqliteDependentDeletes {
			if err := sqliteDeleteTable(txCtx, conn, entry.table, entry.where); err != nil {
				return err
			}
		}
		for index := len(plans) - 1; index >= 0; index-- {
			if err := sqliteDeleteTable(txCtx, conn, plans[index].table, ""); err != nil {
				return err
			}
		}
		for _, plan := range plans {
			if err := sqliteInsertTable(txCtx, conn, plan); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return previewTables(payloads), nil
}

func sqliteDeleteTable(ctx context.Context, conn *sql.Conn, table, where string) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	statement := "DELETE FROM " + quoteTableIdentifier(table)
	if where != "" {
		statement += " WHERE " + where
	}
	if _, err := conn.ExecContext(ctx, statement); err != nil {
		return fmt.Errorf("truncate table %q: %w", table, err)
	}
	return nil
}

func sqliteInsertTable(ctx context.Context, conn *sql.Conn, plan sqliteTablePlan) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	if len(plan.rows) == 0 {
		return nil
	}
	names := make([]string, len(plan.columns))
	marks := make([]string, len(plan.columns))
	for index, column := range plan.columns {
		names[index] = quoteTableIdentifier(column.Name)
		marks[index] = "?"
	}
	statement, err := conn.PrepareContext(ctx, "INSERT INTO "+quoteTableIdentifier(plan.table)+" ("+strings.Join(names, ",")+") VALUES ("+strings.Join(marks, ",")+")")
	if err != nil {
		return fmt.Errorf("insert table %q: %w", plan.table, err)
	}
	defer statement.Close()
	for _, row := range plan.rows {
		if err := contextError(ctx); err != nil {
			return err
		}
		if _, err := statement.ExecContext(ctx, row...); err != nil {
			return fmt.Errorf("insert table %q: %w", plan.table, err)
		}
	}
	return nil
}

func coerceSQLiteRows(table string, columns []dbexport.SQLiteColumnSpec, source []map[string]any) ([][]any, error) {
	known := make(map[string]struct{}, len(columns))
	for _, column := range columns {
		known[column.Name] = struct{}{}
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
			coerced, err := coerceSQLiteValue(column, sourceRow[column.Name])
			if err != nil {
				return nil, fmt.Errorf("column %q in row %d: %w", column.Name, rowIndex, err)
			}
			row[columnIndex] = coerced
		}
		result[rowIndex] = row
	}
	return result, nil
}

var compactUUID = regexp.MustCompile(`^[0-9a-fA-F]{32}$`)

// coerceSQLiteValue reverses the JSON representation produced by the SQLite
// and PostgreSQL exporters into the SQLite storage form of one column. A
// missing value is NULL; a NOT NULL violation is reported by SQLite itself,
// like PostgreSQL does for COPY.
func coerceSQLiteValue(column dbexport.SQLiteColumnSpec, value any) (any, error) {
	if value == nil {
		return nil, nil
	}
	switch column.Kind {
	case dbexport.SQLiteKindText:
		var text string
		switch value := value.(type) {
		case string:
			text = value
		case json.Number:
			text = value.String()
		default:
			return nil, fmt.Errorf("text value has Go type %T", value)
		}
		if column.Name == "id" || strings.HasSuffix(column.Name, "_id") {
			// UUID columns are stored lower-case with hyphens; PostgreSQL-era
			// bundles may carry any case or the compact form. Non-UUID ids
			// ("singleton", "ca", ...) are kept verbatim.
			if compact := strings.ReplaceAll(text, "-", ""); compactUUID.MatchString(compact) {
				compact = strings.ToLower(compact)
				return compact[0:8] + "-" + compact[8:12] + "-" + compact[12:16] + "-" + compact[16:20] + "-" + compact[20:], nil
			}
		}
		return text, nil
	case dbexport.SQLiteKindInt:
		return parseSQLiteInt(value)
	case dbexport.SQLiteKindBool:
		parsed, err := coerceImportBool(value)
		if err != nil {
			return nil, err
		}
		if parsed {
			return int64(1), nil
		}
		return int64(0), nil
	case dbexport.SQLiteKindTimestamp:
		return coerceSQLiteTimestamp(value)
	case dbexport.SQLiteKindJSON:
		encoded, err := coerceImportJSON(value)
		if err != nil {
			return nil, err
		}
		return string(encoded), nil
	case dbexport.SQLiteKindBlob:
		decoded, err := coerceImportBytea(value)
		if err != nil {
			return nil, err
		}
		if decoded == nil {
			decoded = []byte{}
		}
		return decoded, nil
	}
	return nil, &dbexport.UnsupportedSQLiteTypeError{Column: column.Name, TypeName: column.Declared}
}

func coerceSQLiteTimestamp(value any) (any, error) {
	encoded, ok := value.(string)
	if !ok {
		return nil, fmt.Errorf("timestamp value has Go type %T", value)
	}
	var stamp sqlitedb.Timestamp
	switch encoded {
	case "infinity":
		stamp = sqlitedb.PositiveInfinity{}
	case "-infinity":
		stamp = sqlitedb.NegativeInfinity{}
	default:
		parsed, err := parseImportTimestamp(encoded)
		if err != nil {
			return nil, err
		}
		stamp = sqlitedb.FiniteTimestamp{Seconds: parsed.Unix(), Nanos: int32(parsed.Nanosecond())}
	}
	key, err := sqlitedb.EncodeTimestamp(stamp)
	if err != nil || key == nil {
		return nil, fmt.Errorf("encode timestamp %q: %v", encoded, err)
	}
	return *key, nil
}

// parseSQLiteInt keeps integers exact: json.Number and the decimal strings the
// exporter emits above 2^53 never pass through float64 unless they are written
// in exponent form.
func parseSQLiteInt(value any) (int64, error) {
	var text string
	switch value := value.(type) {
	case json.Number:
		text = value.String()
	case string:
		text = value
	case int64:
		return value, nil
	case int:
		return int64(value), nil
	case float64:
		if math.Trunc(value) != value || math.Abs(value) > 1<<53 {
			return 0, fmt.Errorf("integer value %v is out of range", value)
		}
		return int64(value), nil
	default:
		return 0, fmt.Errorf("integer value has Go type %T", value)
	}
	if parsed, err := strconv.ParseInt(text, 10, 64); err == nil {
		return parsed, nil
	}
	if parsed, err := strconv.ParseFloat(text, 64); err == nil && math.Trunc(parsed) == parsed && math.Abs(parsed) <= 1<<53 {
		return int64(parsed), nil
	}
	return 0, fmt.Errorf("parse integer value %q", text)
}
