package configuration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Production modules and the canonical snapshot helper are copied byte-for-byte
// into an isolated runtime tree. Type-only imports disappear at Bun execution.
func revisionStoreCommand(t *testing.T, args ...string) (*exec.Cmd, context.CancelFunc) {
	t.Helper()
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
	_, source, _, _ := runtime.Caller(0)
	root := filepath.Clean(filepath.Join(filepath.Dir(source), "../../../.."))
	temp := t.TempDir()
	modules := []string{"packages/domain/src/snapshot.ts"}
	for _, name := range []string{"open", "timestamp", "write", "revision-record", "revision-latest-record",
		"revision-create-record", "revision-mark-applied-record", "revision-store", "revision-store.bun"} {
		modules = append(modules, "packages/db/src/sqlite-foundation/"+name+".ts")
	}
	for _, name := range modules {
		data, err := os.ReadFile(filepath.Join(root, name))
		// RED reaches the missing factory at Bun's import boundary.
		if os.IsNotExist(err) && strings.HasSuffix(name, "/revision-store.ts") {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(temp, name)
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, data, 0600); err != nil {
			t.Fatal(err)
		}
		copied, err := os.ReadFile(target)
		if err != nil || !bytes.Equal(data, copied) {
			t.Fatal("production module copy differs:", name, err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	runner := filepath.Join(temp, "packages/db/src/sqlite-foundation/revision-store.bun.ts")
	cmd := exec.CommandContext(ctx, bun, append([]string{"--no-env-file", runner}, args...)...)
	cmd.Dir = temp
	cmd.WaitDelay = time.Second
	return cmd, cancel
}

func revisionStoreRun(t *testing.T, args ...string) string {
	t.Helper()
	cmd, cancel := revisionStoreCommand(t, args...)
	defer cancel()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Go-migrated Bun revision store: %v\n%s", err, out)
	}
	t.Logf("embedded Bun coverage:\n%s", out)
	return string(out)
}

func TestSQLiteWorkerRevisionStoreFixture(t *testing.T) {
	db, path := enqueueFixture(t)
	actor := revisionCreateActor(t, db)
	if _, err := db.Exec(`UPDATE installation_settings SET current_desired_revision_id='desired-sentinel',
		current_applied_revision_id='applied-sentinel',ingress_ipv4='192.0.2.99',retention_max_age_days=19`); err != nil {
		t.Fatal("settings fixture:", err)
	}
	out := revisionStoreRun(t, "unit", path, actor)
	if !strings.Contains(out, "COMPLETE store assertions=") {
		t.Fatal("incomplete Bun composition coverage:", out)
	}
	var count int
	if err := db.QueryRow("SELECT count(*) FROM config_revisions").Scan(&count); err != nil || count != 0 {
		t.Fatal("revision cleanup:", count, err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM installation_settings
		WHERE current_desired_revision_id='desired-sentinel' AND current_applied_revision_id='applied-sentinel'
		AND ingress_ipv4='192.0.2.99' AND retention_max_age_days=19`).Scan(&count); err != nil || count != 1 {
		t.Fatal("settings restoration:", count, err)
	}
}

func TestSQLiteWorkerRevisionStoreDurableReopen(t *testing.T) {
	db, path := enqueueFixture(t)
	actor := revisionCreateActor(t, db)
	out := revisionStoreRun(t, "persist", path, actor)
	if !strings.Contains(out, "PERSIST ") {
		t.Fatal("missing durable publication:", out)
	}
	var id, checksum, snapshot, storedActor, applied, pointer string
	var number int
	if err := db.QueryRow(`SELECT id,revision_number,checksum,snapshot,actor_user_id,applied_at
		FROM config_revisions`).Scan(&id, &number, &checksum, &snapshot, &storedActor, &applied); err != nil {
		t.Fatal(err)
	}
	canonical := `{"a":[3,null],"z":1}`
	if number != 1 || snapshot != canonical || checksum != fmt.Sprintf("%x", sha256.Sum256([]byte(canonical))) || storedActor != actor {
		t.Fatal("independent native revision fields:", number, snapshot, checksum, storedActor)
	}
	if applied != "109223372036854775808.123000000" {
		t.Fatal("independent native applied timestamp:", applied)
	}
	if err := db.QueryRow("SELECT current_applied_revision_id FROM installation_settings").Scan(&pointer); err != nil || pointer != id {
		t.Fatal("durable applied pointer:", pointer, err)
	}
	before := enqueueRevisionBytes(t, db)
	if out := revisionStoreRun(t, "reopen", path, actor); !strings.Contains(out, "COMPLETE reopen assertions=") {
		t.Fatal("missing independent process reopen:", out)
	}
	if after := enqueueRevisionBytes(t, db); after != before {
		t.Fatal("native revision bytes changed across reader process", before, after)
	}
}
