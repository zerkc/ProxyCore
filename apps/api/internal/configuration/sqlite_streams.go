package configuration

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

// SQLiteStreamStore is a partial three-method adapter, not a ConfigurationStore.
// The caller owns migration and database lifetime and must supply a writer for
// the same database; mismatched pairs are unsupported and are not detected.
type SQLiteStreamStore struct {
	db     *sql.DB
	writer *sqlitedb.Writer
}

func NewSQLiteStreamStore(db *sql.DB, writer *sqlitedb.Writer) *SQLiteStreamStore {
	return &SQLiteStreamStore{db: db, writer: writer}
}

// sqliteStreamScanner retains the shared typed JSON mapping but checks SQLite's
// integer boolean explicitly rather than relying on database/sql conversion.
type sqliteStreamScanner struct{ scanner }

func (s sqliteStreamScanner) Scan(dest ...any) error {
	var enabled int64
	boolean := dest[1].(*bool)
	dest[1] = &enabled
	if err := s.scanner.Scan(dest...); err != nil {
		return err
	}
	if enabled != 0 && enabled != 1 {
		return fmt.Errorf("invalid stream enabled value: %d", enabled)
	}
	*boolean = enabled == 1
	return nil
}

func sqliteListStreams(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}) ([]domain.StreamRoute, error) {
	rows, err := q.QueryContext(ctx, `select id,enabled,protocol,listen_address,listen_port,upstream
		from stream_routes order by listen_port asc`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	routes := []domain.StreamRoute{}
	for rows.Next() {
		route, err := scanStream(sqliteStreamScanner{rows})
		if err != nil {
			return nil, err
		}
		routes = append(routes, route)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return domain.ValidateStreamRoutes(routes)
}

func (s *SQLiteStreamStore) ListStreams(ctx context.Context) ([]domain.StreamRoute, error) {
	return sqliteListStreams(ctx, s.db)
}

// AddStream locks before loading and validating the complete listener set.
// Only the saved row is persisted; no jobs or auto-apply side effects occur.
func (s *SQLiteStreamStore) AddStream(ctx context.Context, input domain.StreamRoute) (domain.StreamRoute, error) {
	var saved domain.StreamRoute
	err := s.writer.Write(ctx, func(v sqlitedb.WriteValue) error {
		current, err := sqliteListStreams(v.Context(), v.Conn())
		if err != nil {
			return err
		}
		if input.ID == "" {
			input.ID, err = v.NewUUID()
			if err != nil {
				return err
			}
		}
		next := make([]domain.StreamRoute, 0, len(current)+1)
		for _, item := range current {
			if item.ID != input.ID {
				next = append(next, item)
			}
		}
		validated, err := domain.ValidateStreamRoutes(append(next, input))
		if err != nil {
			return err
		}
		for _, item := range validated {
			if item.ID == input.ID {
				saved = item
			}
		}
		if err := sqliteUpsertStream(v, saved); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return domain.StreamRoute{}, err
	}
	return saved, nil
}

// sqliteUpsertStream persists one already-validated route inside the caller's
// writer transaction. Shared by AddStream and the snapshot import materializer.
func sqliteUpsertStream(v sqlitedb.WriteValue, route domain.StreamRoute) error {
	upstream, err := json.Marshal(route.Upstream)
	if err != nil {
		return err
	}
	enabled := 0
	if route.Enabled {
		enabled = 1
	}
	_, err = v.Conn().ExecContext(v.Context(), `insert into stream_routes
		(id,enabled,protocol,listen_address,listen_port,upstream,created_at,updated_at)
		values (?,?,?,?,?,?,?,?) on conflict(id) do update set
		enabled=excluded.enabled,protocol=excluded.protocol,listen_address=excluded.listen_address,
		listen_port=excluded.listen_port,upstream=excluded.upstream,updated_at=excluded.updated_at`,
		route.ID, enabled, route.Protocol, route.ListenAddress, route.ListenPort, string(upstream), v.Timestamp(), v.Timestamp())
	if err != nil {
		return fmt.Errorf("add stream: %w", err)
	}
	return nil
}

func (s *SQLiteStreamStore) DeleteStream(ctx context.Context, id string) error {
	return s.writer.Write(ctx, func(v sqlitedb.WriteValue) error {
		result, err := v.Conn().ExecContext(v.Context(), "delete from stream_routes where id=?", id)
		if err != nil {
			return fmt.Errorf("delete stream: %w", err)
		}
		count, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("delete stream: %w", err)
		}
		if count == 0 {
			return fmt.Errorf("Stream not found: %s", id)
		}
		return nil
	})
}
