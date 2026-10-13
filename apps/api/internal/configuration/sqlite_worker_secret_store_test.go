package configuration

import (
	"context"
	"encoding/base64"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/secrets"
	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

func TestSQLiteWorkerSecretStoreFixture(t *testing.T) {
	if testing.Short() {
		t.Skip("external Bun fixture")
	}
	ctx := context.Background()
	db, err := sqlitedb.Open(filepath.Join(t.TempDir(), "secret-store.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := sqlitedb.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	key := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32)))
	store := secrets.NewSQLiteStore(db, key)
	const goPlain = "synthetic-go-written-secret 雪"
	goID, err := store.Put(ctx, "go-purpose", goPlain)
	if err != nil {
		t.Fatal(err)
	}
	bun, err := exec.LookPath("bun")
	if err != nil {
		t.Fatal("required Bun executable:", err)
	}
	bun, err = filepath.Abs(bun)
	if err != nil {
		t.Fatal(err)
	}
	_, source, _, _ := runtime.Caller(0)
	runner := filepath.Clean(filepath.Join(filepath.Dir(source), "../../../../packages/db/src/sqlite-foundation/secret-store.bun.ts"))
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	path := filepath.Join(t.TempDir(), "unused")
	_ = path
	var dbPath string
	if err := db.QueryRowContext(ctx, "select file from pragma_database_list where name='main'").Scan(&dbPath); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(cctx, bun, "--no-env-file", runner, dbPath, key, goID, goPlain)
	cmd.Dir = t.TempDir()
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Bun secret store fixture: %v\n%s", err, out)
	}
	t.Logf("embedded Bun cases:\n%s", out)
	var tsID, tsPlain string
	for _, line := range strings.Split(string(out), "\n") {
		if rest, ok := strings.CutPrefix(line, "TSWRITTEN "); ok {
			parts := strings.SplitN(rest, " ", 2)
			if len(parts) == 2 {
				tsID, tsPlain = parts[0], parts[1]
			}
		}
	}
	if tsID == "" {
		t.Fatalf("missing TSWRITTEN line:\n%s", out)
	}
	got, err := store.Get(ctx, tsID)
	if err != nil || got != tsPlain {
		t.Fatalf("Go Get of TS-written secret = (%q, %v), want %q", got, err, tsPlain)
	}
	if got, err := store.Get(ctx, goID); err != nil || got != goPlain {
		t.Fatalf("Go secret changed: (%q, %v)", got, err)
	}
}
