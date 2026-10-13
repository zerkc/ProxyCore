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

func TestSQLiteWorkerJobStoreFixture(t *testing.T) {
	if testing.Short() {
		t.Skip("external Bun integration fixture")
	}
	bun, err := exec.LookPath("bun")
	if err != nil {
		t.Fatal("required Bun executable:", err)
	}
	bun, err = filepath.Abs(bun)
	if err != nil {
		t.Fatal(err)
	}
	db, path := enqueueFixture(t)
	job := enqueueCall(t, db, "").Job
	before := enqueueRevisionBytes(t, db)
	_, source, _, _ := runtime.Caller(0)
	runner := filepath.Clean(filepath.Join(filepath.Dir(source), "../../../../packages/db/src/sqlite-foundation/job-store.bun.ts"))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bun, "--no-env-file", runner, path, job.ID, job.RevisionID)
	cmd.Dir = t.TempDir()
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Go-migrated Bun JobStore: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "COMPLETE job store cases=") {
		t.Fatalf("missing completed Bun coverage: %s", out)
	}
	t.Logf("embedded Bun cases:\n%s", out)
	if got := enqueueRevisionBytes(t, db); got != before {
		t.Fatal("JobStore changed revision bytes")
	}
	reopened, err := sqlitedb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	var count int
	if err := reopened.QueryRow("SELECT count(*) FROM apply_jobs").Scan(&count); err != nil || count != 2 {
		t.Fatal("original plus durable store job:", count, err)
	}
	var status, target, correlation string
	if err := reopened.QueryRow("SELECT status,target,correlation_id FROM apply_jobs WHERE id != ?", job.ID).Scan(&status, &target, &correlation); err != nil {
		t.Fatal(err)
	}
	if status != "applied" || target != "certificate" || correlation != "store-durable" {
		t.Fatal("durable lifecycle mismatch:", status, target, correlation)
	}
}
