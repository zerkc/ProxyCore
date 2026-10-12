package hostread

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

const (
	revA = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	revB = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	revC = "cccccccc-cccc-cccc-cccc-cccccccccccc"
	// Canonical finite clock keys: '1' + 20 digits + '.' + 9 digits.
	clockEarly = "100000000000000000100.000000000"
	clockLate  = "100000000000000000200.000000000"
)

func openSQLiteHostreadDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sqlitedb.Open(filepath.Join(t.TempDir(), "hostread.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := sqlitedb.Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	return db
}

// insertRevision stores a synthetic revision; appliedAt nil stores SQL NULL.
func insertRevision(t *testing.T, db *sql.DB, id string, number int, appliedAt any) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO config_revisions(id,revision_number,checksum,snapshot,created_at,applied_at)
		VALUES(?,?,?,'{}','0',?)`, id, number, "fixture-only-"+id, appliedAt); err != nil {
		t.Fatal(err)
	}
}

func TestSQLiteReaderImplementsHostSnapshotReader(t *testing.T) {
	var _ HostSnapshotReader = NewSQLite(nil, Config{})
}

func TestSQLiteAppliedRevision(t *testing.T) {
	tests := []struct {
		name string
		rows []struct {
			id        string
			appliedAt any
		}
		want    string
		wantErr error
	}{
		{name: "empty table", wantErr: ErrNoAppliedRevision},
		{name: "only null applied_at", rows: []struct {
			id        string
			appliedAt any
		}{{revA, nil}, {revB, nil}}, wantErr: ErrNoAppliedRevision},
		{name: "latest applied_at wins over later id", rows: []struct {
			id        string
			appliedAt any
		}{{revA, clockLate}, {revB, clockEarly}, {revC, nil}}, want: revA},
		{name: "tie broken by id desc", rows: []struct {
			id        string
			appliedAt any
		}{{revB, clockLate}, {revC, clockLate}, {revA, clockLate}}, want: revC},
		{name: "positive infinity beats finite", rows: []struct {
			id        string
			appliedAt any
		}{{revA, "2"}, {revB, clockLate}}, want: revA},
		{name: "finite beats negative infinity", rows: []struct {
			id        string
			appliedAt any
		}{{revA, clockEarly}, {revB, "0"}}, want: revA},
		{name: "negative infinity still counts as applied", rows: []struct {
			id        string
			appliedAt any
		}{{revA, "0"}, {revB, nil}}, want: revA},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := openSQLiteHostreadDB(t)
			for i, row := range tt.rows {
				insertRevision(t, db, row.id, i+1, row.appliedAt)
			}
			got, err := NewSQLite(db, Config{}).AppliedRevision(context.Background())
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("revision = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSQLiteAppliedRevisionPropagatesContextAndDatabaseErrors(t *testing.T) {
	db := openSQLiteHostreadDB(t)
	insertRevision(t, db, revA, 1, clockLate)

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewSQLite(db, Config{}).AppliedRevision(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled ctx error = %v, want context.Canceled", err)
	}

	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	_, err := NewSQLite(db, Config{}).AppliedRevision(context.Background())
	if err == nil || errors.Is(err, ErrNoAppliedRevision) {
		t.Fatalf("closed db error = %v, want a non-ErrNoAppliedRevision error", err)
	}
}

func TestSQLiteAppliedRevisionRejectsInvalidReceiver(t *testing.T) {
	var nilReader *sqliteReader
	if _, err := nilReader.AppliedRevision(context.Background()); err == nil {
		t.Fatal("nil reader: want error")
	}
	if _, err := NewSQLite(nil, Config{}).AppliedRevision(context.Background()); err == nil {
		t.Fatal("nil db: want error")
	}
	if _, err := NewSQLite(openSQLiteHostreadDB(t), Config{}).AppliedRevision(nil); err == nil { //nolint:staticcheck // nil ctx is the case under test
		t.Fatal("nil ctx: want error")
	}
}

// Filesystem-only behavior must be identical for both readers.
func TestFilesystemReadsMatchAcrossReaders(t *testing.T) {
	constructors := map[string]func(Config) HostSnapshotReader{
		"sqlite": func(cfg Config) HostSnapshotReader { return NewSQLite(nil, cfg) },
	}
	for name, build := range constructors {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			envPath := filepath.Join(dir, ".env")
			contents := []byte("SYNTHETIC=value\n")
			if err := os.WriteFile(envPath, contents, 0o600); err != nil {
				t.Fatal(err)
			}
			root := filepath.Join(dir, "candidates")
			certDir := filepath.Join(root, "rev-1", "nginx", "certs")
			if err := os.MkdirAll(certDir, 0o755); err != nil {
				t.Fatal(err)
			}
			for _, f := range []string{"a.CRT", "b.key", "skip.txt"} {
				if err := os.WriteFile(filepath.Join(certDir, f), []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			r := build(Config{EnvPath: envPath, CandidateRoot: root})

			env, err := r.EnvFile(context.Background())
			if err != nil || env.Path != "env/env" || env.Size != int64(len(contents)) {
				t.Fatalf("EnvFile = %+v, %v", env, err)
			}
			rc, err := env.Open()
			if err != nil {
				t.Fatal(err)
			}
			got, _ := io.ReadAll(rc)
			_ = rc.Close()
			if string(got) != string(contents) {
				t.Fatalf("env contents = %q", got)
			}

			var pathErr *os.PathError
			if _, err := build(Config{EnvPath: filepath.Join(dir, "missing")}).EnvFile(context.Background()); !errors.As(err, &pathErr) {
				t.Fatalf("missing env error = %v, want *os.PathError", err)
			}

			files, err := r.AppliedCertFiles(context.Background(), "rev-1")
			if err != nil {
				t.Fatal(err)
			}
			paths := map[string]bool{}
			for _, f := range files {
				paths[f.Path] = true
			}
			if len(files) != 2 || !paths["certs/a.CRT"] || !paths["certs/b.key"] {
				t.Fatalf("cert files = %v", paths)
			}

			if none, err := r.AppliedCertFiles(context.Background(), "absent"); err != nil || len(none) != 0 {
				t.Fatalf("absent revision = %v, %v; want empty, nil", none, err)
			}
			for _, bad := range []string{"../escape", `a\b`, "/abs"} {
				if _, err := r.AppliedCertFiles(context.Background(), bad); err == nil {
					t.Fatalf("revision %q: want escape error", bad)
				}
			}
		})
	}
}
