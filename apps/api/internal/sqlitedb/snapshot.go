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

// ErrSnapshotPublishedUnsynced reports that the snapshot was fully verified and
// published at the destination, but fsyncing the parent directory failed, so the
// durability of the directory entry is unconfirmed. Check with errors.Is.
var ErrSnapshotPublishedUnsynced = errors.New("snapshot published but directory sync failed")

// Test seams. Unexported; production values are the real implementations or nil.
var (
	// snapshotAfterVacuum is invoked with the temp path after VACUUM INTO.
	snapshotAfterVacuum func(path string)
	snapshotLink        = os.Link
	snapshotSync        = syncPath
)

// Snapshot writes a consistent, standalone copy of db to destPath using
// VACUUM INTO. It never overwrites, never modifies the source, verifies the
// copy fail-closed, and removes its temp file on every failure. The parent
// directory of destPath must already exist.
//
// Contract:
//   - Any error other than ErrSnapshotPublishedUnsynced means nothing was
//     published at destPath.
//   - An error matching ErrSnapshotPublishedUnsynced means destPath exists with
//     verified 0600 content, but the directory entry may not be durable; callers
//     may keep the file or remove it and retry.
//   - Publishing uses a hard link so it can never overwrite a racing
//     destination. There is deliberately no rename fallback (it would
//     reintroduce the overwrite race): the destination filesystem must support
//     hard links, otherwise Snapshot fails closed.
//   - The source migration ledger is read after the copy on a separate
//     connection. A migration committing in between yields a spurious
//     "schema version mismatch" failure; it is harmless (nothing is published)
//     and the caller may simply retry.
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
	if err := snapshotSync(tmp); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Link then unlink gives an atomic rename that cannot overwrite a racing destination.
	if err := snapshotLink(tmp, dest); err != nil {
		return fmt.Errorf("snapshot publish failed (destination filesystem must support hard links): %w", err)
	}
	if err := snapshotSync(dir); err != nil {
		return fmt.Errorf("%w: %w", ErrSnapshotPublishedUnsynced, err)
	}
	return nil
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
			if cerr := ctx.Err(); cerr != nil {
				return cerr
			}
			return fmt.Errorf("snapshot %s failed", pragma)
		}
	}
	rows, err := snap.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
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
