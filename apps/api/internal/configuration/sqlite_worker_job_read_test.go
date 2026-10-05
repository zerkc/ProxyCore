package configuration

import (
	"context"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

func TestSQLiteWorkerJobReadFixture(t *testing.T) {
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
	runner := filepath.Clean(filepath.Join(filepath.Dir(source), "../../../../packages/db/src/sqlite-foundation/job-read.bun.ts"))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bun, "--no-env-file", runner, path, job.ID, job.RevisionID, job.CorrelationID)
	cmd.Dir = t.TempDir()
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("real Go-migrated Bun job reader fixture: %v\n%s", err, out)
	}
	t.Logf("embedded Bun cases:\n%s", out)
	if got := enqueueRevisionBytes(t, db); got != before {
		t.Fatal("reader fixture changed revision bytes")
	}
	var status, revision, correlation string
	if err := db.QueryRow("SELECT status,revision_id,correlation_id FROM apply_jobs WHERE id=?", job.ID).
		Scan(&status, &revision, &correlation); err != nil {
		t.Fatal(err)
	}
	if status != "queued" || revision != job.RevisionID || correlation != job.CorrelationID {
		t.Fatal("reader fixture did not preserve original Go job")
	}
	reopened, err := sqlitedb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	var count int
	if err := reopened.QueryRow("SELECT count(*) FROM apply_jobs").Scan(&count); err != nil || count != 1 {
		t.Fatal("reopened fixture must contain only original job:", count, err)
	}
}
