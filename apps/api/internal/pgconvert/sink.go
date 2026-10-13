package pgconvert

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

// ErrDestinationExists is returned, without touching anything, when the target
// SQLite path (or one of its sidecar files) already exists.
var ErrDestinationExists = errors.New("pgconvert: destination already exists")

// FidelityError means the destination read back differently from the source.
type FidelityError struct {
	Table                string
	SourceRows, DestRows int64
	SourceHash, DestHash string
}

func (e *FidelityError) Error() string {
	return fmt.Sprintf("pgconvert: %s differs after copy: source %d rows %s, destination %d rows %s",
		e.Table, e.SourceRows, e.SourceHash, e.DestRows, e.DestHash)
}

// TableReport carries per-table counts and canonical hashes for the validator.
type TableReport struct {
	SourceRows, DestRows int64
	Hash, DestHash       string // canonical digests (see RowHash/Digest)
}

// Warning is a non-fatal finding; copying never stops for these.
type Warning struct {
	Code, Table, Column string
	Count               int64
	Samples             []string
}

// Report is the result of a successful copy.
type Report struct {
	Tables   map[string]TableReport
	Warnings []Warning
	Gaps     []Gap // older-variant source columns replaced by defaults
}

// danglingRef is a reference whose PostgreSQL foreign key was dropped by
// Drizzle 0007, so orphan values may exist and are copied unchanged.
type danglingRef struct{ table, column, parent, parentColumn string }

var danglingRefs = []danglingRef{
	{"enrollment_tokens", "consumed_by_attempt_id", "enrollment_attempts", "id"},
	{"enrollment_grants", "attempt_id", "enrollment_attempts", "id"},
	{"enrolled_nodes", "created_by_attempt_id", "enrollment_attempts", "id"},
}

const maxWarningSamples = 5

// Convert copies the whole PostgreSQL snapshot into a new SQLite file at
// destPath, which must not exist. The schema comes from sqlitedb.Migrate, all
// rows are copied in foreign-key-safe order inside one transaction, and every
// table is read back and compared before commit. On any failure the files it
// created are removed. Renaming into place and independent validation belong
// to the caller.
func Convert(ctx context.Context, src *Source, destPath string) (report *Report, err error) {
	for _, path := range []string{destPath, destPath + "-wal", destPath + "-shm", destPath + "-journal"} {
		if _, statErr := os.Lstat(path); statErr == nil {
			return nil, fmt.Errorf("%w: %s", ErrDestinationExists, path)
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return nil, statErr
		}
	}
	plan, err := src.Plan(ctx)
	if err != nil {
		return nil, err
	}
	db, err := sqlitedb.Open(destPath)
	if err != nil {
		return nil, err
	}
	closed := false
	defer func() {
		if !closed {
			db.Close()
		}
		if err != nil {
			for _, path := range []string{destPath, destPath + "-wal", destPath + "-shm", destPath + "-journal"} {
				_ = os.Remove(path)
			}
		}
	}()
	if err = sqlitedb.Migrate(ctx, db); err != nil {
		return nil, fmt.Errorf("pgconvert: create destination schema: %w", err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	report = &Report{Tables: map[string]TableReport{}, Gaps: plan.Gaps()}
	for _, spec := range tables {
		result, copyErr := copyTable(ctx, src, plan, tx, spec)
		if copyErr != nil {
			return nil, copyErr
		}
		report.Tables[spec.Name] = result
	}
	if report.Warnings, err = danglingWarnings(ctx, tx); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, fmt.Errorf("pgconvert: commit destination: %w", err)
	}
	if _, err = db.ExecContext(ctx, `pragma wal_checkpoint(TRUNCATE)`); err != nil {
		return nil, fmt.Errorf("pgconvert: checkpoint destination: %w", err)
	}
	closed = true
	if err = db.Close(); err != nil {
		return nil, err
	}
	return report, nil
}

func copyTable(ctx context.Context, src *Source, plan *Plan, tx *sql.Tx, spec TableSpec) (TableReport, error) {
	names := make([]string, len(spec.Columns))
	marks := make([]string, len(spec.Columns))
	for i, c := range spec.Columns {
		names[i], marks[i] = quoteIdent(c.Name), "?"
	}
	insert, err := tx.PrepareContext(ctx, `insert into `+quoteIdent(spec.Name)+` (`+strings.Join(names, ", ")+`) values (`+strings.Join(marks, ", ")+`)`)
	if err != nil {
		return TableReport{}, fmt.Errorf("pgconvert: prepare %s: %w", spec.Name, err)
	}
	defer insert.Close()
	digest := NewDigest()
	err = src.ReadTable(ctx, plan, spec.Name, func(row []any) error {
		digest.Add(RowHash(spec, row))
		if _, execErr := insert.ExecContext(ctx, row...); execErr != nil {
			return fmt.Errorf("pgconvert: insert into %s: %w", spec.Name, execErr)
		}
		return nil
	})
	if err != nil {
		return TableReport{}, err
	}
	destRows, destHash, err := HashSQLiteTable(ctx, tx, spec)
	if err != nil {
		return TableReport{}, err
	}
	result := TableReport{SourceRows: digest.Rows(), DestRows: destRows, Hash: digest.Hex(), DestHash: destHash}
	if result.SourceRows != result.DestRows || result.Hash != result.DestHash {
		return TableReport{}, &FidelityError{Table: spec.Name, SourceRows: result.SourceRows, DestRows: destRows,
			SourceHash: result.Hash, DestHash: destHash}
	}
	return result, nil
}

func danglingWarnings(ctx context.Context, tx *sql.Tx) ([]Warning, error) {
	var warnings []Warning
	for _, ref := range danglingRefs {
		where := fmt.Sprintf(`from %[1]s c where c.%[2]s is not null and not exists (select 1 from %[3]s p where p.%[4]s = c.%[2]s)`,
			quoteIdent(ref.table), quoteIdent(ref.column), quoteIdent(ref.parent), quoteIdent(ref.parentColumn))
		var count int64
		if err := tx.QueryRowContext(ctx, `select count(*) `+where).Scan(&count); err != nil {
			return nil, fmt.Errorf("pgconvert: check %s.%s references: %w", ref.table, ref.column, err)
		}
		if count == 0 {
			continue
		}
		rows, err := tx.QueryContext(ctx, fmt.Sprintf(`select c.%s %s order by 1 limit %d`, quoteIdent(ref.column), where, maxWarningSamples))
		if err != nil {
			return nil, err
		}
		warning := Warning{Code: "dangling_reference", Table: ref.table, Column: ref.column, Count: count}
		for rows.Next() {
			var sample string
			if err := rows.Scan(&sample); err != nil {
				rows.Close()
				return nil, err
			}
			warning.Samples = append(warning.Samples, sample)
		}
		rows.Close()
		warnings = append(warnings, warning)
	}
	return warnings, nil
}
