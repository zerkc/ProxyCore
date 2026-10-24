package schemawait_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
	"github.com/zerkc/ProxyCore/apps/api/internal/worker/schemawait"
	"github.com/zerkc/ProxyCore/apps/api/internal/worker/testsupport"
)

func TestExpectedVersionMatchesMigrationRegistry(t *testing.T) {
	raw, err := os.ReadFile(testsupport.RepoPath("apps/api/internal/sqlitedb/migrations/registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	var entries []struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(raw, &entries); err != nil {
		t.Fatal(err)
	}
	latest := 0
	for _, e := range entries {
		latest = max(latest, e.Version)
	}
	if schemawait.ExpectedVersion != latest {
		t.Fatalf("ExpectedVersion = %d but the migration registry ends at %d; bump schemawait.ExpectedVersion with the migration", schemawait.ExpectedVersion, latest)
	}
}

// fastOptions never sleeps for real and advances a fake clock by the requested delay.
func fastOptions(logs *[]string) (schemawait.Options, *time.Time) {
	now := time.Unix(1_000_000, 0)
	return schemawait.Options{
		Wait: 60 * time.Second, Poll: time.Second,
		Now:   func() time.Time { return now },
		Sleep: func(_ context.Context, d time.Duration) { now = now.Add(d) },
		Log:   func(m string) { *logs = append(*logs, m) },
	}, &now
}

func TestWaitReturnsWhenSchemaIsAtExpectedVersion(t *testing.T) {
	_, path := testsupport.NewDB(t)
	var logs []string
	opts, _ := fastOptions(&logs)
	if err := schemawait.Wait(context.Background(), path, opts); err != nil {
		t.Fatal(err)
	}
	if len(logs) != 0 {
		t.Fatalf("logged while ready: %v", logs)
	}
}

func TestWaitNeverCreatesTheDatabaseFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.sqlite")
	var logs []string
	opts, now := fastOptions(&logs)
	start := *now
	err := schemawait.Wait(context.Background(), path, opts)
	if err == nil || !strings.Contains(err.Error(), "sqlite schema not ready after 60000ms (expected version 9, database file does not exist); the Go API owns migration") {
		t.Fatalf("err = %v", err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatalf("Wait created or touched the database file: %v", statErr)
	}
	if elapsed := now.Sub(start); elapsed < 60*time.Second {
		t.Fatalf("gave up after %v, want the full 60s wait", elapsed)
	}
	if len(logs) != 60 || logs[0] != "waiting for sqlite schema version 9 at "+path+": database file does not exist" {
		t.Fatalf("logs = %d entries, first %q", len(logs), logs[0])
	}
}

func TestWaitSucceedsOnceTheAPIFinishesMigrating(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proxycore.sqlite")
	db, err := sqlitedb.Open(path) // file exists but has no ledger yet
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var logs []string
	opts, now := fastOptions(&logs)
	sleeps := 0
	opts.Sleep = func(_ context.Context, d time.Duration) {
		*now = now.Add(d)
		if sleeps++; sleeps == 3 {
			if err := sqlitedb.Migrate(context.Background(), db); err != nil {
				t.Error(err)
			}
		}
	}
	if err := schemawait.Wait(context.Background(), path, opts); err != nil {
		t.Fatal(err)
	}
	if sleeps != 3 || len(logs) != 3 {
		t.Fatalf("sleeps=%d logs=%v, want 3 polls before the migration landed", sleeps, logs)
	}
}

func TestWaitFailsFastOnNewerSchema(t *testing.T) {
	db, path := testsupport.NewDB(t)
	if _, err := db.Exec("insert into proxycore_schema_migrations (version,name,checksum,applied_at) values (?,?,?,?)",
		schemawait.ExpectedVersion+1, "future", strings.Repeat("0", 64), "0"); err != nil {
		t.Fatal(err)
	}
	var logs []string
	opts, now := fastOptions(&logs)
	start := *now
	err := schemawait.Wait(context.Background(), path, opts)
	want := "sqlite schema version 10 is newer than the version 9 this worker supports"
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
	if !now.Equal(start) {
		t.Fatal("a newer schema must fail without waiting")
	}
}

func TestWaitReportsAnEmptyLedgerAndHonoursCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.sqlite")
	db, err := sqlitedb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("create table proxycore_schema_migrations(version integer not null primary key, name text, checksum text, applied_at text)"); err != nil {
		t.Fatal(err)
	}
	var logs []string
	opts, _ := fastOptions(&logs)
	ctx, cancel := context.WithCancel(context.Background())
	opts.Sleep = func(context.Context, time.Duration) { cancel() }
	err = schemawait.Wait(ctx, path, opts)
	if err == nil || err != context.Canceled {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if len(logs) != 1 || !strings.HasSuffix(logs[0], ": no migrations applied") {
		t.Fatalf("logs = %v", logs)
	}
}
