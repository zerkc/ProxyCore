package sqlitedb

import (
	"bytes"
	"context"
	"math"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestBunSharedFile(t *testing.T) {
	bun, err := exec.LookPath("bun")
	if err != nil {
		t.Fatal("Bun is required for shared-file acceptance:", err)
	}
	_, source, _, _ := runtime.Caller(0)
	script := filepath.Clean(filepath.Join(filepath.Dir(source), "../../../..", "packages/db/src/sqlite-foundation/interop.bun.ts"))
	path := filepath.Join(t.TempDir(), "shared ?#%&.sqlite")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE sample (
		origin TEXT NOT NULL, value INTEGER NOT NULL, snapshot BLOB NOT NULL,
		label TEXT NOT NULL, optional TEXT, PRIMARY KEY (origin, value)
	); CREATE TABLE child (origin TEXT, value INTEGER,
		FOREIGN KEY (origin, value) REFERENCES sample(origin, value))`); err != nil {
		t.Fatal(err)
	}
	values := []int64{9007199254740993, math.MaxInt64, math.MinInt64}
	snapshot := []byte("{\"format\":1,\"payload\":\"exact\"}\n")
	blob := append(append([]byte{}, snapshot...), 0, 255, 128)
	label := "shared café 🛰"
	for _, value := range values {
		if _, err := db.Exec("INSERT INTO sample VALUES (?, ?, ?, ?, ?)", "go", value, blob, label, nil); err != nil {
			t.Fatal(err)
		}
	}
	// Explicitly disable Bun's automatic .env loading; execute outside the repository.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bun, "--no-env-file", script, path)
	cmd.Dir = filepath.Dir(path)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Bun boundary failed: %v\n%s", err, output)
	}
	for _, value := range values {
		var got int64
		var gotBlob []byte
		var gotLabel string
		var optional *string
		if err := db.QueryRow("SELECT value, snapshot, label, optional FROM sample WHERE origin = 'bun' AND value = ?", value).Scan(&got, &gotBlob, &gotLabel, &optional); err != nil {
			t.Fatal(err)
		}
		if got != value || !bytes.Equal(gotBlob, blob) || gotLabel != label || optional != nil {
			t.Fatalf("Bun → Go mismatch for %d: %d %x %q %v", value, got, gotBlob, gotLabel, optional)
		}
	}
	var count int
	if err := db.QueryRow("SELECT count(*) FROM sample").Scan(&count); err != nil || count != 6 {
		t.Fatalf("Bun rollback was not preserved: count=%d err=%v", count, err)
	}
}
