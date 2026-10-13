package configuration

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

// SQLiteSettingsStore is a partial settings adapter, not a ConfigurationStore.
// The caller owns the database, migration installation, and writer.
type SQLiteSettingsStore struct {
	writer         *sqlitedb.Writer
	defaultIngress domain.Ingress
}

func NewSQLiteSettingsStore(db *sql.DB, writer *sqlitedb.Writer, ingress domain.Ingress) *SQLiteSettingsStore {
	return &SQLiteSettingsStore{writer: writer, defaultIngress: ingress}
}

// settings runs ensure, optional ingress initialization, and mapping on one writer.
func (s *SQLiteSettingsStore) settings(v sqlitedb.WriteValue, ingress domain.Ingress) (settingsRow, bool, error) {
	ctx, conn := v.Context(), v.Conn()
	_, err := conn.ExecContext(ctx, `insert into installation_settings
		(id,forwarding_rules,default_resolver_pool,created_at,updated_at)
		values (?, ?, NULL, ?, ?) on conflict(id) do nothing`, installationID, "[]", v.Timestamp(), v.Timestamp())
	if err != nil {
		return settingsRow{}, false, err
	}
	row, err := s.read(v)
	if err != nil {
		return row, false, err
	}
	changed := row.ingressIPv4 == nil && row.ingressIPv6 == nil && (ingress.IPv4 != "" || ingress.IPv6 != "")
	if changed {
		_, err = conn.ExecContext(ctx, `update installation_settings set ingress_ipv4=?,ingress_ipv6=?,updated_at=? where id=?`, nullableString(ingress.IPv4), nullableString(ingress.IPv6), v.Timestamp(), installationID)
		if err == nil {
			row, err = s.read(v)
		}
	}
	return row, changed, err
}

func (s *SQLiteSettingsStore) read(v sqlitedb.WriteValue) (settingsRow, error) {
	var row settingsRow
	err := v.Conn().QueryRowContext(v.Context(), `select id,ingress_ipv4,ingress_ipv6,
		default_resolver_pool,forwarding_rules,retention_max_age_days,retention_max_size_mb,
		current_desired_revision_id,current_applied_revision_id from installation_settings where id=?`, installationID).Scan(
		&row.id, &row.ingressIPv4, &row.ingressIPv6, &row.defaultResolverPool, &row.forwardingRules,
		&row.retentionMaxAgeDays, &row.retentionMaxSizeMb, &row.currentDesiredRevisionID, &row.currentAppliedRevisionID)
	return row, err
}

func (s *SQLiteSettingsStore) GetSettings(ctx context.Context) (result domain.Settings, err error) {
	err = s.writer.Write(ctx, func(v sqlitedb.WriteValue) error {
		row, _, err := s.settings(v, s.defaultIngress)
		if err != nil {
			return err
		}
		result, err = toSettings(row)
		return err
	})
	return result, err
}

func (s *SQLiteSettingsStore) InitializeIngress(ctx context.Context, ingress domain.Ingress) (changed bool, err error) {
	if ingress.IPv4 == "" && ingress.IPv6 == "" {
		return false, nil
	}
	err = s.writer.Write(ctx, func(v sqlitedb.WriteValue) error {
		var err error
		_, changed, err = s.settings(v, ingress)
		return err
	})
	if err != nil {
		return false, err
	}
	return changed, nil
}

func (s *SQLiteSettingsStore) UpdateSettings(ctx context.Context, patch SettingsPatch) (result domain.Settings, err error) {
	err = s.writer.Write(ctx, func(v sqlitedb.WriteValue) error {
		row, _, err := s.settings(v, s.defaultIngress)
		if err != nil {
			return err
		}
		var ipv4, ipv6, pool, rules any
		if patch.Ingress != nil {
			ipv4, ipv6 = nullableString(patch.Ingress.IPv4), nullableString(patch.Ingress.IPv6)
		}
		if patch.DefaultPool != nil {
			validated, err := domain.ValidateResolverPool(*patch.DefaultPool)
			if err != nil {
				return err
			}
			raw, err := json.Marshal(validated)
			if err != nil {
				return err
			}
			pool = string(raw)
		}
		if patch.ForwardingRules != nil {
			validated, err := domain.ValidateForwardingRules(patch.ForwardingRules)
			if err != nil {
				return err
			}
			raw, err := json.Marshal(validated)
			if err != nil {
				return err
			}
			rules = string(raw)
		}
		if patch.RetentionMaxAgeDays != nil {
			row.retentionMaxAgeDays = *patch.RetentionMaxAgeDays
		}
		if patch.RetentionMaxSizeMb != nil {
			row.retentionMaxSizeMb = *patch.RetentionMaxSizeMb
		}
		if row.retentionMaxAgeDays < 1 || row.retentionMaxSizeMb < 1 {
			return errors.New("Retention limits must be positive")
		}
		_, err = v.Conn().ExecContext(v.Context(), `update installation_settings set
			ingress_ipv4=coalesce(?,ingress_ipv4),ingress_ipv6=coalesce(?,ingress_ipv6),
			default_resolver_pool=coalesce(?,default_resolver_pool),forwarding_rules=coalesce(?,forwarding_rules),
			retention_max_age_days=?,retention_max_size_mb=?,updated_at=? where id=?`,
			ipv4, ipv6, pool, rules, row.retentionMaxAgeDays, row.retentionMaxSizeMb, v.Timestamp(), installationID)
		if err != nil {
			return err
		}
		row, err = s.read(v)
		if err != nil {
			return err
		}
		result, err = toSettings(row)
		return err
	})
	if err != nil {
		return domain.Settings{}, err
	}
	return result, nil
}
