// Package sqlitedb provides local SQLite connections without wiring active runtimes.
package sqlitedb

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Open opens a literal local filename with WAL, foreign keys, and a 5-second
// busy timeout. The caller owns Close. Parent directories must already exist.
func Open(path string) (*sql.DB, error) {
	if path == "" || strings.ContainsRune(path, 0) {
		return nil, fmt.Errorf("SQLite requires a nonempty filename without NUL")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	// Encode filenames as URI paths, never as caller-controlled DSN parameters.
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(absolute)}
	query := url.Values{}
	query.Add("_pragma", "busy_timeout(5000)")
	query.Add("_pragma", "foreign_keys(1)")
	query.Add("_pragma", "journal_mode(WAL)")
	uri.RawQuery = query.Encode()
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("open SQLite: %w", err)
	}
	return db, nil
}
