package configuration

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

// SQLiteSnapshotStore is a partial desired-state reader, not runtime wiring.
// The caller owns the database and must supply a writer for that database.
type SQLiteSnapshotStore struct {
	db     *sql.DB
	writer *sqlitedb.Writer
}

func NewSQLiteSnapshotStore(db *sql.DB, writer *sqlitedb.Writer) *SQLiteSnapshotStore {
	return &SQLiteSnapshotStore{db: db, writer: writer}
}

type sqliteSnapshotQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// Snapshot commits only missing-settings initialization before sequential pool
// reads, like the PostgreSQL source. Later read failures must not undo it.
func (s *SQLiteSnapshotStore) Snapshot(ctx context.Context) (domain.ConfigurationSnapshot, error) {
	var row settingsRow
	err := s.writer.Write(ctx, func(v sqlitedb.WriteValue) error {
		var err error
		row, _, err = (&SQLiteSettingsStore{}).settings(v, domain.Ingress{})
		return err
	})
	if err != nil {
		return domain.ConfigurationSnapshot{}, err
	}
	return sqliteAssembleSnapshot(ctx, s.db, row)
}

// snapshot uses the caller's existing transaction, including uncommitted rows.
// Propagate errors to Writer.Write so initialization and mutations roll back.
func (s *SQLiteSnapshotStore) snapshot(v sqlitedb.WriteValue) (domain.ConfigurationSnapshot, error) {
	row, _, err := (&SQLiteSettingsStore{}).settings(v, domain.Ingress{})
	if err != nil {
		return domain.ConfigurationSnapshot{}, err
	}
	return sqliteAssembleSnapshot(v.Context(), v.Conn(), row)
}

func sqliteAssembleSnapshot(ctx context.Context, q sqliteSnapshotQuerier, row settingsRow) (domain.ConfigurationSnapshot, error) {
	settings, err := toSettings(row)
	if err != nil {
		return domain.ConfigurationSnapshot{}, err
	}
	zones, err := sqliteListZones(ctx, q)
	if err != nil {
		return domain.ConfigurationSnapshot{}, err
	}
	streams, err := sqliteListStreams(ctx, q)
	if err != nil {
		return domain.ConfigurationSnapshot{}, err
	}
	rows, err := q.QueryContext(ctx, `select id,hostnames,issuer,challenge,environment,status,
  expires_at,renew_after,key_secret_id,certificate_pem,failure_reason from certificates order by id asc`)
	if err != nil {
		return domain.ConfigurationSnapshot{}, err
	}
	defer rows.Close()
	certificates := []domain.CertificateStatus{}
	for rows.Next() {
		cert, err := scanCertificate(sqliteCertificateScanner{rows})
		if err != nil {
			return domain.ConfigurationSnapshot{}, err
		}
		certificates = append(certificates, cert)
	}
	if err := rows.Err(); err != nil {
		return domain.ConfigurationSnapshot{}, err
	}
	return domain.ConfigurationSnapshot{Settings: settings, Zones: zones, Streams: streams, Certificates: certificates}, nil
}

// Adapt only the nullable timestamp destinations of the shared row decoder.
// SQL NULL is independent for each clock; infinity is never silently omitted.
type sqliteCertificateScanner struct{ scanner }

func (s sqliteCertificateScanner) Scan(dest ...any) error {
	expires, renew := dest[6].(**time.Time), dest[7].(**time.Time)
	var expiresKey, renewKey *string
	dest[6], dest[7] = &expiresKey, &renewKey
	if err := s.scanner.Scan(dest...); err != nil {
		return err
	}
	var err error
	*expires, err = sqliteCertificateTime(expiresKey)
	if err != nil {
		return fmt.Errorf("certificate expires_at: %w", err)
	}
	*renew, err = sqliteCertificateTime(renewKey)
	if err != nil {
		return fmt.Errorf("certificate renew_after: %w", err)
	}
	return nil
}

func sqliteCertificateTime(key *string) (*time.Time, error) {
	value, err := sqlitedb.DecodeTimestamp(key)
	if err != nil {
		return nil, err
	}
	switch v := value.(type) {
	case sqlitedb.NullTimestamp:
		return nil, nil
	case sqlitedb.FiniteTimestamp:
		// PostgreSQL finite timestamp bounds, shifted from its 2000 epoch to Unix.
		// Compare seconds directly: microsecond multiplication could overflow.
		// https://raw.githubusercontent.com/postgres/postgres/REL_17_STABLE/src/include/datatype/timestamp.h
		// https://www.postgresql.org/docs/17/datatype-datetime.html
		if v.Seconds < -210866803200 || v.Seconds >= 9224318016000 {
			return nil, fmt.Errorf("certificate timestamp outside PostgreSQL finite range")
		}
		// pgx's default timestamptz scan retains time.Local and all nanoseconds.
		// JSON's narrower year range is a separate serialization failure domain.
		result := time.Unix(v.Seconds, int64(v.Nanos))
		return &result, nil
	default:
		return nil, fmt.Errorf("nonfinite certificate timestamp")
	}
}
