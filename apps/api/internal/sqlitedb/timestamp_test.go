package sqlitedb

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"testing"
	"time"
)

type timestampVector struct {
	Kind    string  `json:"kind"`
	Seconds string  `json:"seconds"`
	Nanos   int32   `json:"nanos"`
	Key     *string `json:"key"`
}

func fixtureSeconds(raw string) (int64, error) {
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || strconv.FormatInt(n, 10) != raw {
		return 0, fmt.Errorf("noncanonical int64 seconds: %q", raw)
	}
	return n, nil
}

func TestTimestampContract(t *testing.T) {
	_, source, _, _ := runtime.Caller(0)
	dir := filepath.Dir(source)
	fixture := filepath.Join(dir, "testdata/timestamp-vectors.json")
	data, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	var cases struct {
		Vectors        []timestampVector `json:"vectors"`
		InvalidKeys    []string          `json:"invalidKeys"`
		InvalidSeconds []string          `json:"invalidSeconds"`
		InvalidNanos   []int32           `json:"invalidNanos"`
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	keys := make([]*string, 0, len(cases.Vectors))
	for _, v := range cases.Vectors {
		t.Run(v.Kind+":"+v.Seconds+":"+strconv.Itoa(int(v.Nanos)), func(t *testing.T) {
			var value Timestamp
			switch v.Kind {
			case "null":
				value = NullTimestamp{}
			case "negative-infinity":
				value = NegativeInfinity{}
			case "positive-infinity":
				value = PositiveInfinity{}
			case "finite":
				seconds, err := fixtureSeconds(v.Seconds)
				if err != nil {
					t.Fatal(err)
				}
				value = FiniteTimestamp{Seconds: seconds, Nanos: v.Nanos}
			default:
				t.Fatalf("unknown fixture tag %q", v.Kind)
			}
			key, err := EncodeTimestamp(value)
			if err != nil || !reflect.DeepEqual(key, v.Key) {
				t.Fatalf("encode: got %v, error %v, want %v", key, err, v.Key)
			}
			decoded, err := DecodeTimestamp(v.Key)
			if err != nil || !reflect.DeepEqual(decoded, value) {
				t.Fatalf("decode: got %#v, error %v, want %#v", decoded, err, value)
			}
		})
		keys = append(keys, v.Key)
	}
	for _, key := range cases.InvalidKeys {
		if _, err := DecodeTimestamp(&key); err == nil {
			t.Errorf("accepted malformed key %q", key)
		}
	}
	for _, raw := range cases.InvalidSeconds {
		if _, err := fixtureSeconds(raw); err == nil {
			t.Errorf("accepted malformed fixture seconds %q", raw)
		}
	}
	for _, nanos := range cases.InvalidNanos {
		if _, err := EncodeTimestamp(FiniteTimestamp{Nanos: nanos}); err == nil {
			t.Errorf("accepted nanos %d", nanos)
		}
	}
	if _, err := EncodeTimestamp(nil); err == nil {
		t.Fatal("nil interface must not silently encode SQL NULL")
	}
	dbPath := filepath.Join(t.TempDir(), "timestamps.sqlite")
	db, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("CREATE TABLE timestamps (origin TEXT NOT NULL, key TEXT COLLATE BINARY)"); err != nil {
		t.Fatal(err)
	}
	for i := len(keys) - 1; i >= 0; i-- {
		if _, err := db.Exec("INSERT INTO timestamps VALUES ('go', ?)", keys[i]); err != nil {
			t.Fatal(err)
		}
	}
	assertTimestampOrder(t, db, "go", keys)
	bun, err := exec.LookPath("bun")
	if err != nil {
		t.Fatal("Bun required:", err)
	}
	script := filepath.Join(dir, "../../../..", "packages/db/src/sqlite-foundation/timestamp-vectors.bun.ts")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bun, "--no-env-file", filepath.Clean(script), fixture, dbPath)
	cmd.Dir = filepath.Dir(dbPath)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Bun timestamp boundary: %v\n%s", err, output)
	}
	assertTimestampOrder(t, db, "bun", keys)
}

func assertTimestampOrder(t *testing.T, db *sql.DB, origin string, keys []*string) {
	t.Helper()
	var nulls int
	if err := db.QueryRow("SELECT count(*) FROM timestamps WHERE origin = ? AND key IS NULL", origin).Scan(&nulls); err != nil || nulls != 1 {
		t.Fatalf("explicit NULL count: %d, error %v", nulls, err)
	}
	rows, err := db.Query("SELECT key FROM timestamps WHERE origin = ? AND key IS NOT NULL ORDER BY key COLLATE BINARY", origin)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for _, key := range keys[1:] {
		var got string
		if !rows.Next() || rows.Scan(&got) != nil || got != *key {
			t.Fatalf("BINARY ordering: got %q, want %q", got, *key)
		}
	}
	if rows.Next() || rows.Err() != nil {
		t.Fatalf("unexpected rows or error: %v", rows.Err())
	}
	for i, key := range keys[1:] {
		var count int
		if err := db.QueryRow("SELECT count(*) FROM timestamps WHERE origin = ? AND key < ? COLLATE BINARY", origin, *key).Scan(&count); err != nil || count != i {
			t.Fatalf("bound cutoff %q: count %d, want %d, error %v", *key, count, i, err)
		}
	}
}
