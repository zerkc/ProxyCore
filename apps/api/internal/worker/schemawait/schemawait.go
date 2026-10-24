// Package schemawait ports waitForSchema from apps/worker/src/sqlite-runtime.ts.
// The worker never migrates: it waits, bounded and logged, for the Go API to
// create the database file and migrate it to the version this build expects.
package schemawait

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

// ExpectedVersion is the last version in sqlitedb/migrations/registry.json. A
// test keeps it aligned with that registry (sqlitedb does not export it).
const ExpectedVersion = 9

const (
	// DefaultWait is the total time to wait for the API to migrate.
	DefaultWait = 60 * time.Second
	// DefaultPoll is the delay between inspections.
	DefaultPoll = time.Second
)

// Options configures Wait. Zero values select the defaults.
type Options struct {
	Expected int
	Wait     time.Duration
	Poll     time.Duration
	Log      func(message string)
	// Sleep and Now are injectable for tests.
	Sleep func(ctx context.Context, d time.Duration)
	Now   func() time.Time
}

// Wait blocks until max(version) in proxycore_schema_migrations equals the
// expected version. A newer version is a hard error, the database file is never
// created, and it gives up with a descriptive error after Options.Wait.
func Wait(ctx context.Context, path string, opts Options) error {
	expected, wait, poll := opts.Expected, opts.Wait, opts.Poll
	if expected == 0 {
		expected = ExpectedVersion
	}
	if wait == 0 {
		wait = DefaultWait
	}
	if poll == 0 {
		poll = DefaultPoll
	}
	logf, sleep, now := opts.Log, opts.Sleep, opts.Now
	if logf == nil {
		logf = func(string) {}
	}
	if sleep == nil {
		sleep = func(ctx context.Context, d time.Duration) {
			timer := time.NewTimer(d)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-ctx.Done():
			}
		}
	}
	if now == nil {
		now = time.Now
	}
	deadline := now().Add(wait)
	var reason string
	for {
		version, state := inspect(ctx, path)
		switch {
		case state == "" && version == expected:
			return nil
		case state == "" && version > expected:
			return fmt.Errorf("sqlite schema version %d is newer than the version %d this worker supports", version, expected)
		case state == "":
			reason = fmt.Sprintf("at version %d", version)
		default:
			reason = state
		}
		if !now().Before(deadline) {
			break
		}
		logf(fmt.Sprintf("waiting for sqlite schema version %d at %s: %s", expected, path, reason))
		sleep(ctx, poll)
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	return fmt.Errorf("sqlite schema not ready after %dms (expected version %d, %s); the Go API owns migration",
		wait.Milliseconds(), expected, reason)
}

// inspect returns the schema version, or a non-empty reason when the database
// is missing or not ready. It never creates the file: opening one would race the
// API's migration with an empty database.
func inspect(ctx context.Context, path string) (int, string) {
	if _, err := os.Stat(path); err != nil {
		return 0, "database file does not exist"
	}
	db, err := sqlitedb.Open(path)
	if err != nil {
		return 0, err.Error()
	}
	defer db.Close()
	var version sql.NullInt64
	if err := db.QueryRowContext(ctx, "SELECT max(version) FROM main.proxycore_schema_migrations").Scan(&version); err != nil {
		return 0, err.Error()
	}
	if !version.Valid {
		return 0, "no migrations applied"
	}
	return int(version.Int64), ""
}
