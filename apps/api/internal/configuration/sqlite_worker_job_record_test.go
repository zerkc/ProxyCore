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

func TestSQLiteWorkerJobRecordFixture(t *testing.T) {
	db, path := enqueueFixture(t)
	job := enqueueCall(t, db, "").Job
	before := enqueueRevisionBytes(t, db)
	const rawJob = `SELECT json_array(id,revision_id,actor_user_id,target,status,source,
		source_primary_id,source_node_id,source_revision_id,snapshot_content_hash,
		snapshot_version,replication_version,leadership_generation,correlation_id,
		created_at,claimed_at,started_at,finished_at,validation_output,apply_output,
		health_output,error_message) FROM apply_jobs WHERE id=?`
	var rawBefore string
	if err := db.QueryRow(rawJob, job.ID).Scan(&rawBefore); err != nil {
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
	runner := filepath.Clean(filepath.Join(filepath.Dir(source), "../../../../packages/db/src/sqlite-foundation/job-record.bun.ts"))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bun, "--no-env-file", runner, path, job.ID, job.RevisionID, job.CorrelationID)
	cmd.Dir = t.TempDir()
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("real Go-migrated Bun public job fixture: %v\n%s", err, out)
	}
	t.Logf("embedded Bun cases:\n%s", out)
	if got := enqueueRevisionBytes(t, db); got != before {
		t.Fatal("public reader changed revision bytes")
	}
	reopened, err := sqlitedb.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	var rawAfter string
	if err := reopened.QueryRow(rawJob, job.ID).Scan(&rawAfter); err != nil || rawAfter != rawBefore {
		t.Fatal("committed original Go job changed:", err, rawAfter)
	}
	var count int
	if err := reopened.QueryRow("SELECT count(*) FROM apply_jobs").Scan(&count); err != nil || count != 1 {
		t.Fatal("fixture must preserve only the original Go job:", count, err)
	}
}
