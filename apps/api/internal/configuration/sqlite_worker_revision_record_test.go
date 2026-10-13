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

func TestSQLiteWorkerRevisionRecordFixture(t *testing.T) {
	db, path := enqueueFixture(t)
	first := enqueueCall(t, db, "")
	before := enqueueRevisionBytes(t, db)
	second := enqueueCall(t, db, "")
	if first.RevisionID != second.RevisionID || before != enqueueRevisionBytes(t, db) || first.Job.ID == second.Job.ID || first.Job.CorrelationID == second.Job.CorrelationID {
		t.Fatal("checksum reuse must preserve revision and create fresh job")
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
	runner := filepath.Clean(filepath.Join(filepath.Dir(source), "../../../../packages/db/src/sqlite-foundation/revision-record.bun.ts"))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bun, "--no-env-file", runner, path, first.RevisionID)
	cmd.Dir = t.TempDir()
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("real Go-migrated Bun public revision fixture: %v\n%s", err, out)
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
	var count int
	if err := reopened.QueryRow("SELECT count(*) FROM apply_jobs").Scan(&count); err != nil || count != 2 {
		t.Fatal("fixture must preserve both Go jobs:", count, err)
	}
	var raw string
	if err := reopened.QueryRow(`SELECT json_array(id,revision_number,checksum,snapshot,actor_user_id,
		created_at,applied_at,source,source_primary_id,source_node_id,source_revision_id,
		snapshot_content_hash,snapshot_version,replication_version,leadership_generation) FROM config_revisions`).Scan(&raw); err != nil || raw != before {
		t.Fatal("reopened revision changed:", err, raw)
	}
}
