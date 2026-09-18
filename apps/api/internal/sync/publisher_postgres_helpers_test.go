package sync

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func waitForBlockedPublicationRowLock(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		var blocked bool
		err := pool.QueryRow(ctx, `
			select exists (
				select 1
				from pg_locks l
				left join pg_class c on c.oid = l.relation
				where not l.granted
				  and (l.locktype = 'transactionid' or
					(l.locktype = 'tuple' and c.relname in ('node_credentials', 'enrolled_nodes')))
			)
		`).Scan(&blocked)
		if err != nil {
			t.Fatalf("inspect publication row locks: %v", err)
		}
		if blocked {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("concurrent revoke did not block on publication row locks")
		case <-ticker.C:
		}
	}
}
