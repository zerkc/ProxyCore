package dbimport

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

type auditRow struct {
	actor         sql.NullString
	action        string
	resourceType  string
	resourceID    sql.NullString
	before        sql.NullString
	after         string
	correlationID string
	result        string
	createdAt     string
}

func lastAuditRow(t testing.TB, db *sql.DB) auditRow {
	t.Helper()
	var row auditRow
	err := db.QueryRow(`SELECT actor_user_id, action, resource_type, resource_id, before_value, after_value, correlation_id, result, created_at
		FROM audit_events WHERE action LIKE 'backup.import%' ORDER BY rowid DESC LIMIT 1`).Scan(
		&row.actor, &row.action, &row.resourceType, &row.resourceID, &row.before, &row.after, &row.correlationID, &row.result, &row.createdAt)
	if err != nil {
		t.Fatalf("query audit event: %v", err)
	}
	return row
}

func TestSQLiteAuditEmitterWritesBackupImport(t *testing.T) {
	db := openSQLiteImportDB(t)
	seedSQLiteImportSource(t, db)
	fixedNow := time.Date(2026, 1, 2, 3, 4, 5, 6, time.FixedZone("fixture", 3600))
	emitter := &SQLiteAuditEmitter{DB: db, Now: func() time.Time { return fixedNow }}

	if err := emitter.EmitBackupImport(context.Background(), strings.ToUpper(siUser), "0123456789abcdef0123456789abcdef", false, true); err != nil {
		t.Fatalf("EmitBackupImport: %v", err)
	}
	row := lastAuditRow(t, db)
	if row.action != "backup.import" || row.actor.String != siUser || row.resourceType != "backup" || row.result != "success" {
		t.Fatalf("row = %+v", row)
	}
	if row.resourceID.Valid || row.before.Valid || row.correlationID != "" {
		t.Fatalf("metadata = %+v", row)
	}
	wantKey, _ := sqlitedb.EncodeTimestamp(sqlitedb.FiniteTimestamp{Seconds: fixedNow.Unix(), Nanos: 6})
	if row.createdAt != *wantKey {
		t.Fatalf("created_at = %q, want %q", row.createdAt, *wantKey)
	}
	var after map[string]any
	if err := json.Unmarshal([]byte(row.after), &after); err != nil {
		t.Fatal(err)
	}
	if len(after) != 3 || after["bundleSha256Prefix"] != "0123456789abcdef" || after["dryRun"] != false || after["success"] != true {
		t.Fatalf("after_value = %#v", after)
	}
}

func TestSQLiteAuditEmitterRecordsFailureAndDryRun(t *testing.T) {
	db := openSQLiteImportDB(t)
	emitter := &SQLiteAuditEmitter{DB: db}
	if err := emitter.EmitBackupImport(context.Background(), "", "short", true, false); err != nil {
		t.Fatalf("EmitBackupImport: %v", err)
	}
	row := lastAuditRow(t, db)
	if row.action != "backup.import.dryrun" || row.result != "failure" || row.actor.Valid {
		t.Fatalf("row = %+v", row)
	}
	var after map[string]any
	_ = json.Unmarshal([]byte(row.after), &after)
	if after["bundleSha256Prefix"] != "short" || after["dryRun"] != true || after["success"] != false {
		t.Fatalf("after_value = %#v", after)
	}
	if _, err := sqlitedb.DecodeTimestamp(&row.createdAt); err != nil {
		t.Fatalf("created_at is not a tagged timestamp: %v", err)
	}
}

func TestSQLiteAuditEmitterStoresNullForVanishedOrInvalidActor(t *testing.T) {
	db := openSQLiteImportDB(t)
	emitter := &SQLiteAuditEmitter{DB: db}
	for _, actor := range []string{siUser, "not-a-uuid", "10000000-0000-4000-8000-0000000000ff"} {
		if err := emitter.EmitBackupImport(context.Background(), actor, "sha", false, true); err != nil {
			t.Fatalf("actor %q: %v", actor, err)
		}
		if row := lastAuditRow(t, db); row.actor.Valid {
			t.Fatalf("actor %q stored as %q, want NULL", actor, row.actor.String)
		}
	}
}

func TestSQLiteAuditEmitterNilAndCanceled(t *testing.T) {
	if err := (*SQLiteAuditEmitter)(nil).EmitBackupImport(context.Background(), "", "", false, true); err != nil {
		t.Fatalf("nil emitter: %v", err)
	}
	if err := (&SQLiteAuditEmitter{}).EmitBackupImport(context.Background(), "", "", false, true); err != nil {
		t.Fatalf("nil db: %v", err)
	}
	db := openSQLiteImportDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := (&SQLiteAuditEmitter{DB: db}).EmitBackupImport(ctx, "", "", false, true); err == nil {
		t.Fatal("canceled context must fail")
	}
	if countRows(t, db, "audit_events") != 0 {
		t.Fatal("canceled emit wrote a row")
	}
}
