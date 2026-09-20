package dbimport

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestPostgresAuditEmitterWritesBackupImport(t *testing.T) {
	pool := openImportTestPool(t)
	ctx := context.Background()
	actorID := uuid.NewString()
	username := "audit-emitter-" + actorID
	if _, err := pool.Exec(ctx, `
		insert into users (id, username, password_hash, role, active, password_change_required)
		values ($1, $2, $3, 'owner', true, false)
	`, actorID, username, "fixture-hash"); err != nil {
		t.Fatalf("insert audit actor: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `delete from audit_events where actor_user_id = $1`, actorID)
		_, _ = pool.Exec(context.Background(), `delete from users where id = $1`, actorID)
	})

	fixedNow := time.Date(2026, 1, 2, 3, 4, 5, 6, time.FixedZone("fixture", 3600))
	emitter := &PostgresAuditEmitter{
		Pool: pool,
		Now:  func() time.Time { return fixedNow },
	}
	if err := emitter.EmitBackupImport(ctx, actorID, "0123456789abcdef0123456789abcdef", false, true); err != nil {
		t.Fatalf("EmitBackupImport: %v", err)
	}

	var (
		action        string
		storedActorID string
		resourceType  string
		resourceID    *string
		beforeValue   []byte
		afterValue    []byte
		correlationID string
		result        string
		createdAt     time.Time
	)
	err := pool.QueryRow(ctx, `
		select action, actor_user_id::text, resource_type, resource_id,
		       before_value, after_value, correlation_id, result, created_at
		from audit_events
		where actor_user_id = $1 and action = 'backup.import'
		order by created_at desc
		limit 1
	`, actorID).Scan(
		&action, &storedActorID, &resourceType, &resourceID,
		&beforeValue, &afterValue, &correlationID, &result, &createdAt,
	)
	if err != nil {
		t.Fatalf("query audit event: %v", err)
	}
	if action != "backup.import" || storedActorID != actorID || resourceType != "backup" {
		t.Fatalf("event identity = (%q, %q, %q), want backup.import/%s/backup", action, storedActorID, resourceType, actorID)
	}
	if resourceID != nil || beforeValue != nil || correlationID != "" || result != "success" {
		t.Fatalf("event metadata = resource=%v before=%q correlation=%q result=%q", resourceID, beforeValue, correlationID, result)
	}
	if !createdAt.Equal(fixedNow.UTC()) {
		t.Fatalf("created_at = %s, want %s", createdAt, fixedNow.UTC())
	}

	var after map[string]any
	if err := json.Unmarshal(afterValue, &after); err != nil {
		t.Fatalf("decode after_value: %v", err)
	}
	if len(after) != 3 || after["bundleSha256Prefix"] != "0123456789abcdef" || after["dryRun"] != false || after["success"] != true {
		t.Fatalf("after_value = %#v, want only bundle prefix, dryRun, and success", after)
	}
}

func TestPostgresAuditEmitterRecordsDryRunFailure(t *testing.T) {
	pool := openImportTestPool(t)
	ctx := context.Background()
	actorID := uuid.NewString()
	if _, err := pool.Exec(ctx, `
		insert into users (id, username, password_hash, role, active, password_change_required)
		values ($1, $2, $3, 'owner', true, false)
	`, actorID, "audit-dry-run-"+actorID, "fixture-hash"); err != nil {
		t.Fatalf("insert audit actor: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `delete from audit_events where actor_user_id = $1`, actorID)
		_, _ = pool.Exec(context.Background(), `delete from users where id = $1`, actorID)
	})

	emitter := &PostgresAuditEmitter{Pool: pool, Now: time.Now}
	if err := emitter.EmitBackupImport(ctx, actorID, "short-sha", true, false); err != nil {
		t.Fatalf("EmitBackupImport: %v", err)
	}

	var action, result string
	var afterValue []byte
	if err := pool.QueryRow(ctx, `
		select action, after_value, result
		from audit_events
		where actor_user_id = $1 and action = 'backup.import.dryrun'
		order by created_at desc
		limit 1
	`, actorID).Scan(&action, &afterValue, &result); err != nil {
		t.Fatalf("query dry-run audit event: %v", err)
	}
	var after map[string]any
	if err := json.Unmarshal(afterValue, &after); err != nil {
		t.Fatalf("decode after_value: %v", err)
	}
	if action != "backup.import.dryrun" || result != "failure" || after["bundleSha256Prefix"] != "short-sha" || after["dryRun"] != true || after["success"] != false {
		t.Fatalf("dry-run audit = action %q result %q after %#v", action, result, after)
	}
}

func TestPostgresAuditEmitterNilPoolReturnsNil(t *testing.T) {
	emitter := &PostgresAuditEmitter{}
	if err := emitter.EmitBackupImport(context.Background(), "", "bundle", false, false); err != nil {
		t.Fatalf("EmitBackupImport(nil pool) = %v, want nil", err)
	}

	var nilEmitter *PostgresAuditEmitter
	if err := nilEmitter.EmitBackupImport(context.Background(), "", "bundle", false, false); err != nil {
		t.Fatalf("EmitBackupImport(nil emitter) = %v, want nil", err)
	}
}
