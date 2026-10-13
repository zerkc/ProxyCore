package configuration

import (
	"context"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

// SQLiteZoneWriteStore is a partial, unwired mutation adapter. It promises
// atomic durable enqueue, not worker notification, detection, or execution.
type SQLiteZoneWriteStore struct {
	writer *sqlitedb.Writer
}

func NewSQLiteZoneWriteStore(writer *sqlitedb.Writer) *SQLiteZoneWriteStore {
	return &SQLiteZoneWriteStore{writer: writer}
}

// CreateZone publishes no pending value: Writer.Write must commit both the zone
// and its snapshot/revision/job before the result becomes visible to the caller.
func (s *SQLiteZoneWriteStore) CreateZone(ctx context.Context, name, actorUserID string) (AutoApplyResult[domain.ZoneState], error) {
	normalized, err := domain.NormalizeDNSName(name, false)
	if err != nil {
		return AutoApplyResult[domain.ZoneState]{}, err
	}
	var zone domain.ZoneState
	var apply ApplyResult
	err = s.writer.Write(ctx, func(v sqlitedb.WriteValue) error {
		id, err := v.NewUUID()
		if err != nil {
			return err
		}
		zone.ID = id
		if err := v.Conn().QueryRowContext(v.Context(), `insert into zones
			(id,name,enabled,created_at,updated_at) values (?,?,1,?,?) returning name,enabled`,
			id, normalized, v.Timestamp(), v.Timestamp()).Scan(&zone.Name, &zone.Enabled); err != nil {
			return err
		}
		apply, err = sqliteCreateApplyJobInTransaction(v, actorUserID)
		return err
	})
	if err != nil {
		return AutoApplyResult[domain.ZoneState]{}, err
	}
	zone.Records = []domain.DNSRecord{}
	return AutoApplyResult[domain.ZoneState]{Value: zone, Apply: apply}, nil
}
