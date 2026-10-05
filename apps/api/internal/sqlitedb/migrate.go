package sqlitedb

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// Embed the directory so future registry entries and SQL ship together.
//go:embed migrations
var migrationAssets embed.FS

const ledgerDDL = "CREATE TABLE proxycore_schema_migrations(version INTEGER NOT NULL PRIMARY KEY,name TEXT NOT NULL,checksum TEXT NOT NULL,applied_at TEXT COLLATE BINARY NOT NULL)"

type migrationEntry struct {
	Version  int    `json:"version"`
	Name     string `json:"name"`
	File     string `json:"file"`
	Checksum string `json:"checksum"`
	SQL      string `json:"-"`
}

var migrationFile = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*\.sql$`)
var migrationName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 _.-]{0,127}$`)
var migrationHash = regexp.MustCompile(`^[0-9a-f]{64}$`)
var migrationTokens = regexp.MustCompile(`'([^']|'')*'|"([^"]|"")*"|\[[^\]]*\]|--[^\r\n]*|(?s:/\*.*?\*/)|` + "`[^`]*`" + `|[A-Za-z_][A-Za-z0-9_]*`)

// Migrate uses only the embedded authority. No connection or runtime is rewired.
func Migrate(ctx context.Context, db *sql.DB) error {
	raw, err := migrationAssets.ReadFile("migrations/registry.json")
	if err != nil {
		return err
	}
	return migrateAuthority(ctx, db, raw, func(file string) ([]byte, error) {
		return migrationAssets.ReadFile("migrations/" + file)
	})
}

func validateMigrations(raw []byte, read func(string) ([]byte, error)) ([]migrationEntry, error) {
	var objects []json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(&objects); err != nil {
		return nil, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF || objects == nil {
		return nil, fmt.Errorf("registry must be exactly one JSON array")
	}
	entries := make([]migrationEntry, len(objects))
	seen := map[string]bool{}
	for i, object := range objects {
		e := &entries[i]
		members := json.NewDecoder(bytes.NewReader(object))
		opening, err := members.Token()
		if err != nil || opening != json.Delim('{') {
			return nil, fmt.Errorf("registry entry must be an object")
		}
		keys := map[string]bool{}
		for members.More() {
			token, err := members.Token()
			if err != nil {
				return nil, err
			}
			key, ok := token.(string)
			if !ok || keys[key] {
				return nil, fmt.Errorf("invalid or duplicate registry member %q", key)
			}
			keys[key] = true
			var target any
			switch key {
			case "version":
				target = &e.Version
			case "name":
				target = &e.Name
			case "file":
				target = &e.File
			case "checksum":
				target = &e.Checksum
			default:
				return nil, fmt.Errorf("unknown registry member %q", key)
			}
			if err := members.Decode(target); err != nil {
				return nil, err
			}
		}
		if _, err := members.Token(); err != nil || len(keys) != 4 {
			return nil, fmt.Errorf("incomplete registry entry")
		}
		if e.Version != i+1 || !migrationName.MatchString(e.Name) || strings.TrimSpace(e.Name) != e.Name || !migrationFile.MatchString(e.File) || !migrationHash.MatchString(e.Checksum) || seen["name:"+e.Name] || seen["file:"+e.File] {
			return nil, fmt.Errorf("invalid migration registry entry %d", i+1)
		}
		seen["name:"+e.Name], seen["file:"+e.File] = true, true
		body, err := read(e.File)
		if err != nil {
			return nil, err
		}
		if fmt.Sprintf("%x", sha256.Sum256(body)) != e.Checksum {
			return nil, fmt.Errorf("SQL checksum mismatch for %s", e.File)
		}
		if !utf8.Valid(body) || bytes.ContainsRune(body, 0) {
			return nil, fmt.Errorf("SQL must be UTF-8 without NUL: %s", e.File)
		}
		e.SQL = string(body)
		// The authority owns transaction boundaries. Conservatively exclude
		// transaction/connection control, including BEGIN/END trigger bodies.
		for _, token := range migrationTokens.FindAllString(e.SQL, -1) {
			switch strings.ToUpper(token) {
			case "BEGIN", "END", "COMMIT", "ROLLBACK", "SAVEPOINT", "RELEASE", "ATTACH", "DETACH", "PRAGMA":
				return nil, fmt.Errorf("migration SQL contains unsupported control token %s", token)
			}
		}
	}
	return entries, nil
}

type schemaObject struct{ Name, Type, SQL string }
type schemaReader interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func productSchema(ctx context.Context, db schemaReader) ([]schemaObject, error) {
	rows, err := db.QueryContext(ctx, "SELECT name,type,sql FROM main.sqlite_schema WHERE substr(name,1,7) != 'sqlite_' AND name != 'proxycore_schema_migrations' ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	objects := []schemaObject{}
	for rows.Next() {
		var object schemaObject
		if err := rows.Scan(&object.Name, &object.Type, &object.SQL); err != nil {
			return nil, err
		}
		objects = append(objects, object)
	}
	return objects, rows.Err()
}

// Private fixture seam; it runs the same validation and locking as production.
func migrateAuthority(ctx context.Context, db *sql.DB, raw []byte, read func(string) ([]byte, error)) (err error) {
	entries, err := validateMigrations(raw, read)
	if err != nil {
		return err
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, rollbackErr := conn.ExecContext(cleanup, "ROLLBACK"); rollbackErr != nil {
			// ErrBadConn tells database/sql to discard, never recycle, this driver connection.
			discardErr := conn.Raw(func(any) error { return driver.ErrBadConn })
			err = errors.Join(err, fmt.Errorf("rollback cleanup: %w (discard=%v)", rollbackErr, discardErr))
		}
	}()
	var temporary int
	if err := conn.QueryRowContext(ctx, "SELECT count(*) FROM temp.sqlite_schema WHERE substr(name,1,7) != 'sqlite_'").Scan(&temporary); err != nil || temporary != 0 {
		return fmt.Errorf("unrecognized temporary schema: count=%d error=%v", temporary, err)
	}
	var kind, ddl string
	err = conn.QueryRowContext(ctx, "SELECT type,sql FROM main.sqlite_schema WHERE name = 'proxycore_schema_migrations'").Scan(&kind, &ddl)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if err == nil && (kind != "table" || ddl != ledgerDDL) {
		return fmt.Errorf("unrecognized migration ledger schema")
	}
	if err == sql.ErrNoRows {
		if _, err := conn.ExecContext(ctx, ledgerDDL); err != nil {
			return err
		}
	}
	var invalidStorage int
	if err := conn.QueryRowContext(ctx, "SELECT count(*) FROM main.proxycore_schema_migrations WHERE typeof(version) != 'integer' OR typeof(name) != 'text' OR typeof(checksum) != 'text' OR typeof(applied_at) != 'text'").Scan(&invalidStorage); err != nil || invalidStorage != 0 {
		return fmt.Errorf("invalid ledger storage classes: count=%d error=%v", invalidStorage, err)
	}
	rows, err := conn.QueryContext(ctx, "SELECT version,name,checksum,applied_at FROM main.proxycore_schema_migrations ORDER BY version")
	if err != nil {
		return err
	}
	applied := 0
	for rows.Next() {
		var version int
		var name, checksum, clock string
		if err := rows.Scan(&version, &name, &checksum, &clock); err != nil {
			rows.Close()
			return err
		}
		stamp, clockErr := DecodeTimestamp(&clock)
		_, finite := stamp.(FiniteTimestamp)
		if applied >= len(entries) || version != applied+1 || name != entries[applied].Name || checksum != entries[applied].Checksum || clockErr != nil || !finite {
			rows.Close()
			return fmt.Errorf("unrecognized applied migration %d", version)
		}
		applied++
	}
	rowsErr := rows.Err()
	rows.Close()
	if rowsErr != nil {
		return rowsErr
	}
	scratch, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return err
	}
	defer scratch.Close()
	scratch.SetMaxOpenConns(1)
	if _, err := scratch.ExecContext(ctx, "PRAGMA foreign_keys = ON"); err != nil {
		return err
	}
	for _, e := range entries[:applied] {
		if _, err := scratch.ExecContext(ctx, e.SQL); err != nil {
			return fmt.Errorf("replay applied schema: %w", err)
		}
	}
	expected, err := productSchema(ctx, scratch)
	if err != nil {
		return err
	}
	actual, err := productSchema(ctx, conn)
	if err != nil || !slices.Equal(actual, expected) {
		return fmt.Errorf("unrecognized product schema: %v", err)
	}
	now := time.Now()
	clock, err := EncodeTimestamp(FiniteTimestamp{Seconds: now.Unix(), Nanos: int32(now.Nanosecond())})
	if err != nil {
		return err
	}
	for _, e := range entries[applied:] {
		if _, err := conn.ExecContext(ctx, e.SQL); err != nil {
			return fmt.Errorf("migration %d: %w", e.Version, err)
		}
		if _, err := conn.ExecContext(ctx, "INSERT INTO main.proxycore_schema_migrations VALUES (?, ?, ?, ?)", e.Version, e.Name, e.Checksum, *clock); err != nil {
			return err
		}
	}
	_, err = conn.ExecContext(ctx, "COMMIT")
	committed = err == nil
	return err
}
