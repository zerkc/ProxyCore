package pgconvert

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

// Source reads one consistent PostgreSQL snapshot. It owns a single connection
// whose only transaction is REPEATABLE READ READ ONLY; nothing is ever written.
type Source struct {
	conn *pgx.Conn
	tx   pgx.Tx
}

// OpenSource connects and fixes the snapshot before any table is read.
func OpenSource(ctx context.Context, connString string) (*Source, error) {
	cfg, err := pgx.ParseConfig(connString)
	if err != nil {
		return nil, fmt.Errorf("pgconvert: parse PostgreSQL URL: %w", err)
	}
	cfg.RuntimeParams["default_transaction_read_only"] = "on"
	cfg.RuntimeParams["application_name"] = "proxycore-convert"
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("pgconvert: connect to PostgreSQL: %w", err)
	}
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		conn.Close(ctx)
		return nil, fmt.Errorf("pgconvert: begin read-only snapshot: %w", err)
	}
	// REPEATABLE READ takes its snapshot at the first statement, not at BEGIN.
	if _, err := tx.Exec(ctx, `select txid_current_snapshot()`); err != nil {
		_ = tx.Rollback(ctx)
		conn.Close(ctx)
		return nil, fmt.Errorf("pgconvert: fix snapshot: %w", err)
	}
	return &Source{conn: conn, tx: tx}, nil
}

// Close ends the snapshot without committing anything.
func (s *Source) Close(ctx context.Context) error {
	rollbackErr := s.tx.Rollback(ctx)
	closeErr := s.conn.Close(ctx)
	if rollbackErr != nil {
		return rollbackErr
	}
	return closeErr
}

// Gap is a documented older-variant column absent from the source; the
// destination receives the column's default instead.
type Gap struct{ Table, Column string }

// TablePlan binds a table mapping to the columns actually present in PostgreSQL.
type TablePlan struct {
	Spec    TableSpec
	Present []bool
}

// Plan is the validated mapping between the source schema and the destination.
type Plan struct {
	tables map[string]*TablePlan
	gaps   []Gap
}

// Gaps lists tolerated missing source columns in table/column order.
func (p *Plan) Gaps() []Gap { return append([]Gap(nil), p.gaps...) }

type pgColumn struct{ name, dataType, udt string }

// Plan introspects information_schema and fails closed on any table, column,
// type or enum label the mapping does not explicitly know.
func (s *Source) Plan(ctx context.Context) (*Plan, error) {
	tableNames, err := s.stringColumn(ctx, `select table_name::text from information_schema.tables
		where table_schema = current_schema() and table_type = 'BASE TABLE' order by 1`)
	if err != nil {
		return nil, err
	}
	known := map[string]bool{}
	for _, spec := range tables {
		known[spec.Name] = true
	}
	present := map[string]bool{}
	for _, name := range tableNames {
		present[name] = true
		if !known[name] {
			return nil, &SchemaError{Kind: ErrUnknownTable, Table: name}
		}
	}
	rows, err := s.tx.Query(ctx, `select table_name::text, column_name::text, data_type::text, udt_name::text
		from information_schema.columns where table_schema = current_schema() order by table_name, ordinal_position`)
	if err != nil {
		return nil, fmt.Errorf("pgconvert: introspect columns: %w", err)
	}
	columns := map[string][]pgColumn{}
	for rows.Next() {
		var table string
		var c pgColumn
		if err := rows.Scan(&table, &c.name, &c.dataType, &c.udt); err != nil {
			rows.Close()
			return nil, err
		}
		if present[table] {
			columns[table] = append(columns[table], c)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	labels, err := s.enumLabels(ctx)
	if err != nil {
		return nil, err
	}
	plan := &Plan{tables: map[string]*TablePlan{}}
	for _, spec := range tables {
		if !present[spec.Name] {
			return nil, &SchemaError{Kind: ErrMissingTable, Table: spec.Name}
		}
		byName := map[string]pgColumn{}
		specColumns := map[string]bool{}
		for _, c := range spec.Columns {
			specColumns[c.Name] = true
		}
		for _, c := range columns[spec.Name] {
			byName[c.name] = c
			if !specColumns[c.name] {
				return nil, &SchemaError{Kind: ErrUnknownColumn, Table: spec.Name, Column: c.name}
			}
		}
		tp := &TablePlan{Spec: spec, Present: make([]bool, len(spec.Columns))}
		for i, c := range spec.Columns {
			got, ok := byName[c.Name]
			if !ok {
				if !c.Older {
					return nil, &SchemaError{Kind: ErrMissingColumn, Table: spec.Name, Column: c.Name}
				}
				plan.gaps = append(plan.gaps, Gap{spec.Name, c.Name})
				continue
			}
			if err := checkColumn(spec.Name, c, got, labels); err != nil {
				return nil, err
			}
			tp.Present[i] = true
		}
		plan.tables[spec.Name] = tp
	}
	return plan, nil
}

func checkColumn(table string, c ColumnSpec, got pgColumn, labels map[string][]string) error {
	mismatch := func(want string) error {
		return &SchemaError{Kind: ErrTypeMismatch, Table: table, Column: c.Name,
			Detail: fmt.Sprintf("source type %s/%s, expected %s", got.dataType, got.udt, want)}
	}
	switch c.Kind {
	case KindUUID:
		if got.dataType != "uuid" {
			return mismatch("uuid")
		}
	case KindText:
		if got.dataType != "text" {
			return mismatch("text")
		}
	case KindJSON:
		if got.dataType != "jsonb" {
			return mismatch("jsonb")
		}
	case KindInt:
		if got.dataType != "integer" && got.dataType != "bigint" {
			return mismatch("integer or bigint")
		}
	case KindBool:
		if got.dataType != "boolean" {
			return mismatch("boolean")
		}
	case KindTimestamp:
		if got.dataType != "timestamp with time zone" {
			return mismatch("timestamp with time zone")
		}
	case KindBytes:
		if got.dataType != "bytea" {
			return mismatch("bytea")
		}
	case KindEnum:
		if got.dataType != "USER-DEFINED" || got.udt != c.EnumType {
			return mismatch("enum " + c.EnumType)
		}
		allowed := map[string]bool{}
		for _, v := range c.EnumValues {
			allowed[v] = true
		}
		for _, label := range labels[c.EnumType] {
			if !allowed[label] {
				return &SchemaError{Kind: ErrUnknownEnumValue, Table: table, Column: c.Name,
					Detail: fmt.Sprintf("enum %s has unmapped label %q", c.EnumType, label)}
			}
		}
	}
	return nil
}

func (s *Source) stringColumn(ctx context.Context, sql string) ([]string, error) {
	rows, err := s.tx.Query(ctx, sql)
	if err != nil {
		return nil, fmt.Errorf("pgconvert: introspect: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *Source) enumLabels(ctx context.Context) (map[string][]string, error) {
	rows, err := s.tx.Query(ctx, `select t.typname::text, e.enumlabel::text from pg_enum e
		join pg_type t on t.oid = e.enumtypid order by t.typname, e.enumsortorder`)
	if err != nil {
		return nil, fmt.Errorf("pgconvert: introspect enums: %w", err)
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var typ, label string
		if err := rows.Scan(&typ, &label); err != nil {
			return nil, err
		}
		out[typ] = append(out[typ], label)
	}
	return out, rows.Err()
}

// CountRows returns the table's row count inside the source snapshot.
func (s *Source) CountRows(ctx context.Context, table string) (int64, error) {
	if _, err := tableSpec(table); err != nil {
		return 0, err
	}
	var n int64
	if err := s.tx.QueryRow(ctx, `select count(*) from `+quoteIdent(table)).Scan(&n); err != nil {
		return 0, fmt.Errorf("pgconvert: count %s: %w", table, err)
	}
	return n, nil
}

// ReadTable streams every row of table as destination-ready values in spec
// column order: nil, string, int64 or []byte. Timestamps are already tagged,
// booleans are 0/1, jsonb is the raw ::text, and tolerated-missing columns
// carry their default. The callback owns the slice.
func (s *Source) ReadTable(ctx context.Context, plan *Plan, table string, fn func(row []any) error) error {
	tp, ok := plan.tables[table]
	if !ok {
		return fmt.Errorf("pgconvert: table %q is not in the plan", table)
	}
	var exprs []string
	for i, c := range tp.Spec.Columns {
		if tp.Present[i] {
			exprs = append(exprs, selectExpr(c))
		}
	}
	rows, err := s.tx.Query(ctx, `select `+strings.Join(exprs, ", ")+` from `+quoteIdent(table))
	if err != nil {
		return fmt.Errorf("pgconvert: read %s: %w", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		targets := make([]any, 0, len(exprs))
		for i, c := range tp.Spec.Columns {
			if tp.Present[i] {
				targets = append(targets, newTarget(c))
			}
		}
		if err := rows.Scan(targets...); err != nil {
			return fmt.Errorf("pgconvert: scan %s: %w", table, err)
		}
		row := make([]any, len(tp.Spec.Columns))
		next := 0
		for i, c := range tp.Spec.Columns {
			if !tp.Present[i] {
				row[i] = c.Default
				continue
			}
			value, err := convertValue(table, c, targets[next])
			if err != nil {
				return err
			}
			next++
			row[i] = value
		}
		if err := fn(row); err != nil {
			return err
		}
	}
	return rows.Err()
}

func quoteIdent(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }

func selectExpr(c ColumnSpec) string {
	q := quoteIdent(c.Name)
	switch c.Kind {
	case KindUUID, KindText, KindJSON, KindEnum:
		return q + `::text`
	case KindInt:
		return q + `::bigint`
	default:
		return q
	}
}

func newTarget(c ColumnSpec) any {
	switch c.Kind {
	case KindUUID, KindText, KindJSON, KindEnum:
		return new(*string)
	case KindInt:
		return new(*int64)
	case KindBool:
		return new(*bool)
	case KindTimestamp:
		return new(pgtype.Timestamptz)
	default:
		return new([]byte)
	}
}

func convertValue(table string, c ColumnSpec, target any) (any, error) {
	null := func() (any, error) {
		if !c.Nullable {
			return nil, &DataError{Table: table, Column: c.Name, Detail: "NULL in a NOT NULL destination column"}
		}
		return nil, nil
	}
	switch v := target.(type) {
	case **string:
		if *v == nil {
			return null()
		}
		if !utf8.ValidString(**v) {
			return nil, &DataError{Table: table, Column: c.Name, Detail: "text is not valid UTF-8"}
		}
		return **v, nil
	case **int64:
		if *v == nil {
			return null()
		}
		return **v, nil
	case **bool:
		if *v == nil {
			return null()
		}
		if **v {
			return int64(1), nil
		}
		return int64(0), nil
	case *pgtype.Timestamptz:
		if !v.Valid {
			return null()
		}
		var stamp sqlitedb.Timestamp
		switch v.InfinityModifier {
		case pgtype.Infinity:
			stamp = sqlitedb.PositiveInfinity{}
		case pgtype.NegativeInfinity:
			stamp = sqlitedb.NegativeInfinity{}
		default:
			stamp = sqlitedb.FiniteTimestamp{Seconds: v.Time.Unix(), Nanos: int32(v.Time.Nanosecond())}
		}
		key, err := sqlitedb.EncodeTimestamp(stamp)
		if err != nil {
			return nil, &DataError{Table: table, Column: c.Name, Detail: err.Error()}
		}
		return *key, nil
	case *[]byte:
		if *v == nil {
			return null()
		}
		return *v, nil
	}
	return nil, fmt.Errorf("pgconvert: unsupported scan target %T", target)
}
