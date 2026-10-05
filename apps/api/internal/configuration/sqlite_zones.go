package configuration

import (
	"context"
	"database/sql"
	"errors"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

// SQLiteZoneReadStore is an additive GetZone-only reader, not runtime wiring.
// Stored UUIDs must be canonical; lookup aliases never rewrite stored rows.
type SQLiteZoneReadStore struct {
	db *sql.DB
}

func NewSQLiteZoneReadStore(db *sql.DB) *SQLiteZoneReadStore {
	return &SQLiteZoneReadStore{db: db}
}

// GetZone retains PostgreSQL's two nontransactional reads and caller-spelled
// grouping. Ordering by name alone does not establish cross-engine collation
// parity or a tie-breaker. scanRecord retains the source's float64 JSON decoding.
func (s *SQLiteZoneReadStore) GetZone(ctx context.Context, id string) (domain.ZoneState, error) {
	canonicalID, err := sqlitedb.ParseUUIDLookupText(id)
	if err != nil {
		return domain.ZoneState{}, err
	}
	var zone domain.ZoneState
	err = s.db.QueryRowContext(ctx, `select id, name, enabled from zones where id = ?`, canonicalID).Scan(&zone.ID, &zone.Name, &zone.Enabled)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ZoneState{}, errors.New("Zone not found")
	}
	if err != nil {
		return domain.ZoneState{}, err
	}
	rows, err := s.db.QueryContext(ctx, `
		select id, zone_id, name, type, value, ttl, enabled, proxied, proxy_settings, comment
		from dns_records where zone_id = ? order by name asc
	`, canonicalID)
	if err != nil {
		return domain.ZoneState{}, err
	}
	defer rows.Close()
	grouped := map[string][]domain.DNSRecord{}
	for rows.Next() {
		record, zoneID, err := scanRecord(rows)
		if err != nil {
			return domain.ZoneState{}, err
		}
		grouped[zoneID] = append(grouped[zoneID], record)
	}
	if err := rows.Err(); err != nil {
		return domain.ZoneState{}, err
	}
	// Even aliases must decode all matching rows before selecting by original id.
	zone.Records = grouped[id]
	if zone.Records == nil {
		zone.Records = []domain.DNSRecord{}
	}
	return zone, nil
}

// ListZones follows the source's metadata-then-all-records reads without a
// transaction. Name-only ordering does not promise collation parity or ties.
func (s *SQLiteZoneReadStore) ListZones(ctx context.Context) ([]domain.ZoneState, error) {
	return sqliteListZones(ctx, s.db)
}

func sqliteListZones(ctx context.Context, q sqliteSnapshotQuerier) ([]domain.ZoneState, error) {
	zoneRows, err := q.QueryContext(ctx, `select id, name, enabled from zones order by name asc`)
	if err != nil {
		return nil, err
	}
	var metas []domain.ZoneState
	for zoneRows.Next() {
		var zone domain.ZoneState
		if err := zoneRows.Scan(&zone.ID, &zone.Name, &zone.Enabled); err != nil {
			zoneRows.Close()
			return nil, err
		}
		metas = append(metas, zone)
	}
	// Release the metadata cursor before query two, including with one connection.
	zoneRows.Close()
	if err := zoneRows.Err(); err != nil {
		return nil, err
	}

	rows, err := q.QueryContext(ctx, `
		select id, zone_id, name, type, value, ttl, enabled, proxied, proxy_settings, comment
		from dns_records order by name asc
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	grouped := map[string][]domain.DNSRecord{}
	for rows.Next() {
		record, zoneID, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		grouped[zoneID] = append(grouped[zoneID], record)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	zones := make([]domain.ZoneState, 0, len(metas))
	for _, zone := range metas {
		zone.Records = grouped[zone.ID]
		if zone.Records == nil {
			zone.Records = []domain.DNSRecord{}
		}
		zones = append(zones, zone)
	}
	return zones, nil
}
