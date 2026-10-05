package sqlitedb

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sync"
	"testing"
	"time"
)

func writeOK(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func writeDB(t *testing.T) (*Writer, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "write.sqlite")
	db, err := Open(path)
	writeOK(t, err)
	t.Cleanup(func() { db.Close() })
	_, err = db.Exec("CREATE TABLE writes(origin TEXT, stamp TEXT, id TEXT)")
	writeOK(t, err)
	return NewWriter(db, nil), path
}

func TestWriteValues(t *testing.T) {
	w, _ := writeDB(t)
	calls := 0
	w = NewWriter(w.db, func() FiniteTimestamp {
		calls++
		return FiniteTimestamp{Seconds: int64(calls), Nanos: 123}
	})
	pattern := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	for i := 0; i < 2; i++ {
		writeOK(t, w.Write(context.Background(), func(v WriteValue) error {
			if err := NewWriter(w.db, nil).Write(v.Context(), func(WriteValue) error {
				t.Fatal("nested callback")
				return nil
			}); err == nil {
				t.Fatal("nested accepted")
			}
			for j := 0; j < 2; j++ {
				id, err := v.NewUUID()
				writeOK(t, err)
				if !pattern.MatchString(id) {
					t.Fatalf("UUID %q", id)
				}
				_, err = v.Conn().ExecContext(v.Context(), "INSERT INTO writes VALUES ('go', ?, ?)", v.Timestamp(), id)
				writeOK(t, err)
			}
			return nil
		}))
	}
	var rows, stamps, ids int
	writeOK(t, w.db.QueryRow("SELECT count(*), count(DISTINCT stamp), count(DISTINCT id) FROM writes").Scan(&rows, &stamps, &ids))
	if calls != 2 || rows != 4 || stamps != 2 || ids != 4 {
		t.Fatalf("capture/commit: %d %d %d %d", calls, rows, stamps, ids)
	}
}

func TestWriteRollback(t *testing.T) {
	for _, scenario := range []string{"error", "panic", "cancel"} {
		t.Run(scenario, func(t *testing.T) {
			w, _ := writeDB(t)
			w.db.SetMaxOpenConns(1)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			sentinel := errors.New("callback")
			func() {
				defer func() {
					p := recover()
					if scenario == "panic" && p != sentinel || scenario != "panic" && p != nil {
						t.Fatalf("panic lost: %v", p)
					}
				}()
				err := w.Write(ctx, func(v WriteValue) error {
					_, err := v.Conn().ExecContext(v.Context(), "INSERT INTO writes VALUES ('bad', ?, 'id')", v.Timestamp())
					writeOK(t, err)
					switch scenario {
					case "panic":
						panic(sentinel)
					case "cancel":
						cancel()
						return nil
					default:
						return sentinel
					}
				})
				if scenario == "error" && !errors.Is(err, sentinel) || scenario == "cancel" && !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			}()
			ctx, done := context.WithTimeout(context.Background(), 10*time.Second)
			defer done()
			var count int
			writeOK(t, w.db.QueryRowContext(ctx, "SELECT count(*) FROM writes").Scan(&count))
			if count != 0 {
				t.Fatalf("rollback: %d", count)
			}
			writeOK(t, w.Write(ctx, func(WriteValue) error { return nil }))
		})
	}
	w, _ := writeDB(t)
	bad := NewWriter(w.db, func() FiniteTimestamp { return FiniteTimestamp{Nanos: -1} })
	if err := bad.Write(context.Background(), func(WriteValue) error {
		t.Fatal("invalid clock callback")
		return nil
	}); err == nil {
		t.Fatal("invalid clock accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := w.Write(ctx, func(WriteValue) error {
		t.Fatal("cancelled entry callback")
		return nil
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled entry: %v", err)
	}
	writeOK(t, w.Write(context.Background(), func(WriteValue) error { return nil }))
}

func TestWriteIsolatedClocks(t *testing.T) {
	a, _ := writeDB(t)
	b, _ := writeDB(t)
	var wg sync.WaitGroup
	for i, w := range []*Writer{a, b} {
		wg.Add(1)
		go func(seconds int64, w *Writer) {
			defer wg.Done()
			w = NewWriter(w.db, func() FiniteTimestamp { return FiniteTimestamp{Seconds: seconds} })
			err := w.Write(context.Background(), func(v WriteValue) error {
				want, _ := EncodeTimestamp(FiniteTimestamp{Seconds: seconds})
				if v.Timestamp() != *want {
					t.Error("clock leaked")
				}
				_, err := v.Conn().ExecContext(v.Context(), "INSERT INTO writes VALUES ('isolated', ?, 'id')", v.Timestamp())
				return err
			})
			if err != nil {
				t.Error(err)
			}
		}(int64(i+1), w)
	}
	wg.Wait()
}

func TestWriteBunSharedFile(t *testing.T) {
	if testing.Short() {
		t.Skip("Bun subprocess integration")
	}
	bun, err := exec.LookPath("bun")
	writeOK(t, err) // Missing Bun fails acceptance, never silently skips.
	bun, err = filepath.Abs(bun)
	writeOK(t, err)
	w, path := writeDB(t)
	w = NewWriter(w.db, func() FiniteTimestamp { return FiniteTimestamp{Seconds: -1, Nanos: 999999999} })
	writeOK(t, w.Write(context.Background(), func(v WriteValue) error {
		_, err := v.Conn().ExecContext(v.Context(), "INSERT INTO writes VALUES ('go', ?, 'fixture')", v.Timestamp())
		return err
	}))
	_, source, _, _ := runtime.Caller(0)
	script := filepath.Clean(filepath.Join(filepath.Dir(source), "../../../..", "packages/db/src/sqlite-foundation/write.bun.ts"))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bun, "--no-env-file", script, path)
	cmd.Dir = filepath.Dir(path)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Bun: %v\n%s", err, output)
	}
	var matches int
	writeOK(t, w.db.QueryRow("SELECT count(*) FROM writes a JOIN writes b ON a.stamp=b.stamp WHERE a.origin='go' AND b.origin='bun'").Scan(&matches))
	if matches != 2 {
		t.Fatalf("shared timestamps: %d", matches)
	}
}
