package configuration

import (
	"context"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

func TestSQLiteWorkerJobUpdateRecordFixture(t *testing.T) {
	if testing.Short() {
		t.Skip("external Bun integration fixture")
	}
	db, path := enqueueFixture(t)
	job := enqueueCall(t, db, "").Job
	before := enqueueRevisionBytes(t, db)
	bun, err := exec.LookPath("bun")
	if err != nil {
		t.Fatal("required Bun executable:", err)
	}
	bun, err = filepath.Abs(bun)
	if err != nil {
		t.Fatal(err)
	}
	_, source, _, _ := runtime.Caller(0)
	runner := filepath.Clean(filepath.Join(filepath.Dir(source), "../../../../packages/db/src/sqlite-foundation/job-update-record.bun.ts"))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bun, "--no-env-file", runner, path, job.ID)
	cmd.Dir = t.TempDir()
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Go-migrated public Bun update: %v\n%s", err, out)
	}
	t.Logf("embedded Bun cases (%d):\n%s", strings.Count(string(out), "PASS "), out)
	if got := enqueueRevisionBytes(t, db); got != before {
		t.Fatal("public update changed revision bytes")
	}
	reopened, err := sqlitedb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	claimDurable(t, reopened, job.ID, "applied")
}
