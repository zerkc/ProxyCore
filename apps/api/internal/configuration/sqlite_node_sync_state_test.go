package configuration

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

func nodeSyncFixture(t *testing.T) (*SQLiteNodeSyncStore, func(query string, args ...any)) {
	t.Helper()
	db, _, _ := sqliteSettingsFixture(t)
	exec := func(query string, args ...any) { settingsExec(t, db, query, args...) }
	return NewSQLiteNodeSyncStore(settingsWriter(db)), exec
}

func TestNodeSyncStoreIsUnboundUntilEnrollmentRecordsIt(t *testing.T) {
	store, _ := nodeSyncFixture(t)
	if _, bound, err := store.LoadNodeSyncState(context.Background()); err != nil || bound {
		t.Fatalf("bound=%v err=%v", bound, err)
	}
	if err := store.SaveNodeSyncProgress(context.Background(), NodeSyncProgress{AppliedHash: "x"}); !errors.Is(err, ErrNodeSyncUnbound) {
		t.Fatalf("Save before bind = %v, want ErrNodeSyncUnbound", err)
	}
}

func TestNodeSyncStoreRoundTripsBindingAndProgress(t *testing.T) {
	store, _ := nodeSyncFixture(t)
	ctx := context.Background()
	binding := NodeSyncBinding{
		PrimaryURL: "https://primary.example:3443", CredentialID: uuid.NewString(), PrimaryInstallationID: uuid.NewString(),
	}
	hash := strings.Repeat("a", 64)
	now := time.Date(2026, 6, 1, 12, 0, 0, 123, time.UTC)
	if err := store.BindNodeSync(ctx, binding, hash, now); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	state, bound, err := store.LoadNodeSyncState(ctx)
	if err != nil || !bound || state.NodeSyncBinding != binding {
		t.Fatalf("state=%+v bound=%v err=%v", state, bound, err)
	}
	if state.AppliedHash != hash || state.PrimaryHash != hash || state.Outdated() || !state.LastSuccessAt.Equal(now) {
		t.Fatalf("progress=%+v", state.NodeSyncProgress)
	}

	next := state.NodeSyncProgress
	next.PrimaryHash = strings.Repeat("b", 64)
	next.AckedHash = hash
	next.PrimaryAppliedAt = now.Add(time.Minute)
	next.LastAttemptAt = now.Add(2 * time.Minute)
	next.NextAttemptAt = now.Add(3 * time.Minute)
	next.ConsecutiveFailures = 2
	next.LastError = "The primary could not be reached."
	if err := store.SaveNodeSyncProgress(ctx, next); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, _, err := store.LoadNodeSyncState(ctx)
	if err != nil || got.NodeSyncProgress != next || !got.Outdated() {
		t.Fatalf("round trip = %+v err=%v, want %+v", got.NodeSyncProgress, err, next)
	}
	// Exactly one progress observation is kept.
	var n int
	if err := store.writer.Write(ctx, func(v sqlitedb.WriteValue) error {
		return v.Conn().QueryRowContext(v.Context(), `select count(*) from health_observations where component = 'node-sync'`).Scan(&n)
	}); err != nil || n != 1 {
		t.Fatalf("observations=%d err=%v", n, err)
	}
}

func TestNodeSyncStoreDerivesAppliedHashFromImportedRevisionWhenProgressIsLost(t *testing.T) {
	store, exec := nodeSyncFixture(t)
	ctx := context.Background()
	if err := store.BindNodeSync(ctx, NodeSyncBinding{PrimaryURL: "https://p.example:3443", CredentialID: uuid.NewString()}, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	hash := strings.Repeat("c", 64)
	revision, job := uuid.NewString(), uuid.NewString()
	exec(`insert into config_revisions(id,revision_number,checksum,snapshot,created_at,source,source_primary_id,snapshot_content_hash,snapshot_version,replication_version,leadership_generation) values(?,1,?, '{}','0','import',?,?,1,1,2)`,
		revision, uuid.NewString(), uuid.NewString(), hash)
	exec(`insert into apply_jobs(id,revision_id,target,status,correlation_id,created_at,source,source_primary_id,snapshot_content_hash,snapshot_version,replication_version,leadership_generation) values(?,?,'combined','applied',?,'0','import',?,?,1,1,2)`,
		job, revision, uuid.NewString(), uuid.NewString(), hash)
	exec(`delete from health_observations where component = 'node-sync'`)
	state, bound, err := store.LoadNodeSyncState(ctx)
	if err != nil || !bound || state.AppliedHash != hash {
		t.Fatalf("state=%+v bound=%v err=%v", state, bound, err)
	}
}

func TestNodeSyncStoreRebindResetsBackoff(t *testing.T) {
	store, _ := nodeSyncFixture(t)
	ctx := context.Background()
	first := NodeSyncBinding{PrimaryURL: "https://one.example:3443", CredentialID: uuid.NewString()}
	if err := store.BindNodeSync(ctx, first, strings.Repeat("a", 64), time.Now()); err != nil {
		t.Fatal(err)
	}
	second := NodeSyncBinding{PrimaryURL: "https://two.example:3443", CredentialID: uuid.NewString()}
	if err := store.BindNodeSync(ctx, second, strings.Repeat("d", 64), time.Now()); err != nil {
		t.Fatal(err)
	}
	state, bound, err := store.LoadNodeSyncState(ctx)
	if err != nil || !bound || state.NodeSyncBinding != second || state.AppliedHash != strings.Repeat("d", 64) || state.AckedHash != "" {
		t.Fatalf("state=%+v bound=%v err=%v", state, bound, err)
	}
}
