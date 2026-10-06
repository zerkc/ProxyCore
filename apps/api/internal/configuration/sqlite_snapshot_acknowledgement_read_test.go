package configuration

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

const ackReadNode = "abcdefab-0000-0000-0000-000000000001"
const ackReadRevision = "00000000-0000-0000-0000-000000000002"

var ackReadHash = strings.Repeat("a", 64)

func ackReadFixture(t *testing.T) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "acks.db")
	db, err := sqlitedb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := sqlitedb.Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	settingsExec(t, db, `insert into config_revisions(id,revision_number,checksum,snapshot,created_at) values (?,1,'synthetic','{}','0')`, ackReadRevision)
	ackReadParent(t, db, ackReadNode)
	return db, path
}

func ackReadParent(t *testing.T, db *sql.DB, node string) {
	t.Helper()
	// Synthetic FK-only parent; no actual credential or authentication material.
	settingsExec(t, db, `insert into node_credentials(id,node_id,credential_hash,hash_version,created_at) values (?,?,'synthetic','fixture','0')`, node, node)
	settingsExec(t, db, `insert into enrolled_nodes(node_id,installation_id,primary_id,credential_id,enrolled_at,created_by_attempt_id) values (?,?,?,?,'0',?)`, node, node, ackReadRevision, node, node)
}

func ackReadStamp(t *testing.T, seconds int64, nanos int) string {
	t.Helper()
	key, err := sqlitedb.EncodeTimestamp(sqlitedb.FiniteTimestamp{Seconds: seconds, Nanos: int32(nanos)})
	if err != nil {
		t.Fatal(err)
	}
	return *key
}

func ackReadInsert(t *testing.T, db *sql.DB, node, hash string) SnapshotAcknowledgement {
	t.Helper()
	settingsExec(t, db, `insert into node_snapshot_acks(node_id,content_hash,snapshot_version,replication_version,revision_id,leadership_generation,applied_at,received_at) values (?,?,7,9,?,11,?,?)`, node, hash, ackReadRevision, ackReadStamp(t, -1, 123456789), ackReadStamp(t, 1, 987654321))
	return SnapshotAcknowledgement{NodeID: node, ContentHash: hash, SnapshotVersion: 7, ReplicationVersion: 9, RevisionID: ackReadRevision, LeadershipGeneration: 11, AppliedAt: time.Unix(-1, 123456789), ReceivedAt: time.Unix(1, 987654321)}
}

func TestSQLiteSnapshotAcknowledgementReadRow(t *testing.T) {
	db, _ := ackReadFixture(t)
	want := ackReadInsert(t, db, ackReadNode, ackReadHash)
	got, err := NewSQLiteSnapshotAcknowledgementReadStore(db).GetSnapshotAcknowledgement(context.Background(), ackReadNode, ackReadHash)
	if err != nil || got != want {
		t.Fatalf("read = %#v, %v; want %#v", got, err, want)
	}
}

func TestSQLiteSnapshotAcknowledgementReadCompositeMiss(t *testing.T) {
	db, _ := ackReadFixture(t)
	ackReadInsert(t, db, ackReadNode, ackReadHash)
	for _, tuple := range [][2]string{{ackReadNode, strings.Repeat("b", 64)}, {ackReadRevision, ackReadHash}} {
		got, err := NewSQLiteSnapshotAcknowledgementReadStore(db).GetSnapshotAcknowledgement(context.Background(), tuple[0], tuple[1])
		if got != (SnapshotAcknowledgement{}) || err != ErrSnapshotAcknowledgementNotFound {
			t.Fatalf("miss = %#v, %v", got, err)
		}
	}
}

func ackReadError(t *testing.T, store *SQLiteSnapshotAcknowledgementReadStore, ctx context.Context, node, hash string, want error) {
	t.Helper()
	got, err := store.GetSnapshotAcknowledgement(ctx, node, hash)
	if got != (SnapshotAcknowledgement{}) || err != want {
		t.Fatalf("read = %#v, %v; want zero, %v", got, err, want)
	}
}

func TestSQLiteSnapshotAcknowledgementReadValidation(t *testing.T) {
	db, _ := ackReadFixture(t)
	stores := []*SQLiteSnapshotAcknowledgementReadStore{NewSQLiteSnapshotAcknowledgementReadStore(db), nil, NewSQLiteSnapshotAcknowledgementReadStore(nil)}
	for i, store := range stores {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			ackReadError(t, store, nil, ackReadNode, ackReadHash, ErrSnapshotAcknowledgementDenied)
			for _, node := range []string{"", "bad", "00000000-0000-0000-0000-000000000000", strings.ToUpper(ackReadNode), strings.ReplaceAll(ackReadNode, "-", ""), "{" + ackReadNode + "}", "urn:uuid:" + ackReadNode, ackReadNode + "\x00"} {
				t.Run("node="+node, func(t *testing.T) {
					ackReadError(t, store, context.Background(), node, ackReadHash, ErrSnapshotAcknowledgementDenied)
				})
			}
			for _, hash := range []string{"", strings.Repeat("a", 63), strings.Repeat("a", 65), strings.Repeat("A", 64), strings.Repeat("z", 64), strings.Repeat("a", 63) + "\x00"} {
				t.Run("hash="+hash, func(t *testing.T) {
					ackReadError(t, store, context.Background(), ackReadNode, hash, ErrSnapshotAcknowledgementDenied)
				})
			}
			if i > 0 {
				ackReadError(t, store, context.Background(), ackReadNode, ackReadHash, ErrSnapshotAcknowledgementUnavailable)
			}
		})
	}
}

func TestSQLiteSnapshotAcknowledgementReadDatabaseErrors(t *testing.T) {
	db, _ := ackReadFixture(t)
	store := NewSQLiteSnapshotAcknowledgementReadStore(db)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ackReadError(t, store, ctx, ackReadNode, ackReadHash, ErrSnapshotAcknowledgementUnavailable)
	missing, err := sqlitedb.Open(filepath.Join(t.TempDir(), "missing.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer missing.Close()
	ackReadError(t, NewSQLiteSnapshotAcknowledgementReadStore(missing), context.Background(), ackReadNode, ackReadHash, ErrSnapshotAcknowledgementUnavailable)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	ackReadError(t, store, context.Background(), ackReadNode, ackReadHash, ErrSnapshotAcknowledgementUnavailable)
}

func TestSQLiteSnapshotAcknowledgementReadStoredIntegers(t *testing.T) {
	db, _ := ackReadFixture(t)
	want := ackReadInsert(t, db, ackReadNode, ackReadHash)
	for _, v := range []struct {
		snapshot, replication int
		generation            int64
	}{{0, 0, 0}, {-1, -2, -3}, {math.MinInt32, math.MaxInt32, math.MinInt64}, {math.MaxInt32, math.MinInt32, math.MaxInt64}} {
		t.Run(fmt.Sprint(v), func(t *testing.T) {
			settingsExec(t, db, `update node_snapshot_acks set snapshot_version=?,replication_version=?,leadership_generation=?`, v.snapshot, v.replication, v.generation)
			want.SnapshotVersion, want.ReplicationVersion, want.LeadershipGeneration = v.snapshot, v.replication, v.generation
			got, err := NewSQLiteSnapshotAcknowledgementReadStore(db).GetSnapshotAcknowledgement(context.Background(), ackReadNode, ackReadHash)
			if err != nil || got != want {
				t.Fatalf("stored integers = %#v, %v; want %#v", got, err, want)
			}
		})
	}
}

func TestSQLiteSnapshotAcknowledgementReadClocks(t *testing.T) {
	for _, column := range []string{"applied_at", "received_at"} {
		for _, clock := range []string{"0", "2", ackReadStamp(t, math.MaxInt64, 0), ackReadStamp(t, math.MinInt64, 0)} {
			t.Run(column+"/"+clock, func(t *testing.T) {
				db, _ := ackReadFixture(t)
				ackReadInsert(t, db, ackReadNode, ackReadHash)
				settingsExec(t, db, "update node_snapshot_acks set "+column+"=?", clock)
				ackReadError(t, NewSQLiteSnapshotAcknowledgementReadStore(db), context.Background(), ackReadNode, ackReadHash, ErrSnapshotAcknowledgementUnavailable)
			})
		}
	}
	t.Run("epoch and zero Go time", func(t *testing.T) {
		db, _ := ackReadFixture(t)
		want := ackReadInsert(t, db, ackReadNode, ackReadHash)
		settingsExec(t, db, `update node_snapshot_acks set applied_at=?,received_at=?`, ackReadStamp(t, 0, 1), ackReadStamp(t, -62135596800, 0))
		want.AppliedAt, want.ReceivedAt = time.Unix(0, 1), time.Unix(-62135596800, 0)
		got, err := NewSQLiteSnapshotAcknowledgementReadStore(db).GetSnapshotAcknowledgement(context.Background(), ackReadNode, ackReadHash)
		if err != nil || got != want {
			t.Fatalf("finite clocks = %#v, %v", got, err)
		}
	})
}

func TestSQLiteSnapshotAcknowledgementReadCompositeRowsAndPurity(t *testing.T) {
	db, _ := ackReadFixture(t)
	node2 := "abcdefab-0000-0000-0000-000000000003"
	ackReadParent(t, db, node2)
	hash2 := strings.Repeat("b", 64)
	rows := []SnapshotAcknowledgement{ackReadInsert(t, db, ackReadNode, ackReadHash), ackReadInsert(t, db, ackReadNode, hash2), ackReadInsert(t, db, node2, ackReadHash)}
	settingsExec(t, db, `pragma query_only=on`)
	var before, after int
	if err := db.QueryRow(`select total_changes()`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	for repeat := 0; repeat < 2; repeat++ {
		for _, want := range rows {
			got, err := NewSQLiteSnapshotAcknowledgementReadStore(db).GetSnapshotAcknowledgement(context.Background(), want.NodeID, want.ContentHash)
			if err != nil || got != want {
				t.Fatalf("composite row = %#v, %v; want %#v", got, err, want)
			}
		}
	}
	ackReadError(t, NewSQLiteSnapshotAcknowledgementReadStore(db), context.Background(), node2, hash2, ErrSnapshotAcknowledgementNotFound)
	if err := db.QueryRow(`select total_changes()`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("reader changed rows: %d -> %d", before, after)
	}
}

func TestSQLiteSnapshotAcknowledgementReadVisibilityAndReopen(t *testing.T) {
	db, path := ackReadFixture(t)
	want := ackReadInsert(t, db, ackReadNode, ackReadHash)
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
	if _, err := tx.Exec(`update node_snapshot_acks set snapshot_version=42`); err != nil {
		t.Fatal(err)
	}
	store := NewSQLiteSnapshotAcknowledgementReadStore(db)
	got, err := store.GetSnapshotAcknowledgement(context.Background(), ackReadNode, ackReadHash)
	if err != nil || got != want {
		t.Fatalf("uncommitted visible: %#v, %v", got, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	want.SnapshotVersion = 42
	got, err = store.GetSnapshotAcknowledgement(context.Background(), ackReadNode, ackReadHash)
	if err != nil || got != want {
		t.Fatalf("committed invisible: %#v, %v", got, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := sqlitedb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got, err = NewSQLiteSnapshotAcknowledgementReadStore(reopened).GetSnapshotAcknowledgement(context.Background(), ackReadNode, ackReadHash)
	if err != nil || got != want {
		t.Fatalf("reopen = %#v, %v", got, err)
	}
}

// Test-only database/sql faults isolate scan and decode failures prohibited by
// the real STRICT schema. No alternate production schema or driver is wired.
type ackReadFaultConnector struct{ values []driver.Value }
type ackReadFaultDriver struct{}
type ackReadFaultConn struct{ values []driver.Value }
type ackReadFaultRows struct {
	values []driver.Value
	done   bool
}

func (c ackReadFaultConnector) Connect(context.Context) (driver.Conn, error) {
	return ackReadFaultConn{c.values}, nil
}
func (ackReadFaultConnector) Driver() driver.Driver         { return ackReadFaultDriver{} }
func (ackReadFaultDriver) Open(string) (driver.Conn, error) { return nil, errors.New("use connector") }
func (ackReadFaultConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected prepare")
}
func (ackReadFaultConn) Begin() (driver.Tx, error) { return nil, errors.New("unexpected transaction") }
func (ackReadFaultConn) Close() error              { return nil }
func (c ackReadFaultConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	return &ackReadFaultRows{values: c.values}, nil
}
func (*ackReadFaultRows) Columns() []string {
	return []string{"node_id", "content_hash", "snapshot_version", "replication_version", "revision_id", "leadership_generation", "applied_at", "received_at"}
}
func (*ackReadFaultRows) Close() error { return nil }
func (r *ackReadFaultRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	copy(dest, r.values)
	return nil
}
func TestSQLiteSnapshotAcknowledgementReadFaultsReturnZero(t *testing.T) {
	for _, fault := range []struct {
		name  string
		index int
		value driver.Value
	}{
		{"late scan", 5, "not an integer"}, {"applied NULL", 6, nil}, {"received NULL", 7, nil}, {"applied decode", 6, "malformed"}, {"received decode", 7, "malformed"},
	} {
		t.Run(fault.name, func(t *testing.T) {
			values := []driver.Value{ackReadNode, ackReadHash, int64(7), int64(9), ackReadRevision, int64(11), ackReadStamp(t, -1, 123456789), ackReadStamp(t, 1, 987654321)}
			values[fault.index] = fault.value
			db := sql.OpenDB(ackReadFaultConnector{values})
			defer db.Close()
			ackReadError(t, NewSQLiteSnapshotAcknowledgementReadStore(db), context.Background(), ackReadNode, ackReadHash, ErrSnapshotAcknowledgementUnavailable)
		})
	}
}
