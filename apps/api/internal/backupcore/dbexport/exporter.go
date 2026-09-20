package dbexport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ExporterOptions controls the database tables selected by an Exporter.
type ExporterOptions struct {
	Tables []string
	Now    func() time.Time
}

// Exporter streams configuration table rows into one JSON array per table.
type Exporter struct {
	pool   *pgxpool.Pool
	tables []string
	now    func() time.Time
}

func New(pool *pgxpool.Pool, opts ExporterOptions) *Exporter {
	tables := append([]string(nil), opts.Tables...)
	if len(tables) == 0 {
		tables = append([]string(nil), DefaultConfigTables...)
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Exporter{pool: pool, tables: tables, now: now}
}

func (e *Exporter) Export(ctx context.Context, w ZipWriter) (ExportReport, error) {
	report := ExportReport{Tables: make([]TableStats, 0)}
	if e == nil {
		return report, fmt.Errorf("export: nil exporter")
	}
	if e.pool == nil {
		return report, fmt.Errorf("export: nil pool")
	}
	if ctx == nil {
		return report, fmt.Errorf("export: nil context")
	}
	if w == nil {
		return report, fmt.Errorf("export: nil writer")
	}
	order, err := e.exportOrder(ctx)
	if err != nil {
		return report, fmt.Errorf("export order: %w", err)
	}

	if e.now == nil {
		e.now = time.Now
	}
	errs := make([]error, 0)
	for _, table := range order {
		started := e.now()
		rowCount, bytesWritten, exportErr := e.exportTable(ctx, w, table)
		finished := e.now()
		duration := finished.Sub(started).Milliseconds()
		if duration < 0 {
			duration = 0
		}
		stats := TableStats{
			Name:       table,
			RowCount:   rowCount,
			Bytes:      bytesWritten,
			DurationMS: duration,
			Error:      exportErr,
		}
		report.Tables = append(report.Tables, stats)
		if exportErr != nil {
			errs = append(errs, fmt.Errorf("table %s: %w", table, exportErr))
		}
	}
	if len(errs) > 0 {
		return report, errors.Join(errs...)
	}
	return report, nil
}

func (e *Exporter) exportTable(ctx context.Context, w ZipWriter, table string) (int, int64, error) {
	conn, err := e.pool.Acquire(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("acquire connection: %w", err)
	}
	defer conn.Release()

	if err := registerTableTypes(ctx, conn.Conn(), table); err != nil {
		return 0, 0, fmt.Errorf("register types for %q: %w", table, err)
	}
	rows, err := conn.Query(ctx, "select * from "+quoteIdentifier(table)+" order by 1")
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
			err := encodeRows(ctx, rows, writer, &rowCount)
			rows.Close()
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
		rows.Close()
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
	if streamErr != nil {
		return rowCount, bytesWritten, streamErr
	}
	return rowCount, bytesWritten, nil
}

func encodeRows(ctx context.Context, rows pgx.Rows, dst io.Writer, rowCount *int) error {
	if _, err := io.WriteString(dst, "["); err != nil {
		return err
	}
	first := true
	fields := rows.FieldDescriptions()
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		raw := rows.RawValues()
		values, err := rows.Values()
		if err != nil {
			return fmt.Errorf("read row values: %w", err)
		}
		row, err := coerceRow(fields, values, raw, rows)
		if err != nil {
			return err
		}
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
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read rows: %w", err)
	}
	_, err := io.WriteString(dst, "]")
	return err
}

// countingReadCloser records the bytes consumed by the ZipWriter without
// buffering the entry or requiring a second pass over the database rows.
type countingReadCloser struct {
	io.Reader
	closer io.Closer
	count  *int64
}

func (r *countingReadCloser) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	*r.count += int64(n)
	return n, err
}

func (r *countingReadCloser) Close() error {
	if r.closer != nil {
		return r.closer.Close()
	}
	if closer, ok := r.Reader.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

func registerTableTypes(ctx context.Context, conn *pgx.Conn, table string) error {
	if conn == nil {
		return fmt.Errorf("nil connection")
	}
	rows, err := conn.Query(ctx, `
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
