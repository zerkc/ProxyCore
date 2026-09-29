package dbimport

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

const importAdvisoryLockName = "backup.import"

func acquireImportLock(ctx context.Context, tx pgx.Tx) error {
	if tx == nil {
		return fmt.Errorf("acquire backup import lock: nil transaction")
	}
	var acquired bool
	if err := tx.QueryRow(ctx, `select pg_try_advisory_xact_lock(hashtextextended($1, 0))`, importAdvisoryLockName).Scan(&acquired); err != nil {
		return fmt.Errorf("acquire backup import lock: %w", err)
	}
	if !acquired {
		return ErrImportAlreadyInProgress
	}
	return nil
}
