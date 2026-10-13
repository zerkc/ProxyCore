package identity

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

// SQLiteStore is the SQLite-backed Store. It is additive and inactive until a
// caller selects it; the caller owns Open, Migrate, Close and the matching
// database/writer pair. Reads use the database directly, writes go through the
// single writer.
//
// Deliberate differences from PgStore: concurrent first boots converge on one
// identity (the existence check is repeated inside the write, so exactly one
// caller sees created=true), a created identity is returned in canonical
// lowercase form, and UpdatedAt is always UTC.
type SQLiteStore struct {
	db     *sql.DB
	writer *sqlitedb.Writer
}

var _ Store = (*SQLiteStore)(nil)

func NewSQLiteStore(db *sql.DB, writer *sqlitedb.Writer) *SQLiteStore {
	return &SQLiteStore{db: db, writer: writer}
}

const sqliteSelectIdentity = `select installation_id, node_id, role, leadership_generation,
	latest_known_generation, cluster_key_id, updated_at from installation_identity where id = ?`

type sqliteRowScanner interface{ Scan(dest ...any) error }

// sqliteScanIdentity decodes one row. Like PG's Ensure-on-hit it does not
// validate the role; Get does.
func sqliteScanIdentity(row sqliteRowScanner) (Identity, error) {
	var installation, node, role, updated string
	var leadership, latest int64
	var clusterKey sql.NullString
	if err := row.Scan(&installation, &node, &role, &leadership, &latest, &clusterKey, &updated); err != nil {
		return Identity{}, err
	}
	stamp, err := sqlitedb.DecodeTimestamp(&updated)
	if err != nil {
		return Identity{}, fmt.Errorf("installation_identity updated_at: %w", err)
	}
	finite, ok := stamp.(sqlitedb.FiniteTimestamp)
	if !ok {
		return Identity{}, errors.New("installation_identity updated_at: timestamp must be finite")
	}
	id := Identity{
		InstallationID:        domain.InstallationID(installation),
		NodeID:                domain.NodeID(node),
		Role:                  domain.TopologyRole(role),
		LeadershipGeneration:  domain.LeadershipGeneration(leadership),
		LatestKnownGeneration: domain.LeadershipGeneration(latest),
		UpdatedAt:             time.Unix(finite.Seconds, int64(finite.Nanos)).UTC(),
	}
	if clusterKey.Valid {
		parsed, err := uuid.Parse(clusterKey.String)
		if err != nil {
			return Identity{}, fmt.Errorf("installation_identity cluster_key_id: %w", err)
		}
		id.ClusterKeyID = &parsed
	}
	return id, nil
}

func (s *SQLiteStore) ready(ctx context.Context, needWriter bool) error {
	if ctx == nil {
		return errors.New("identity store context is required")
	}
	if s == nil || s.db == nil || (needWriter && s.writer == nil) {
		return errors.New("identity store is required")
	}
	return nil
}

func (s *SQLiteStore) Get(ctx context.Context) (Identity, error) {
	if err := s.ready(ctx, false); err != nil {
		return Identity{}, err
	}
	id, err := sqliteScanIdentity(s.db.QueryRowContext(ctx, sqliteSelectIdentity, singletonID))
	if errors.Is(err, sql.ErrNoRows) {
		return Identity{}, ErrNotFound
	}
	if err != nil {
		return Identity{}, err
	}
	if !id.Role.IsValid() {
		return Identity{}, errors.New("invalid role in installation_identity: " + string(id.Role))
	}
	return id, nil
}

func (s *SQLiteStore) Ensure(ctx context.Context, installationID domain.InstallationID, nodeID domain.NodeID) (Identity, bool, error) {
	if err := s.ready(ctx, true); err != nil {
		return Identity{}, false, err
	}
	// Fast path: an existing row never needs the write lock.
	existing, err := sqliteScanIdentity(s.db.QueryRowContext(ctx, sqliteSelectIdentity, singletonID))
	if err == nil {
		return existing, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Identity{}, false, err
	}
	var result Identity
	var created bool
	err = s.writer.Write(ctx, func(v sqlitedb.WriteValue) error {
		// Re-check under the write lock so racing first boots converge on the
		// winner's row instead of failing on the primary key.
		found, err := sqliteScanIdentity(v.Conn().QueryRowContext(v.Context(), sqliteSelectIdentity, singletonID))
		if err == nil {
			result, created = found, false
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		parsedInstallation, err := uuid.Parse(string(installationID))
		if err != nil {
			return errors.New("invalid installation id: " + err.Error())
		}
		parsedNode, err := uuid.Parse(string(nodeID))
		if err != nil {
			return errors.New("invalid node id: " + err.Error())
		}
		if _, err := v.Conn().ExecContext(v.Context(),
			`insert into installation_identity
			   (id, installation_id, node_id, role, leadership_generation, latest_known_generation, updated_at)
			 values (?, ?, ?, 'standalone-primary', 1, 1, ?)`,
			singletonID, parsedInstallation.String(), parsedNode.String(), v.Timestamp(),
		); err != nil {
			return err
		}
		result, err = sqliteScanIdentity(v.Conn().QueryRowContext(v.Context(), sqliteSelectIdentity, singletonID))
		if err != nil {
			return err
		}
		created = true
		return nil
	})
	if err != nil {
		return Identity{}, false, err
	}
	return result, created, nil
}

// sqliteUpdate runs one UPDATE of the singleton row, appending the write
// clock and the singleton id to args, and maps zero affected rows to ErrNotFound.
func (s *SQLiteStore) sqliteUpdate(ctx context.Context, query string, args ...any) error {
	if err := s.ready(ctx, true); err != nil {
		return err
	}
	return s.writer.Write(ctx, func(v sqlitedb.WriteValue) error {
		res, err := v.Conn().ExecContext(v.Context(), query, append(args, v.Timestamp(), singletonID)...)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

func (s *SQLiteStore) UpdateRole(ctx context.Context, role domain.TopologyRole) error {
	if !role.IsValid() {
		return errors.New("invalid role: " + string(role))
	}
	return s.sqliteUpdate(ctx, `update installation_identity set role = ?, updated_at = ? where id = ?`, string(role))
}

func (s *SQLiteStore) UpdateLeadershipGeneration(ctx context.Context, gen domain.LeadershipGeneration) error {
	return s.sqliteUpdate(ctx,
		`update installation_identity
		    set leadership_generation = ?,
		        latest_known_generation = max(latest_known_generation, ?),
		        updated_at = ?
		  where id = ?`, int64(gen), int64(gen))
}

func (s *SQLiteStore) UpdateLatestKnownGeneration(ctx context.Context, gen domain.LeadershipGeneration) error {
	return s.sqliteUpdate(ctx,
		`update installation_identity
		    set latest_known_generation = max(latest_known_generation, ?),
		        updated_at = ?
		  where id = ?`, int64(gen))
}

func (s *SQLiteStore) UpdateClusterKeyID(ctx context.Context, keyID *uuid.UUID) error {
	var arg any
	if keyID != nil {
		arg = keyID.String()
	}
	return s.sqliteUpdate(ctx, `update installation_identity set cluster_key_id = ?, updated_at = ? where id = ?`, arg)
}
