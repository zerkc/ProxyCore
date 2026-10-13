package dbimport

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

// SQLiteAuditEmitter writes backup import audit events to audit_events through
// its own writer transaction, after the import transaction has ended. It never
// records a passphrase, key or secret: only the bundle digest prefix, the dry
// run flag and the outcome.
type SQLiteAuditEmitter struct {
	DB *sql.DB
	// Writer is optional; it defaults to a writer on DB.
	Writer *sqlitedb.Writer
	// Now overrides the event clock; nil uses the writer's transaction clock.
	Now func() time.Time
}

var _ AuditEmitter = (*SQLiteAuditEmitter)(nil)

func (e *SQLiteAuditEmitter) EmitBackupImport(ctx context.Context, actorID string, bundleSHA256 string, dryRun, success bool) error {
	if e == nil || e.DB == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	writer := e.Writer
	if writer == nil {
		writer = sqlitedb.NewWriter(e.DB, nil)
	}

	shaPrefix := bundleSHA256
	if len(shaPrefix) > 16 {
		shaPrefix = shaPrefix[:16]
	}
	serialized, err := json.Marshal(map[string]any{
		"bundleSha256Prefix": shaPrefix,
		"dryRun":             dryRun,
		"success":            success,
	})
	if err != nil {
		return err
	}
	action := "backup.import"
	if dryRun {
		action = "backup.import.dryrun"
	}
	result := "success"
	if !success {
		result = "failure"
	}
	return writer.Write(ctx, func(v sqlitedb.WriteValue) error {
		createdAt := v.Timestamp()
		if e.Now != nil {
			now := e.Now()
			key, err := sqlitedb.EncodeTimestamp(sqlitedb.FiniteTimestamp{
				Seconds: now.Unix(), Nanos: int32(now.Nanosecond()),
			})
			if err != nil || key == nil {
				return errors.Join(errors.New("encode audit timestamp"), err)
			}
			createdAt = *key
		}
		// The actor is stored only while it still exists: a replaced users
		// table may no longer contain it and audit_events.actor_user_id is a
		// NO ACTION foreign key. The subselect yields NULL for an empty,
		// malformed or vanished actor.
		_, err := v.Conn().ExecContext(v.Context(), `
			insert into audit_events (
				id, actor_user_id, action, resource_type, resource_id,
				before_value, after_value, correlation_id, result, created_at
			) values (
				?, (select id from users where id = ?), ?, 'backup', null,
				null, ?, '', ?, ?
			)`, uuid.NewString(), strings.ToLower(actorID), action, string(serialized), result, createdAt)
		return err
	})
}
