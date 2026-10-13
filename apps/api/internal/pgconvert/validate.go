package pgconvert

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/secrets"
	"github.com/zerkc/ProxyCore/apps/api/internal/snapshot"
	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

// Check names one independent validation check.
type Check string

const (
	CheckTableRows        Check = "table_rows"
	CheckTableHash        Check = "table_hash"
	CheckForeignKeys      Check = "foreign_key_check"
	CheckIntegrity        Check = "integrity_check"
	CheckSchema           Check = "schema_version"
	CheckSecret           Check = "secret_ciphertext"
	CheckRevisionChecksum Check = "revision_checksum"
	CheckSnapshotHash     Check = "snapshot_content_hash"
	CheckSnapshotBody     Check = "snapshot_body"
)

// Failure is one failed validation finding. Detail never contains secret values.
type Failure struct {
	Check  Check
	Table  string
	Detail string
}

// ValidationError means the converted file cannot be trusted; every finding is
// listed. Any ValidationError means the conversion failed.
type ValidationError struct{ Failures []Failure }

func (e *ValidationError) Error() string {
	var parts []string
	for i, f := range e.Failures {
		if i == 5 {
			parts = append(parts, fmt.Sprintf("and %d more", len(e.Failures)-5))
			break
		}
		part := string(f.Check)
		if f.Table != "" {
			part += " " + f.Table
		}
		if f.Detail != "" {
			part += ": " + f.Detail
		}
		parts = append(parts, part)
	}
	return fmt.Sprintf("pgconvert: validation failed (%d findings): %s", len(e.Failures), strings.Join(parts, "; "))
}

// ValidationReport summarizes what the validator verified.
type ValidationReport struct {
	Tables            map[string]TableReport // source/destination counts and hashes, recomputed
	SchemaVersion     int                    // latest applied sqlitedb migration
	SecretsDecrypted  int                    // secrets.ciphertext + cluster_keys.wrapped_kek
	RevisionChecksums int                    // config_revisions.checksum recomputed
	SnapshotBlobs     int                    // applied_snapshots bodies compared, hash recomputed
	LegacySnapshots   int                    // applied_snapshots rows without a body (nothing to verify)
}

// encryptedColumns lists every column that holds ciphertext produced by
// secrets.EncryptSecret with the master key: secrets.ciphertext, and the
// cluster KEKs wrapped with the same key (cluster.SQLiteKeyStore).
var encryptedColumns = []struct{ table, column string }{
	{"secrets", "ciphertext"},
	{"cluster_keys", "wrapped_kek"},
}

// Validate independently re-verifies a converted SQLite file against the same
// PostgreSQL snapshot held by src, without trusting Convert's own in-transaction
// check. It never writes to src or to the destination's data. The returned
// error is a *ValidationError when findings exist; other errors mean the
// verification itself could not run. The report is returned alongside either.
func Validate(ctx context.Context, src *Source, destPath, masterKeyBase64 string) (*ValidationReport, error) {
	if _, err := os.Stat(destPath); err != nil {
		return nil, fmt.Errorf("pgconvert: converted file: %w", err)
	}
	plan, err := src.Plan(ctx)
	if err != nil {
		return nil, err
	}
	db, err := sqlitedb.Open(destPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	v := &validator{ctx: ctx, src: src, plan: plan, db: db, key: masterKeyBase64,
		report: &ValidationReport{Tables: map[string]TableReport{}}}
	steps := []func() error{v.tables, v.foreignKeys, v.integrity, v.schema, v.secrets, v.revisions, v.snapshots}
	for _, step := range steps {
		if err := step(); err != nil {
			return v.report, err
		}
	}
	if len(v.failures) > 0 {
		return v.report, &ValidationError{Failures: v.failures}
	}
	return v.report, nil
}

type validator struct {
	ctx      context.Context
	src      *Source
	plan     *Plan
	db       *sql.DB
	key      string
	report   *ValidationReport
	failures []Failure
}

func (v *validator) fail(check Check, table, format string, args ...any) {
	v.failures = append(v.failures, Failure{Check: check, Table: table, Detail: fmt.Sprintf(format, args...)})
}

func (v *validator) tables() error {
	for _, spec := range tables {
		srcRows, srcHash, err := HashPGTable(v.ctx, v.src, v.plan, spec.Name)
		if err != nil {
			return err
		}
		dstRows, dstHash, err := HashSQLiteTable(v.ctx, v.db, spec)
		if err != nil {
			return err
		}
		v.report.Tables[spec.Name] = TableReport{SourceRows: srcRows, DestRows: dstRows, Hash: srcHash, DestHash: dstHash}
		if srcRows != dstRows {
			v.fail(CheckTableRows, spec.Name, "source %d rows, destination %d rows", srcRows, dstRows)
		}
		if srcHash != dstHash {
			v.fail(CheckTableHash, spec.Name, "source %s, destination %s", srcHash, dstHash)
		}
	}
	return nil
}

func (v *validator) foreignKeys() error {
	rows, err := v.db.QueryContext(v.ctx, `pragma foreign_key_check`)
	if err != nil {
		return fmt.Errorf("pgconvert: foreign_key_check: %w", err)
	}
	defer rows.Close()
	counts := map[string]int{}
	for rows.Next() {
		var table, parent string
		var rowid, fkid sql.NullInt64
		if err := rows.Scan(&table, &rowid, &parent, &fkid); err != nil {
			return fmt.Errorf("pgconvert: foreign_key_check: %w", err)
		}
		counts[table]++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for table, n := range counts {
		v.fail(CheckForeignKeys, table, "%d violating rows", n)
	}
	return nil
}

func (v *validator) integrity() error {
	rows, err := v.db.QueryContext(v.ctx, `pragma integrity_check`)
	if err != nil {
		v.fail(CheckIntegrity, "", "%v", err)
		return nil
	}
	defer rows.Close()
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return err
		}
		if line != "ok" {
			v.fail(CheckIntegrity, "", "%s", line)
		}
	}
	return rows.Err()
}

type ledgerRow struct {
	Version        int
	Name, Checksum string
}

type schemaFingerprint struct {
	Ledger  []ledgerRow
	Objects [][3]string
}

func readFingerprint(ctx context.Context, db *sql.DB) (schemaFingerprint, error) {
	var fp schemaFingerprint
	rows, err := db.QueryContext(ctx, `select version, name, checksum from proxycore_schema_migrations order by version`)
	if err != nil {
		return fp, err
	}
	for rows.Next() {
		var r ledgerRow
		if err := rows.Scan(&r.Version, &r.Name, &r.Checksum); err != nil {
			rows.Close()
			return fp, err
		}
		fp.Ledger = append(fp.Ledger, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fp, err
	}
	objects, err := db.QueryContext(ctx, `select name, type, coalesce(sql, '') from main.sqlite_schema
		where substr(name, 1, 7) != 'sqlite_' and name != 'proxycore_schema_migrations' order by name`)
	if err != nil {
		return fp, err
	}
	defer objects.Close()
	for objects.Next() {
		var o [3]string
		if err := objects.Scan(&o[0], &o[1], &o[2]); err != nil {
			return fp, err
		}
		fp.Objects = append(fp.Objects, o)
	}
	return fp, objects.Err()
}

// schema compares the migration ledger (version, name, checksum) and every
// product table/index/trigger definition with a pristine database produced by
// the latest embedded sqlitedb migrations.
func (v *validator) schema() error {
	dir, err := os.MkdirTemp("", "pgconvert-schema-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	ref, err := sqlitedb.Open(filepath.Join(dir, "reference.db"))
	if err != nil {
		return err
	}
	defer ref.Close()
	if err := sqlitedb.Migrate(v.ctx, ref); err != nil {
		return fmt.Errorf("pgconvert: build reference schema: %w", err)
	}
	want, err := readFingerprint(v.ctx, ref)
	if err != nil {
		return err
	}
	got, err := readFingerprint(v.ctx, v.db)
	if err != nil {
		v.fail(CheckSchema, "", "read destination schema: %v", err)
		return nil
	}
	if n := len(got.Ledger); n > 0 {
		v.report.SchemaVersion = got.Ledger[n-1].Version
	}
	if !reflect.DeepEqual(want.Ledger, got.Ledger) {
		v.fail(CheckSchema, "", "migration ledger differs from the latest embedded migrations (expected version %d, found %d)",
			want.Ledger[len(want.Ledger)-1].Version, v.report.SchemaVersion)
	}
	if !reflect.DeepEqual(want.Objects, got.Objects) {
		v.fail(CheckSchema, "", "schema objects differ from the latest embedded migrations")
	}
	return nil
}

// readSource collects one source column by id, in destination form.
func (v *validator) readSource(table, idColumn, column string) (map[string]any, error) {
	spec, err := tableSpec(table)
	if err != nil {
		return nil, err
	}
	idIdx, colIdx := -1, -1
	for i, c := range spec.Columns {
		switch c.Name {
		case idColumn:
			idIdx = i
		case column:
			colIdx = i
		}
	}
	if idIdx < 0 || colIdx < 0 {
		return nil, fmt.Errorf("pgconvert: %s has no %s/%s mapping", table, idColumn, column)
	}
	out := map[string]any{}
	err = v.src.ReadTable(v.ctx, v.plan, table, func(row []any) error {
		id, _ := row[idIdx].(string)
		out[id] = row[colIdx]
		return nil
	})
	return out, err
}

// secrets verifies that every ciphertext is byte-identical to the source value
// and decrypts with the master key (AES-GCM authenticates the whole value).
func (v *validator) secrets() error {
	for _, col := range encryptedColumns {
		source, err := v.readSource(col.table, "id", col.column)
		if err != nil {
			return err
		}
		rows, err := v.db.QueryContext(v.ctx, `select id, `+quoteIdent(col.column)+` from `+quoteIdent(col.table))
		if err != nil {
			return err
		}
		seen := 0
		for rows.Next() {
			var id, value string
			if err := rows.Scan(&id, &value); err != nil {
				rows.Close()
				return err
			}
			seen++
			want, ok := source[id].(string)
			switch {
			case !ok:
				v.fail(CheckSecret, col.table, "row %s is not present in the source", id)
			case want != value:
				v.fail(CheckSecret, col.table, "row %s ciphertext differs from the source", id)
			default:
				if _, err := secrets.DecryptSecret(value, v.key); err != nil {
					v.fail(CheckSecret, col.table, "row %s does not decrypt with the master key", id)
				} else {
					v.report.SecretsDecrypted++
				}
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if seen != len(source) {
			v.fail(CheckSecret, col.table, "destination has %d ciphertext rows, source %d", seen, len(source))
		}
	}
	return nil
}

// revisions recomputes config_revisions.checksum from the stored snapshot.
func (v *validator) revisions() error {
	rows, err := v.db.QueryContext(v.ctx, `select id, checksum, snapshot from config_revisions`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, checksum, raw string
		if err := rows.Scan(&id, &checksum, &raw); err != nil {
			return err
		}
		var value any
		if err := json.Unmarshal([]byte(raw), &value); err != nil {
			v.fail(CheckRevisionChecksum, "config_revisions", "row %s snapshot is not valid JSON", id)
			continue
		}
		if domain.ChecksumSnapshot(value) != checksum {
			v.fail(CheckRevisionChecksum, "config_revisions", "row %s checksum does not match its snapshot", id)
			continue
		}
		v.report.RevisionChecksums++
	}
	return rows.Err()
}

// snapshots compares applied_snapshots.snapshot_body with the source BLOB and
// recomputes content_hash from the replication envelope it carries. Rows with a
// NULL body are legacy rows with nothing to verify.
func (v *validator) snapshots() error {
	source, err := v.readSource("applied_snapshots", "id", "snapshot_body")
	if err != nil {
		return err
	}
	rows, err := v.db.QueryContext(v.ctx, `select id, content_hash, snapshot_body from applied_snapshots`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, contentHash string
		var body any
		if err := rows.Scan(&id, &contentHash, &body); err != nil {
			return err
		}
		srcBody := source[id]
		switch got := body.(type) {
		case nil:
			if srcBody != nil {
				v.fail(CheckSnapshotBody, "applied_snapshots", "row %s lost its body", id)
			} else {
				v.report.LegacySnapshots++
			}
		case []byte:
			want, isBlob := srcBody.([]byte)
			if !isBlob || !bytes.Equal(want, got) {
				v.fail(CheckSnapshotBody, "applied_snapshots", "row %s body differs from the source", id)
			}
			if err := checkEnvelopeHash(got, contentHash); err != nil {
				v.fail(CheckSnapshotHash, "applied_snapshots", "row %s: %v", id, err)
			} else {
				v.report.SnapshotBlobs++
			}
		default:
			v.fail(CheckSnapshotBody, "applied_snapshots", "row %s body has type %T", id, body)
		}
	}
	return rows.Err()
}

func checkEnvelopeHash(body []byte, contentHash string) error {
	env, err := snapshot.Unmarshal(body)
	if err != nil {
		return errors.New("body is not a replication snapshot")
	}
	expected, err := env.ExpectedHash()
	if err != nil {
		return fmt.Errorf("body hash cannot be computed: %w", err)
	}
	if env.ContentHash != expected {
		return errors.New("body does not match its own declared content hash")
	}
	if contentHash != expected {
		return errors.New("content_hash column does not match the recomputed body hash")
	}
	return nil
}
