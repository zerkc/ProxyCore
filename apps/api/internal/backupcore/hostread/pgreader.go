package hostread

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const appliedRevisionQuery = `
SELECT id::text
FROM config_revisions
WHERE applied_at IS NOT NULL
ORDER BY applied_at DESC, id DESC
LIMIT 1`

type pgReader struct {
	pool *pgxpool.Pool
	cfg  Config
}

var _ HostSnapshotReader = (*pgReader)(nil)

// New constructs a host snapshot reader backed by the configuration database.
func New(pool *pgxpool.Pool, cfg Config) *pgReader {
	cfg.ApplyDefaults(nil)
	return &pgReader{pool: pool, cfg: cfg}
}

func (r *pgReader) EnvFile(context.Context) (NamedFile, error) {
	if r == nil {
		return NamedFile{}, errors.New("hostread: nil reader")
	}
	return fsReader{cfg: r.cfg}.envFile()
}

func (r *pgReader) AppliedRevision(ctx context.Context) (string, error) {
	if r == nil {
		return "", errors.New("hostread: nil reader")
	}
	if r.pool == nil {
		return "", errors.New("hostread: nil database pool")
	}
	if ctx == nil {
		return "", errors.New("hostread: nil context")
	}

	var revisionID string
	if err := r.pool.QueryRow(ctx, appliedRevisionQuery).Scan(&revisionID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrNoAppliedRevision
		}
		return "", fmt.Errorf("hostread: query applied revision: %w", err)
	}
	return revisionID, nil
}

func (r *pgReader) AppliedCertFiles(_ context.Context, revisionID string) ([]NamedFile, error) {
	if r == nil {
		return nil, errors.New("hostread: nil reader")
	}
	return fsReader{cfg: r.cfg}.appliedCertFiles(revisionID)
}
