// Package dbimport audit emitter bridges the import engine to the
// audit_events table. The passphrase, master key, secrets, and
// certificate bytes MUST NEVER appear in any audit field. The
// emitter is the only layer that should hydrate the
// dbimport.AuditEmitter interface.
package dbimport

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type PostgresAuditEmitter struct {
	Pool *pgxpool.Pool
	Now  func() time.Time
}

func (e *PostgresAuditEmitter) EmitBackupImport(ctx context.Context, actorID string, bundleSHA256 string, dryRun, success bool) error {
	if e == nil || e.Pool == nil {
		return nil
	}

	shaPrefix := bundleSHA256
	if len(shaPrefix) > 16 {
		shaPrefix = shaPrefix[:16]
	}
	after := map[string]any{
		"bundleSha256Prefix": shaPrefix,
		"dryRun":             dryRun,
		"success":            success,
	}
	serialized, err := json.Marshal(after)
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
	now := time.Now
	if e.Now != nil {
		now = e.Now
	}
	createdAt := now().UTC().Truncate(time.Microsecond)
	_, err = e.Pool.Exec(ctx, `
		insert into audit_events (
			id, actor_user_id, action, resource_type, resource_id,
			before_value, after_value, correlation_id, result, created_at
		) values (
			$1, $2, $3, $4, $5,
			null, $6::jsonb, $7, $8, $9
		)
	`, uuid.NewString(), nilOrUUID(actorID), action, "backup", nilOrUUID(""), string(serialized), "", result, createdAt)
	return err
}

func nilOrUUID(s string) any {
	if s == "" {
		return nil
	}
	return s
}
