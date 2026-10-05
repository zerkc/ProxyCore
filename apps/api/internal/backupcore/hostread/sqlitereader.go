package hostread

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// applied_at is a BINARY-collated TEXT clock key: '0' (negative infinity) <
// '1'+digits (finite, fixed width) < '2' (positive infinity). Byte order
// therefore equals chronological order and matches PostgreSQL timestamptz
// ordering with infinities, so a plain ORDER BY selects the latest revision.
const sqliteAppliedRevisionQuery = `
SELECT id
FROM config_revisions
WHERE applied_at IS NOT NULL
ORDER BY applied_at DESC, id DESC
LIMIT 1`

type sqliteReader struct {
	db *sql.DB
	fs fsReader
}

var _ HostSnapshotReader = (*sqliteReader)(nil)

// NewSQLite constructs a host snapshot reader backed by the SQLite
// configuration database.
func NewSQLite(db *sql.DB, cfg Config) *sqliteReader {
	cfg.ApplyDefaults(nil)
	return &sqliteReader{db: db, fs: fsReader{cfg: cfg}}
}

func (r *sqliteReader) EnvFile(context.Context) (NamedFile, error) {
	if r == nil {
		return NamedFile{}, errors.New("hostread: nil reader")
	}
	return r.fs.envFile()
}

func (r *sqliteReader) AppliedRevision(ctx context.Context) (string, error) {
	if r == nil {
		return "", errors.New("hostread: nil reader")
	}
	if r.db == nil {
		return "", errors.New("hostread: nil database")
	}
	if ctx == nil {
		return "", errors.New("hostread: nil context")
	}

	var revisionID string
	if err := r.db.QueryRowContext(ctx, sqliteAppliedRevisionQuery).Scan(&revisionID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrNoAppliedRevision
		}
		return "", fmt.Errorf("hostread: query applied revision: %w", err)
	}
	return revisionID, nil
}

func (r *sqliteReader) AppliedCertFiles(_ context.Context, revisionID string) ([]NamedFile, error) {
	if r == nil {
		return nil, errors.New("hostread: nil reader")
	}
	return r.fs.appliedCertFiles(revisionID)
}
