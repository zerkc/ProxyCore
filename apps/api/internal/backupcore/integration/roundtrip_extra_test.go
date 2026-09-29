package integration

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zerkc/ProxyCore/apps/api/internal/backupcore/dbimport"
	"github.com/zerkc/ProxyCore/apps/api/internal/httpserver"
)

func TestRoundTripTriangulation(t *testing.T) {
	t.Run("no passphrase", func(t *testing.T) {
		fixture := newRoundTripFixture(t, nil)
		report, _, err := importFixture(fixture, fixture.masterKey, nil)
		if err != nil {
			t.Fatalf("no-passphrase: import returned error: %v", err)
		}
		if report.DryRun {
			t.Fatal("no-passphrase: expected dryRun=false")
		}
		if report.AppliedPostImport {
			t.Fatal("no-passphrase: expected appliedPostImport=false for the no-op apply")
		}
		assertDatabaseSnapshot(t, "no-passphrase", fixture.pool, fixture.counts, fixture.checksums)
	})

	t.Run("wrong passphrase", func(t *testing.T) {
		fixture := newRoundTripFixture(t, []byte("correct-passphrase"))
		_, _, err := importFixture(fixture, fixture.masterKey, []byte("wrong-passphrase"))
		if !backupErrorMatches(err, httpserver.ErrPassphraseMismatch) {
			t.Fatalf("wrong-passphrase: error=%v, want ErrPassphraseMismatch", err)
		}
		assertWiped(t, "wrong-passphrase", fixture.pool)
	})

	t.Run("master-key mismatch", func(t *testing.T) {
		fixture := newRoundTripFixtureWithWipeAfterExport(t, nil, false)
		before := captureConfigRowCounts(t, fixture.pool)
		if !equalSnapshots(before, fixture.preExportCounts) {
			t.Fatalf("master-key-mismatch: expected the pre-import database to contain the exported fixture: before=%v fixture=%v", before, fixture.preExportCounts)
		}

		wrongKey := randomMasterKey(t)
		_, _, err := importFixture(fixture, wrongKey, nil)
		if !errors.Is(err, httpserver.ErrMasterKeyMismatch) {
			t.Fatalf("master-key-mismatch: error=%v, want ErrMasterKeyMismatch", err)
		}

		after := captureConfigRowCounts(t, fixture.pool)
		assertConfigRowCountsUnchanged(t, "master-key-mismatch", before, after)
	})

	t.Run("master-key mismatch with non-empty bundle (captured pre/post counts)", func(t *testing.T) {
		fixture := newRoundTripFixtureWithWipeAfterExport(t, nil, false)
		tables := []string{"users", "zones", "certificates"}
		before := captureConfigRowCounts(t, fixture.pool, tables...)
		for _, table := range tables {
			if before[table] == 0 {
				t.Fatalf("master-key-mismatch-non-empty: expected %s to contain fixture rows", table)
			}
		}

		wrongKey := randomMasterKey(t)
		_, _, err := importFixture(fixture, wrongKey, nil)
		if !errors.Is(err, httpserver.ErrMasterKeyMismatch) {
			t.Fatalf("master-key-mismatch-non-empty: error=%v, want ErrMasterKeyMismatch", err)
		}

		after := captureConfigRowCounts(t, fixture.pool, tables...)
		assertConfigRowCountsUnchanged(t, "master-key-mismatch-non-empty", before, after, tables...)
	})

	t.Run("concurrent import", func(t *testing.T) {
		fixture := newRoundTripFixture(t, nil)
		installImportDelay(t, fixture.pool)
		start := make(chan struct{})
		results := make(chan error, 2)
		for index := 0; index < 2; index++ {
			go func() {
				<-start
				_, _, err := importFixture(fixture, fixture.masterKey, nil)
				results <- err
			}()
		}
		close(start)
		// The test-only trigger keeps the winning transaction inside the
		// advisory-lock window; this short sleep only lets both goroutines reach
		// the import boundary before results are collected.
		time.Sleep(10 * time.Millisecond)

		var successes, lockErrors int
		for index := 0; index < 2; index++ {
			err := <-results
			switch {
			case err == nil:
				successes++
			case errors.Is(err, dbimport.ErrImportAlreadyInProgress):
				lockErrors++
			default:
				t.Fatalf("concurrent-import: unexpected result: %v", err)
			}
		}
		if successes != 1 {
			t.Fatalf("concurrent-import: successful imports=%d, want 1", successes)
		}
		if lockErrors != 1 {
			t.Fatalf("concurrent-import: ErrImportAlreadyInProgress results=%d, want 1", lockErrors)
		}
	})

	t.Run("wipe import wipe reimport", func(t *testing.T) {
		fixture := newRoundTripFixture(t, nil)
		first, _, err := importFixture(fixture, fixture.masterKey, nil)
		if err != nil {
			t.Fatalf("double-import: first import returned error: %v", err)
		}
		if first.DryRun || first.AppliedPostImport {
			t.Fatalf("double-import: first report=%+v, want non-dry no-op apply", first)
		}
		firstCounts, firstChecksums := snapshotDatabase(t, fixture.pool)
		firstEnv, err := readFileForTest(fixture.envRestorePath)
		if err != nil {
			t.Fatalf("double-import: read first env: %v", err)
		}
		wipeConfig(t, fixture.pool)
		second, _, err := importFixture(fixture, fixture.masterKey, nil)
		if err != nil {
			t.Fatalf("double-import: second import returned error: %v", err)
		}
		if second.DryRun || second.AppliedPostImport {
			t.Fatalf("double-import: second report=%+v, want non-dry no-op apply", second)
		}
		secondCounts, secondChecksums := snapshotDatabase(t, fixture.pool)
		if !equalSnapshots(firstCounts, secondCounts) {
			t.Fatalf("double-import: row counts differ after reimport: first=%v second=%v", firstCounts, secondCounts)
		}
		if !equalSnapshots(firstChecksums, secondChecksums) {
			t.Fatalf("double-import: checksums differ after reimport: first=%v second=%v", firstChecksums, secondChecksums)
		}
		secondEnv, err := readFileForTest(fixture.envRestorePath)
		if err != nil {
			t.Fatalf("double-import: read second env: %v", err)
		}
		if !bytes.Equal(firstEnv, secondEnv) {
			t.Fatal("double-import: restored env differs after reimport")
		}
	})
}

func backupErrorMatches(err, want error) bool {
	return err != nil && (errors.Is(err, want) || err.Error() == want.Error())
}

func assertWiped(t *testing.T, name string, pool *pgxpool.Pool) {
	t.Helper()
	counts, _ := snapshotDatabase(t, pool)
	for table, count := range counts {
		if count != 0 {
			t.Fatalf("%s: wipe did not persist for %s (count=%d)", name, table, count)
		}
	}
}

func equalSnapshots[T comparable](left, right map[string]T) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}

func readFileForTest(path string) ([]byte, error) {
	return os.ReadFile(path)
}

func installImportDelay(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		create or replace function proxycore_backup_test_delay() returns trigger
		language plpgsql as $$ begin perform pg_sleep(0.150); return new; end $$;
		drop trigger if exists proxycore_backup_test_delay on users;
		create trigger proxycore_backup_test_delay before insert on users
		for each row execute function proxycore_backup_test_delay();
	`); err != nil {
		t.Fatalf("concurrent-import: install lock-window trigger: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `drop trigger if exists proxycore_backup_test_delay on users; drop function if exists proxycore_backup_test_delay()`)
	})
}
