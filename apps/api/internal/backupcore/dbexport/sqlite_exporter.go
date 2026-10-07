package dbexport

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

// UnsupportedSQLiteTypeError prevents a column or value from silently
// disappearing from an export (the SQLite counterpart of UnsupportedTypeError).
type UnsupportedSQLiteTypeError struct {
	Table    string
	Column   string
	TypeName string
}

func (e *UnsupportedSQLiteTypeError) Error() string {
	if e == nil {
		return "unsupported SQLite type"
	}
	return fmt.Sprintf("unsupported SQLite type %q in column %s.%s", e.TypeName, e.Table, e.Column)
}

type sqliteKind int

const (
	kindText sqliteKind = iota
	kindInt
	kindBool
	kindTimestamp
	kindJSON
	kindBlob
)

// sqliteSpecialColumns lists the columns whose JSON representation cannot be
// derived from the declared SQLite type alone (STRICT tables only expose TEXT
// and INTEGER). A schema test keeps this map aligned with the CHECK constraints
// (`IN (0,1)`, `json_valid`, tagged clock keys) of the migrated tables.
var sqliteSpecialColumns = map[string]map[string]sqliteKind{
	"installation_settings":        {"default_resolver_pool": kindJSON, "forwarding_rules": kindJSON, "enrollment_hostnames": kindJSON, "created_at": kindTimestamp, "updated_at": kindTimestamp},
	"users":                        {"active": kindBool, "password_change_required": kindBool, "created_at": kindTimestamp, "updated_at": kindTimestamp},
	"secrets":                      {"created_at": kindTimestamp, "updated_at": kindTimestamp},
	"resolver_pools":               {"endpoints": kindJSON, "is_default": kindBool, "created_at": kindTimestamp, "updated_at": kindTimestamp},
	"zones":                        {"enabled": kindBool, "created_at": kindTimestamp, "updated_at": kindTimestamp},
	"dns_records":                  {"value": kindJSON, "enabled": kindBool, "proxied": kindBool, "proxy_settings": kindJSON, "created_at": kindTimestamp, "updated_at": kindTimestamp},
	"forwarding_rules":             {"created_at": kindTimestamp, "updated_at": kindTimestamp},
	"stream_routes":                {"enabled": kindBool, "upstream": kindJSON, "created_at": kindTimestamp, "updated_at": kindTimestamp},
	"internal_ca":                  {"created_at": kindTimestamp, "updated_at": kindTimestamp},
	"internal_ca_enrollment_state": {"established_at": kindTimestamp},
	"certificates":                 {"hostnames": kindJSON, "expires_at": kindTimestamp, "renew_after": kindTimestamp, "created_at": kindTimestamp, "updated_at": kindTimestamp},
	"provider_connections":         {"enabled": kindBool, "created_at": kindTimestamp, "updated_at": kindTimestamp},
	"config_revisions":             {"snapshot": kindJSON, "created_at": kindTimestamp, "applied_at": kindTimestamp},
	"installation_identity":        {"updated_at": kindTimestamp},
	"cluster_keys":                 {"created_at": kindTimestamp, "retired_at": kindTimestamp},
	"node_state":                   {"enrolled_at": kindTimestamp, "last_seen_at": kindTimestamp, "last_attempt_at": kindTimestamp, "last_success_at": kindTimestamp, "next_attempt_at": kindTimestamp, "sync_enabled": kindBool, "updated_at": kindTimestamp},
}

type sqliteColumn struct {
	name     string
	declared string
	kind     sqliteKind
	notNull  bool
}

type sqliteQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// SQLiteQuerier is satisfied by *sql.DB, *sql.Tx and *sql.Conn.
type SQLiteQuerier = sqliteQuerier

// SQLiteKind is the JSON kind of a SQLite column in the bundle.
type SQLiteKind = sqliteKind

// Exported column kinds, shared with the SQLite importer so both directions
// use the same schema-and-special-column classification.
const (
	SQLiteKindText      = kindText
	SQLiteKindInt       = kindInt
	SQLiteKindBool      = kindBool
	SQLiteKindTimestamp = kindTimestamp
	SQLiteKindJSON      = kindJSON
	SQLiteKindBlob      = kindBlob
)

// SQLiteColumnSpec describes one column of an exported table.
type SQLiteColumnSpec struct {
	Name     string
	Declared string
	Kind     SQLiteKind
	NotNull  bool
}

// DescribeSQLiteColumns classifies the columns of table exactly as the
// exporter does. Unknown declared types yield *UnsupportedSQLiteTypeError.
func DescribeSQLiteColumns(ctx context.Context, q SQLiteQuerier, table string) ([]SQLiteColumnSpec, error) {
	columns, err := sqliteTableColumns(ctx, q, table)
	if err != nil {
		return nil, err
	}
	specs := make([]SQLiteColumnSpec, len(columns))
	for index, column := range columns {
		specs[index] = SQLiteColumnSpec{Name: column.name, Declared: column.declared, Kind: column.kind, NotNull: column.notNull}
	}
	return specs, nil
}

// resolveSQLiteKind maps a declared SQLite type plus the table-specific
// knowledge above to the JSON kind used by the PostgreSQL exporter.
func resolveSQLiteKind(table, column, declared string) (sqliteKind, error) {
	unsupported := &UnsupportedSQLiteTypeError{Table: table, Column: column, TypeName: declared}
	declaredUpper := strings.ToUpper(strings.TrimSpace(declared))
	if special, ok := sqliteSpecialColumns[table][column]; ok {
		switch {
		case special == kindBool && declaredUpper == "INTEGER":
			return special, nil
		case (special == kindTimestamp || special == kindJSON) && declaredUpper == "TEXT":
			return special, nil
		}
		return 0, unsupported
	}
	switch declaredUpper {
	case "TEXT":
		return kindText, nil
	case "INTEGER":
		return kindInt, nil
	case "BLOB":
		return kindBlob, nil
	}
	return 0, unsupported
}

func sqliteTableColumns(ctx context.Context, q sqliteQuerier, table string) ([]sqliteColumn, error) {
	rows, err := q.QueryContext(ctx, "PRAGMA table_info("+quoteIdentifier(table)+")")
	if err != nil {
		return nil, fmt.Errorf("describe %q: %w", table, err)
	}
	defer rows.Close()
	columns := make([]sqliteColumn, 0)
	for rows.Next() {
		var cid, notNull, pk int
		var name, declared string
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &declared, &notNull, &defaultValue, &pk); err != nil {
			return nil, fmt.Errorf("scan column of %q: %w", table, err)
		}
		kind, err := resolveSQLiteKind(table, name, declared)
		if err != nil {
			return nil, err
		}
		columns = append(columns, sqliteColumn{name: name, declared: declared, kind: kind, notNull: notNull != 0})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("describe %q: %w", table, err)
	}
	return columns, nil
}

// encodeSQLiteValue converts one SQLite value to the exact JSON value the
// PostgreSQL exporter emits for the corresponding PostgreSQL type.
func encodeSQLiteValue(table string, column sqliteColumn, value any) (any, error) {
	if value == nil {
		return nil, nil
	}
	unsupported := func() error {
		return &UnsupportedSQLiteTypeError{Table: table, Column: column.name, TypeName: fmt.Sprintf("%s value %T", column.declared, value)}
	}
	switch column.kind {
	case kindText:
		switch value := value.(type) {
		case string:
			return value, nil
		case []byte:
			return string(value), nil
		}
	case kindInt:
		if value, ok := value.(int64); ok {
			return BigintAboveFloat64Safe(value), nil
		}
	case kindBool:
		if value, ok := value.(int64); ok {
			if value != 0 && value != 1 {
				return nil, fmt.Errorf("column %s.%s: boolean value %d is not 0 or 1", table, column.name, value)
			}
			return value == 1, nil
		}
	case kindTimestamp:
		key, ok := value.(string)
		if !ok {
			break
		}
		decoded, err := sqlitedb.DecodeTimestamp(&key)
		if err != nil {
			return nil, fmt.Errorf("column %s.%s: %w", table, column.name, err)
		}
		switch decoded := decoded.(type) {
		case sqlitedb.NegativeInfinity:
			return "-infinity", nil
		case sqlitedb.PositiveInfinity:
			return "infinity", nil
		case sqlitedb.FiniteTimestamp:
			return time.Unix(decoded.Seconds, int64(decoded.Nanos)).UTC().Format(time.RFC3339Nano), nil
		}
		return nil, nil
	case kindJSON:
		var text []byte
		switch value := value.(type) {
		case string:
			text = []byte(value)
		case []byte:
			text = value
		default:
			return nil, unsupported()
		}
		// UseNumber keeps numbers lossless; re-marshaling sorts object keys as
		// the PostgreSQL exporter does, so the bundle bytes are deterministic.
		decoder := json.NewDecoder(bytes.NewReader(text))
		decoder.UseNumber()
		var decoded any
		if err := decoder.Decode(&decoded); err != nil {
			return nil, fmt.Errorf("column %s.%s: decode JSON: %w", table, column.name, err)
		}
		if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("column %s.%s: trailing JSON data", table, column.name)
		}
		return decoded, nil
	case kindBlob:
		if value, ok := value.([]byte); ok {
			if len(value) == 0 {
				return "", nil
			}
			return base64.RawURLEncoding.EncodeToString(value), nil
		}
	}
	return nil, unsupported()
}

// SQLiteExporter streams the configuration tables of a SQLite database into
// the same per-table JSON bundle entries as Exporter.
type SQLiteExporter struct {
	db              *sql.DB
	tables          []string
	now             func() time.Time
	masterKeyBase64 string
}

// NewSQLite builds an exporter reading from db. opts.Tables may select a
// subset of DefaultConfigTables; entries always follow SQLiteExportOrder.
func NewSQLite(db *sql.DB, opts ExporterOptions) *SQLiteExporter {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &SQLiteExporter{
		db:              db,
		tables:          append([]string(nil), opts.Tables...),
		now:             now,
		masterKeyBase64: opts.MasterKeyBase64,
	}
}

func (e *SQLiteExporter) orderedTables() ([]string, error) {
	if len(e.tables) == 0 {
		return append([]string(nil), SQLiteExportOrder...), nil
	}
	wanted := make(map[string]bool, len(e.tables))
	for _, table := range e.tables {
		if !validIdentifier(table) {
			return nil, fmt.Errorf("invalid SQLite table identifier %q", table)
		}
		if wanted[table] {
			return nil, fmt.Errorf("duplicate SQLite table %q", table)
		}
		wanted[table] = true
	}
	ordered := make([]string, 0, len(wanted))
	for _, table := range SQLiteExportOrder {
		if wanted[table] {
			ordered = append(ordered, table)
			delete(wanted, table)
		}
	}
	if len(wanted) > 0 {
		return nil, fmt.Errorf("unsupported SQLite export table(s) %v", mapKeys(wanted))
	}
	return ordered, nil
}

func mapKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	return keys
}

// Export writes one db/<table>.json entry per table. All tables are read in a
// single read transaction, so the bundle is one consistent WAL snapshot.
func (e *SQLiteExporter) Export(ctx context.Context, w ZipWriter) (ExportReport, error) {
	report := ExportReport{Tables: make([]TableStats, 0)}
	if e == nil {
		return report, errors.New("export: nil exporter")
	}
	if e.db == nil {
		return report, errors.New("export: nil database")
	}
	if ctx == nil {
		return report, errors.New("export: nil context")
	}
	if w == nil {
		return report, errors.New("export: nil writer")
	}
	order, err := e.orderedTables()
	if err != nil {
		return report, fmt.Errorf("export order: %w", err)
	}
	tx, err := e.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return report, fmt.Errorf("begin read transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	// A deferred BEGIN only pins the WAL snapshot at its first read.
	var pinned int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema").Scan(&pinned); err != nil {
		return report, fmt.Errorf("pin snapshot: %w", err)
	}

	errs := make([]error, 0)
	for _, table := range order {
		started := e.now()
		rowCount, bytesWritten, exportErr := e.exportTable(ctx, tx, w, table)
		duration := e.now().Sub(started).Milliseconds()
		if duration < 0 {
			duration = 0
		}
		report.Tables = append(report.Tables, TableStats{Name: table, RowCount: rowCount, Bytes: bytesWritten, DurationMS: duration, Error: exportErr})
		if exportErr != nil {
			errs = append(errs, fmt.Errorf("table %s: %w", table, exportErr))
		}
	}
	return report, errors.Join(errs...)
}

func (e *SQLiteExporter) exportTable(ctx context.Context, tx *sql.Tx, w ZipWriter, table string) (int, int64, error) {
	var prefix map[string]any
	if table == "secrets" {
		var err error
		prefix, err = (&Exporter{now: e.now, masterKeyBase64: e.masterKeyBase64}).masterKeyMarker()
		if err != nil {
			return 0, 0, err
		}
	}
	columns, err := sqliteTableColumns(ctx, tx, table)
	if err != nil {
		return 0, 0, err
	}
	if len(columns) == 0 {
		return 0, 0, &MissingTablesError{Tables: []string{table}}
	}
	names := make([]string, len(columns))
	for index, column := range columns {
		names[index] = quoteIdentifier(column.name)
	}
	rows, err := tx.QueryContext(ctx, "SELECT "+strings.Join(names, ",")+" FROM "+quoteIdentifier(table)+" ORDER BY 1")
	if err != nil {
		return 0, 0, fmt.Errorf("query %q: %w", table, err)
	}

	var stream *countingReadCloser
	started := false
	done := make(chan error, 1)
	rowCount := 0
	var bytesWritten int64
	open := func() (io.ReadCloser, error) {
		if started {
			return nil, fmt.Errorf("table %q stream opened more than once", table)
		}
		started = true
		reader, writer := io.Pipe()
		stream = &countingReadCloser{Reader: reader, count: &bytesWritten}
		go func() {
			err := encodeSQLiteRows(ctx, table, columns, rows, writer, &rowCount, prefix)
			_ = rows.Close()
			if err != nil {
				_ = writer.CloseWithError(err)
			} else {
				_ = writer.Close()
			}
			done <- err
		}()
		return stream, nil
	}

	addErr := w.Add("db/"+table+".json", -1, open)
	if !started {
		_ = rows.Close()
		if addErr != nil {
			return 0, 0, addErr
		}
		return 0, 0, fmt.Errorf("writer did not open table %q", table)
	}
	if addErr != nil {
		_ = stream.Close()
	}
	streamErr := <-done
	if addErr != nil {
		return rowCount, bytesWritten, addErr
	}
	return rowCount, bytesWritten, streamErr
}

func encodeSQLiteRows(ctx context.Context, table string, columns []sqliteColumn, rows *sql.Rows, dst io.Writer, rowCount *int, prefix map[string]any) error {
	if _, err := io.WriteString(dst, "["); err != nil {
		return err
	}
	first := true
	writeRow := func(row map[string]any) error {
		encoded, err := json.Marshal(row)
		if err != nil {
			return fmt.Errorf("encode row JSON: %w", err)
		}
		if !first {
			if _, err := io.WriteString(dst, ","); err != nil {
				return err
			}
		}
		if _, err := dst.Write(encoded); err != nil {
			return err
		}
		first = false
		(*rowCount)++
		return nil
	}
	if prefix != nil {
		if err := writeRow(prefix); err != nil {
			return err
		}
	}

	values := make([]any, len(columns))
	targets := make([]any, len(columns))
	for index := range values {
		targets[index] = &values[index]
	}
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := rows.Scan(targets...); err != nil {
			return fmt.Errorf("read row values: %w", err)
		}
		row := make(map[string]any, len(columns))
		for index, column := range columns {
			encoded, err := encodeSQLiteValue(table, column, values[index])
			if err != nil {
				return err
			}
			row[column.name] = encoded
		}
		// A prior import may have persisted the reserved marker row. Emit only
		// the newly encrypted marker so the bundle holds exactly one.
		if prefix != nil && row["purpose"] == BackupMasterKeyMarkerPurpose {
			continue
		}
		if err := writeRow(row); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read rows: %w", err)
	}
	_, err := io.WriteString(dst, "]")
	return err
}
