package configuration

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

// Copy real production modules unchanged. Relative domain imports resolve to the
// canonical snapshot module, not an alias, substitute algorithm or runtime hook.
func revisionCreateCommand(t *testing.T, args ...string) (*exec.Cmd, context.CancelFunc) {
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
	for _, name := range []string{"open", "timestamp", "write", "revision-record", "revision-latest-record", "revision-create-record", "revision-create-record.bun"} {
		modules = append(modules, "packages/db/src/sqlite-foundation/"+name+".ts")
	}
	for _, name := range modules {
		data, err := os.ReadFile(filepath.Join(root, name))
		// RED must reach Bun's missing production module, not fail in copying.
		if os.IsNotExist(err) && strings.HasSuffix(name, "/revision-create-record.ts") {
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
			t.Fatal("module copy differs:", name, err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	runner := filepath.Join(temp, "packages/db/src/sqlite-foundation/revision-create-record.bun.ts")
	cmd := exec.CommandContext(ctx, bun, append([]string{"--no-env-file", runner}, args...)...)
	cmd.Dir = temp
	cmd.WaitDelay = time.Second
	return cmd, cancel
}

func revisionCreateActor(t *testing.T, db *sql.DB) string {
	t.Helper()
	actor := "aaaaaaaa-aaaa-aaaa-aaaa-000000000001"
	// Explicit native timestamps: PG column defaults are not SQLite defaults.
	_, err := db.Exec(`INSERT INTO users
		(id,username,password_hash,role,active,password_change_required,created_at,updated_at)
		VALUES (?,'create-fixture','noncredential-fixture','owner',1,0,'0','0')`, actor)
	if err != nil {
		t.Fatal("valid actor fixture:", err)
	}
	return actor
}

func TestSQLiteWorkerRevisionCreateRecordFixture(t *testing.T) {
	db, path := enqueueFixture(t)
	actor := revisionCreateActor(t, db)
	if _, err := db.Exec(`UPDATE installation_settings SET current_desired_revision_id='desired-sentinel',
		current_applied_revision_id='applied-sentinel',ingress_ipv4='192.0.2.99',retention_max_age_days=19`); err != nil {
		t.Fatal(err)
	}
	cmd, cancel := revisionCreateCommand(t, "unit", path, actor)
	defer cancel()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Go-migrated Bun create fixture: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "COMPLETE create assertions=") {
		t.Fatalf("missing complete coverage: %s", out)
	}
	t.Logf("embedded Bun coverage:\n%s", out)
	var count int
	if err := db.QueryRow("SELECT count(*) FROM config_revisions").Scan(&count); err != nil || count != 0 {
		t.Fatal("fixture cleanup:", count, err)
	}
	var desired, applied string
	if err := db.QueryRow("SELECT current_desired_revision_id,current_applied_revision_id FROM installation_settings").Scan(&desired, &applied); err != nil || desired != "desired-sentinel" || applied != "applied-sentinel" {
		t.Fatal("settings pointers:", err, desired, applied)
	}
}

func revisionCreateStart(t *testing.T, path, actor string) *claimChild {
	t.Helper()
	cmd, cancel := revisionCreateCommand(t, "child", path, actor)
	input, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Cancel = func() error { _, err := io.WriteString(input, "exit\n"); return err }
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	lines := make(chan string, 32)
	finished := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(output)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
		close(lines)
		err := scanner.Err()
		if waitErr := cmd.Wait(); err == nil {
			err = waitErr
		}
		finished <- err
	}()
	t.Cleanup(func() {
		_, _ = io.WriteString(input, "exit\n")
		timer := time.NewTimer(2 * time.Second)
		defer timer.Stop()
		select {
		case err := <-finished:
			if err != nil {
				t.Errorf("create child: %v\n%s", err, stderr.String())
			}
		case <-timer.C:
			cancel()
			t.Errorf("create child cleanup deadline: %v", <-finished)
		}
		cancel()
		_ = input.Close()
		_ = output.Close()
	})
	child := &claimChild{input: input, lines: lines}
	if got := child.read(t); got != "READY" {
		t.Fatal("independent handshake:", got)
	}
	return child
}

func TestSQLiteWorkerRevisionCreateRecordIndependentProcesses(t *testing.T) {
	for _, same := range []bool{true, false} {
		t.Run(map[bool]string{true: "same", false: "distinct"}[same], func(t *testing.T) {
			db, path := enqueueFixture(t)
			actor := revisionCreateActor(t, db)
			first, second := revisionCreateStart(t, path, actor), revisionCreateStart(t, path, "")
			first.send(t, "create alpha")
			content := "beta"
			if same {
				content = "alpha"
			}
			second.send(t, "create "+content)
			one, two := first.read(t), second.read(t)
			if !strings.HasPrefix(one, "CREATED ") || !strings.HasPrefix(two, "CREATED ") {
				t.Fatal("concurrent commits:", one, two)
			}
			parts1, parts2 := strings.Fields(one), strings.Fields(two)
			if same && one != two {
				t.Fatal("same content must reuse entire original record:", one, two)
			}
			if !same && (parts1[3] != actor || parts2[3] != "NULL") {
				t.Fatal("independent actor origins:", one, two)
			}
			for _, entry := range []struct{ key, content string }{{parts1[1], "alpha"}, {parts2[1], content}} {
				var snapshot, checksum, source string
				if err := db.QueryRow("SELECT snapshot,checksum,source FROM config_revisions WHERE id=?", entry.key).Scan(&snapshot, &checksum, &source); err != nil {
					t.Fatal(err)
				}
				wantSnapshot := fmt.Sprintf(`{"content":%q}`, entry.content)
				wantHash := fmt.Sprintf("%x", sha256.Sum256([]byte(wantSnapshot)))
				if snapshot != wantSnapshot || checksum != wantHash || source != "ordinary" {
					t.Fatal("durable canonical bytes:", snapshot, checksum, source)
				}
			}
			if !same && (parts1[1] == parts2[1] || parts1[2] == parts2[2]) {
				t.Fatal("distinct rows and counters:", one, two)
			}
			state := func() string {
				t.Helper()
				var raw string
				if err := db.QueryRow(`SELECT json_group_array(json_array(id,revision_number,checksum,snapshot,actor_user_id,
					created_at,applied_at,source,source_primary_id,source_node_id,source_revision_id,
					snapshot_content_hash,snapshot_version,replication_version,leadership_generation))
					FROM (SELECT * FROM config_revisions ORDER BY revision_number)`).Scan(&raw); err != nil {
					t.Fatal(err)
				}
				return raw
			}
			before := state()
			first.send(t, "hold")
			if got := first.read(t); got != "HELD" {
				t.Fatal(got)
			}
			start := time.Now()
			second.send(t, "create gamma")
			if got := second.read(t); got != "BUSY" {
				t.Fatal("BUSY must reject:", got)
			}
			elapsed := time.Since(start)
			if elapsed < 100*time.Millisecond || elapsed > 2*time.Second {
				t.Fatal("busy bound:", elapsed)
			}
			t.Logf("independent-process BUSY elapsed=%s", elapsed)
			if state() != before {
				t.Fatal("BUSY changed native rows")
			}
			second.send(t, "create alpha")
			if got := second.read(t); got != "BUSY" {
				t.Fatal("dedup must also own writer transaction:", got)
			}
			if state() != before {
				t.Fatal("dedup BUSY changed stored originals")
			}
			first.send(t, "release")
			if got := first.read(t); got != "RELEASED" {
				t.Fatal(got)
			}
			second.send(t, "create alpha")
			if got := second.read(t); got != one {
				t.Fatal("post-BUSY dedup must preserve original actor and row:", got, one)
			}
			second.send(t, "create gamma")
			if got := second.read(t); !strings.HasPrefix(got, "CREATED ") {
				t.Fatal("release recovery:", got)
			}
			reopened, err := sqlitedb.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			var count, min, max, ordinary, attributed int
			want := 3
			if same {
				want = 2
			}
			if err := reopened.QueryRow(`SELECT count(*),min(revision_number),max(revision_number),
				sum(source='ordinary'),sum(actor_user_id IS NOT NULL) FROM config_revisions`).Scan(&count, &min, &max, &ordinary, &attributed); err != nil || count != want || min != 1 || max != want || ordinary != want {
				t.Fatal("durable sequential counters:", count, min, max, ordinary, err)
			}
			if !same && attributed != 1 {
				t.Fatal("distinct actor origins:", attributed)
			}
			var storedActor sql.NullString
			if err := reopened.QueryRow("SELECT actor_user_id FROM config_revisions WHERE id=?", parts1[1]).Scan(&storedActor); err != nil {
				t.Fatal(err)
			}
			if !same && (!storedActor.Valid || storedActor.String != actor) {
				t.Fatal("first actor:", storedActor)
			}
			if same && (storedActor.Valid && storedActor.String != actor) {
				t.Fatal("winning actor must be native original:", storedActor)
			}
		})
	}
}
