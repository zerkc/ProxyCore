package configuration

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

const candidateRevisionID = "00000000-0000-0000-0000-000000000001"

func candidateFixture(t *testing.T) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "candidates.db")
	db, err := sqlitedb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := sqlitedb.Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	settingsExec(t, db, `insert into config_revisions(id,revision_number,checksum,snapshot,created_at) values (?,1,'fixture','{}','0')`, candidateRevisionID)
	return db, path
}

func candidateJob(t *testing.T, db *sql.DB, id string, finished any) {
	t.Helper()
	settingsExec(t, db, `insert into apply_jobs(id,revision_id,target,status,correlation_id,created_at,finished_at) values (?,?,'combined','applied','fixture','0',?)`, id, candidateRevisionID, finished)
}

func assertCandidates(t *testing.T, store *SQLiteCanonicalSnapshotCandidateStore, ids ...string) {
	t.Helper()
	want := make([]CanonicalSnapshotApplyRef, 0, len(ids))
	for _, id := range ids {
		want = append(want, CanonicalSnapshotApplyRef{JobID: id})
	}
	got, err := store.ListCanonicalSnapshotCandidates(context.Background())
	if err != nil || got == nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("candidates = %#v, %v; want %#v", got, err, want)
	}
}

func TestSQLiteCanonicalSnapshotCandidatesEligible(t *testing.T) {
	db, _ := candidateFixture(t)
	store := NewSQLiteCanonicalSnapshotCandidateStore(db)
	assertCandidates(t, store)
	id := "00000000-0000-0000-0000-000000000002"
	candidateJob(t, db, id, "0")
	assertCandidates(t, store, id)
}

func candidateID(n int) string {
	return fmt.Sprintf("00000000-0000-0000-0000-%012x", n)
}

func candidateSnapshot(t *testing.T, db *sql.DB, n int, job any, status string, discarded, body any) {
	t.Helper()
	settingsExec(t, db, `insert into applied_snapshots
  (id,source_primary_id,leadership_generation,snapshot_version,replication_version,content_hash,status,apply_job_id,applied_at,discarded_at,snapshot_body)
  values (?,?,1,1,1,?,?,?,'0',?,?)`, candidateID(n), candidateRevisionID, fmt.Sprint(n), status, job, discarded, body)
}

func TestSQLiteCanonicalSnapshotCandidatesJobFilters(t *testing.T) {
	for _, axis := range []struct {
		column string
		values []string
	}{
		{"status", []string{"queued", "validating", "applying", "applied", "failed", "rolled-back"}},
		{"target", []string{"coredns", "nginx", "combined", "certificate"}},
		{"source", []string{"ordinary", "import", "sync"}},
	} {
		for _, value := range axis.values {
			t.Run(axis.column+"/"+value, func(t *testing.T) {
				db, _ := candidateFixture(t)
				id := candidateID(2)
				candidateJob(t, db, id, "2")
				settingsExec(t, db, "update apply_jobs set "+axis.column+" = ?", value)
				store := NewSQLiteCanonicalSnapshotCandidateStore(db)
				if value == "applied" || value == "combined" || value == "ordinary" {
					assertCandidates(t, store, id)
				} else {
					assertCandidates(t, store)
				}
			})
		}
	}
	t.Run("unfinished", func(t *testing.T) {
		db, _ := candidateFixture(t)
		candidateJob(t, db, candidateID(2), nil)
		assertCandidates(t, NewSQLiteCanonicalSnapshotCandidateStore(db))
	})
}

func TestSQLiteCanonicalSnapshotCandidatesSnapshotFilters(t *testing.T) {
	for _, status := range []string{"pending", "applied", "rejected", "rolled-back", "archived"} {
		for _, discarded := range []any{nil, "0"} {
			for _, body := range []struct {
				name  string
				value any
			}{
				{"null", nil}, {"empty", []byte{}}, {"opaque", []byte{0, 255}},
			} {
				t.Run(fmt.Sprintf("%s/discarded=%v/%s", status, discarded, body.name), func(t *testing.T) {
					db, _ := candidateFixture(t)
					id := candidateID(2)
					candidateJob(t, db, id, "0")
					candidateSnapshot(t, db, 100, id, status, discarded, body.value)
					store := NewSQLiteCanonicalSnapshotCandidateStore(db)
					if status == "applied" && discarded == nil && body.value != nil {
						assertCandidates(t, store)
					} else {
						assertCandidates(t, store, id)
					}
				})
			}
		}
	}
}

func TestSQLiteCanonicalSnapshotCandidatesMultipleAndUnrelated(t *testing.T) {
	db, _ := candidateFixture(t)
	store := NewSQLiteCanonicalSnapshotCandidateStore(db)
	for n := 2; n <= 4; n++ {
		candidateJob(t, db, candidateID(n), "0")
	}
	// Multiple qualifying snapshots must exclude, not duplicate, their job.
	candidateSnapshot(t, db, 100, candidateID(2), "applied", nil, []byte{})
	candidateSnapshot(t, db, 101, candidateID(2), "applied", nil, []byte{1})
	// Multiple nonqualifying rows still produce exactly one candidate.
	candidateSnapshot(t, db, 102, candidateID(3), "pending", nil, []byte{})
	candidateSnapshot(t, db, 103, candidateID(3), "applied", "2", []byte{})
	candidateSnapshot(t, db, 104, candidateID(3), "applied", nil, nil)
	// No job association and another job's publication cannot affect job 4.
	candidateSnapshot(t, db, 105, nil, "applied", nil, []byte{})
	assertCandidates(t, store, candidateID(3), candidateID(4))
	candidateSnapshot(t, db, 106, candidateID(3), "applied", nil, []byte{})
	assertCandidates(t, store, candidateID(4))
}

func TestSQLiteCanonicalSnapshotCandidatesTaggedOrdering(t *testing.T) {
	db, _ := candidateFixture(t)
	times := []sqlitedb.Timestamp{
		sqlitedb.PositiveInfinity{},
		sqlitedb.FiniteTimestamp{Seconds: 0, Nanos: 1},
		sqlitedb.FiniteTimestamp{Seconds: 0},
		sqlitedb.NegativeInfinity{},
		sqlitedb.FiniteTimestamp{Seconds: -1, Nanos: 999999999},
		sqlitedb.FiniteTimestamp{Seconds: 0},
		sqlitedb.FiniteTimestamp{Seconds: 1<<63 - 1, Nanos: 999999999},
		sqlitedb.FiniteTimestamp{Seconds: -1 << 63},
	}
	// Deliberately insert backwards, including equal clocks with different IDs.
	for i := len(times) - 1; i >= 0; i-- {
		key, err := sqlitedb.EncodeTimestamp(times[i])
		if err != nil {
			t.Fatal(err)
		}
		candidateJob(t, db, candidateID(i+2), *key)
	}
	assertCandidates(t, NewSQLiteCanonicalSnapshotCandidateStore(db),
		candidateID(5), candidateID(9), candidateID(6), candidateID(4),
		candidateID(7), candidateID(3), candidateID(8), candidateID(2))
}

func candidateState(t *testing.T, db *sql.DB) [][][]any {
	t.Helper()
	state := make([][][]any, 0)
	for _, table := range []string{"installation_settings", "config_revisions", "apply_jobs", "applied_snapshots"} {
		rows, err := db.Query("select * from " + table + " order by id")
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		data := make([][]any, 0)
		for rows.Next() {
			values := make([]any, len(columns))
			destinations := make([]any, len(columns))
			for i := range values {
				destinations[i] = &values[i]
			}
			if err := rows.Scan(destinations...); err != nil {
				t.Fatal(err)
			}
			data = append(data, values)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			t.Fatal(err)
		}
		state = append(state, data)
	}
	return state
}

func TestSQLiteCanonicalSnapshotCandidatesPurity(t *testing.T) {
	db, _ := candidateFixture(t)
	db.SetMaxOpenConns(1)
	candidateJob(t, db, candidateID(2), "0")
	candidateJob(t, db, candidateID(3), "2")
	candidateSnapshot(t, db, 100, candidateID(2), "applied", nil, []byte{})
	before := candidateState(t, db)
	// This connection forbids writes, including accidental settings initialization.
	settingsExec(t, db, "pragma query_only=on")
	store := NewSQLiteCanonicalSnapshotCandidateStore(db)
	for i := 0; i < 3; i++ {
		assertCandidates(t, store, candidateID(3))
	}
	if after := candidateState(t, db); !reflect.DeepEqual(after, before) {
		t.Fatal("candidate reads mutated state")
	}
}

func TestSQLiteCanonicalSnapshotCandidatesVisibilityAndReopen(t *testing.T) {
	db, path := candidateFixture(t)
	store := NewSQLiteCanonicalSnapshotCandidateStore(db)
	candidateJob(t, db, candidateID(2), "0")
	external, err := sqlitedb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer external.Close()
	tx, err := external.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	_, err = tx.Exec(`insert into apply_jobs(id,revision_id,target,status,correlation_id,created_at,finished_at)
  values (?,?,'combined','applied','external','0','2')`, candidateID(3), candidateRevisionID)
	if err != nil {
		t.Fatal(err)
	}
	assertCandidates(t, store, candidateID(2))
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	assertCandidates(t, store, candidateID(2), candidateID(3))
	// Uncommitted publication is invisible; rolling it back leaves the job eligible.
	tx, err = external.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	_, err = tx.Exec(`insert into applied_snapshots
  (id,source_primary_id,leadership_generation,snapshot_version,replication_version,content_hash,apply_job_id,applied_at,snapshot_body)
  values (?,?,1,1,1,'external',?,'0',x'')`, candidateID(100), candidateRevisionID, candidateID(2))
	if err != nil {
		t.Fatal(err)
	}
	assertCandidates(t, store, candidateID(2), candidateID(3))
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	assertCandidates(t, store, candidateID(2), candidateID(3))
	candidateSnapshot(t, external, 101, candidateID(2), "applied", nil, []byte{})
	assertCandidates(t, store, candidateID(3))
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := sqlitedb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	assertCandidates(t, NewSQLiteCanonicalSnapshotCandidateStore(reopened), candidateID(3))
}

func TestSQLiteCanonicalSnapshotCandidatesErrors(t *testing.T) {
	db, _ := candidateFixture(t)
	candidateJob(t, db, candidateID(2), "0")
	store := NewSQLiteCanonicalSnapshotCandidateStore(db)
	for _, bad := range []*SQLiteCanonicalSnapshotCandidateStore{nil, NewSQLiteCanonicalSnapshotCandidateStore(nil)} {
		got, err := bad.ListCanonicalSnapshotCandidates(context.Background())
		if got != nil || !errors.Is(err, ErrSnapshotPublicationStore) {
			t.Fatalf("nil store: %v, %v", got, err)
		}
	}
	got, err := store.ListCanonicalSnapshotCandidates(nil)
	if got != nil || err == nil || err.Error() != "snapshot candidate context is required" {
		t.Fatalf("nil context: %v, %v", got, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err = store.ListCanonicalSnapshotCandidates(ctx)
	if got != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled: %v, %v", got, err)
	}
	// An unmigrated temporary SQLite database exercises actual query failure.
	missing, err := sqlitedb.Open(filepath.Join(t.TempDir(), "missing.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer missing.Close()
	got, err = NewSQLiteCanonicalSnapshotCandidateStore(missing).ListCanonicalSnapshotCandidates(context.Background())
	if got != nil || err == nil {
		t.Fatalf("missing schema: %v, %v", got, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	got, err = store.ListCanonicalSnapshotCandidates(context.Background())
	if got != nil || err == nil {
		t.Fatalf("closed DB: %v, %v", got, err)
	}
}

// Fault rows isolate database/sql failure boundaries that the strict, migrated
// TEXT primary key cannot naturally trigger. Real SQLite covers behavior above.
type candidateFaultConnector struct{ scan bool }
type candidateFaultDriver struct{}
type candidateFaultConn struct{ scan bool }
type candidateFaultRows struct {
	scan bool
	next int
}

var candidateIterationError = errors.New("candidate iteration fault")

func (c candidateFaultConnector) Connect(context.Context) (driver.Conn, error) {
	return candidateFaultConn{scan: c.scan}, nil
}
func (candidateFaultConnector) Driver() driver.Driver { return candidateFaultDriver{} }
func (candidateFaultDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("use connector")
}
func (candidateFaultConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected prepare")
}
func (candidateFaultConn) Close() error { return nil }
func (candidateFaultConn) Begin() (driver.Tx, error) {
	return nil, errors.New("unexpected transaction")
}
func (c candidateFaultConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	return &candidateFaultRows{scan: c.scan}, nil
}
func (*candidateFaultRows) Columns() []string { return []string{"id"} }
func (*candidateFaultRows) Close() error      { return nil }
func (r *candidateFaultRows) Next(values []driver.Value) error {
	r.next++
	if r.next == 1 {
		values[0] = candidateID(2)
		return nil
	}
	if r.next == 2 {
		if !r.scan {
			return candidateIterationError
		}
		values[0] = nil // SQL NULL cannot scan into the required string.
		return nil
	}
	return io.EOF
}

func TestSQLiteCanonicalSnapshotCandidatesNoPartialOnRowErrors(t *testing.T) {
	for _, scan := range []bool{false, true} {
		t.Run(fmt.Sprintf("scan=%v", scan), func(t *testing.T) {
			db := sql.OpenDB(candidateFaultConnector{scan: scan})
			defer db.Close()
			got, err := NewSQLiteCanonicalSnapshotCandidateStore(db).ListCanonicalSnapshotCandidates(context.Background())
			if got != nil || err == nil {
				t.Fatalf("partial results on fault: %v, %v", got, err)
			}
			if !scan && !errors.Is(err, candidateIterationError) {
				t.Fatalf("iteration error lost: %v", err)
			}
		})
	}
}
