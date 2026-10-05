package sqlitedb

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// snapshotAfterVacuum is a test seam invoked with the temp path after VACUUM INTO.
var snapshotAfterVacuum func(path string)

// Snapshot writes a consistent, standalone copy of db to destPath using
// VACUUM INTO. It never overwrites, never modifies the source, verifies the
// copy fail-closed, and removes its temp file on every failure. The parent
// directory of destPath must already exist.
func Snapshot(ctx context.Context, db *sql.DB, destPath string) (err error) {
	if destPath == "" || strings.ContainsRune(destPath, 0) {
		return fmt.Errorf("snapshot requires a nonempty destination without NUL")
	}
	dest, err := filepath.Abs(destPath)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(dest); err == nil {
		return fmt.Errorf("snapshot destination already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	dir := filepath.Dir(dest)
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return fmt.Errorf("snapshot destination directory is unavailable")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return err
	}
	tmp := filepath.Join(dir, ".snapshot-"+hex.EncodeToString(suffix[:])+".tmp")
	// O_EXCL refuses a pre-existing temp; SQLite accepts an empty target file.
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	f.Close()
	defer func() {
		// Success links dest then removes tmp; failures remove tmp and any sidecars.
		for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
			os.Remove(tmp + suffix)
		}
	}()

	if _, err := db.ExecContext(ctx, "VACUUM INTO ?", tmp); err != nil {
		return fmt.Errorf("snapshot VACUUM INTO: %w", err)
	}
	if snapshotAfterVacuum != nil {
		snapshotAfterVacuum(tmp)
	}
	if err := verifySnapshot(ctx, db, tmp); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	if err := syncPath(tmp); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Link then unlink gives an atomic rename that cannot overwrite a racing destination.
	if err := os.Link(tmp, dest); err != nil {
		return fmt.Errorf("snapshot publish: %w", err)
	}
	return syncPath(dir)
}

func syncPath(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func verifySnapshot(ctx context.Context, src *sql.DB, path string) error {
	// immutable=1 reads without creating -wal/-shm sidecars.
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(path), RawQuery: "mode=ro&immutable=1"}
	snap, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return err
	}
	defer snap.Close()
	snap.SetMaxOpenConns(1)
	for _, pragma := range []string{"integrity_check", "quick_check"} {
		var result string
		if err := snap.QueryRowContext(ctx, "PRAGMA "+pragma).Scan(&result); err != nil || result != "ok" {
			return fmt.Errorf("snapshot %s failed", pragma)
		}
	}
	rows, err := snap.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return fmt.Errorf("snapshot foreign_key_check failed")
	}
	violations := rows.Next()
	rows.Close()
	if violations {
		return fmt.Errorf("snapshot foreign_key_check failed")
	}
	want, err := ledgerSummary(ctx, src)
	if err != nil {
		return err
	}
	got, err := ledgerSummary(ctx, snap)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("snapshot schema version mismatch")
	}
	return nil
}

// ledgerSummary returns the applied migration ledger as comparable text; an
// absent ledger (unmigrated database) summarizes as empty.
func ledgerSummary(ctx context.Context, db *sql.DB) (string, error) {
	var exists int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM main.sqlite_schema WHERE name = 'proxycore_schema_migrations'").Scan(&exists); err != nil {
		return "", err
	}
	if exists == 0 {
		return "", nil
	}
	var summary sql.NullString
	if err := db.QueryRowContext(ctx, "SELECT group_concat(version || ':' || checksum, ',') FROM (SELECT version,checksum FROM main.proxycore_schema_migrations ORDER BY version)").Scan(&summary); err != nil {
		return "", err
	}
	return summary.String, nil
}
