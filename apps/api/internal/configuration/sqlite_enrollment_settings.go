package configuration

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

// GetEnrollmentHostnames reads exact SAN configuration without initializing ingress.
func (s *SQLiteSettingsStore) GetEnrollmentHostnames(ctx context.Context) (result EnrollmentHostnameConfig, err error) {
	err = s.writer.Write(ctx, func(v sqlitedb.WriteValue) error {
		if _, _, err := s.settings(v, domain.Ingress{}); err != nil {
			return err
		}
		var raw sql.NullString
		if err := v.Conn().QueryRowContext(v.Context(),
			`select enrollment_hostnames from installation_settings where id=?`, installationID,
		).Scan(&raw); err != nil {
			return err
		}
		var hostnames []string
		if raw.Valid {
			if err := json.Unmarshal([]byte(raw.String), &hostnames); err != nil {
				return fmt.Errorf("read enrollment hostnames: %w", err)
			}
		}
		canonical, err := NormalizeEnrollmentHostnames(hostnames)
		if err != nil {
			return fmt.Errorf("read enrollment hostnames: %w", err)
		}
		result = EnrollmentHostnameConfig{Configured: len(canonical) > 0, Hostnames: canonical}
		return nil
	})
	if err != nil {
		return EnrollmentHostnameConfig{}, err
	}
	return result, nil
}

// UpdateEnrollmentHostnames validates before ensuring the row and stores TEXT or SQL NULL.
func (s *SQLiteSettingsStore) UpdateEnrollmentHostnames(ctx context.Context, hostnames []string) (EnrollmentHostnameConfig, error) {
	canonical, err := NormalizeEnrollmentHostnames(hostnames)
	if err != nil {
		return EnrollmentHostnameConfig{}, err
	}
	var payload any
	if len(canonical) > 0 {
		raw, err := json.Marshal(canonical)
		if err != nil {
			return EnrollmentHostnameConfig{}, err
		}
		payload = string(raw)
	}
	err = s.writer.Write(ctx, func(v sqlitedb.WriteValue) error {
		if _, _, err := s.settings(v, domain.Ingress{}); err != nil {
			return err
		}
		_, err := v.Conn().ExecContext(v.Context(), `update installation_settings
			set enrollment_hostnames=?,updated_at=? where id=?`, payload, v.Timestamp(), installationID)
		return err
	})
	if err != nil {
		return EnrollmentHostnameConfig{}, err
	}
	return EnrollmentHostnameConfig{Configured: len(canonical) > 0, Hostnames: canonical}, nil
}
