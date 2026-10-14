package configuration

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	replicationsnapshot "github.com/zerkc/ProxyCore/apps/api/internal/snapshot"
	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

// preImportArchiveReason labels the standalone archive that preserves the
// configuration an import is about to replace.
const preImportArchiveReason = "pre-import-configuration"

// sqliteMaterializeImportedConfiguration turns the replicated configuration of
// an imported envelope into the node's own editable tables (settings, zones
// with their records, streams, certificate metadata), replacing the previous
// content. It runs inside the writer transaction that also creates the import
// revision, so the tables and the revision commit or roll back together.
//
// Node-local ingress (owner exception): nodeLocal.ingress is authoritative on
// the receiving node (the importer rewrites it with the installation's own
// address), so it replaces settings.ingress in the tables. The stored envelope
// is deliberately NOT rewritten: its contentHash covers replicated.configuration
// and must stay exactly the primary's value (D11); the worker substitutes
// nodeLocal.ingress when it renders.
//
// T13 SEAM: secrets and owners are NOT materialized here. When T13 imports
// them it must do so in this same transaction BEFORE the certificate rows are
// written, so key_secret_id references resolve; until then a certificate whose
// key secret does not exist locally is stored without the reference.
func sqliteMaterializeImportedConfiguration(v sqlitedb.WriteValue, snapshotBytes []byte) error {
	env, err := replicationsnapshot.Unmarshal(snapshotBytes)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(env.Replicated.Configuration)
	if err != nil {
		return err
	}
	var cfg domain.ConfigurationSnapshot
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return fmt.Errorf("decode replicated configuration: %w", err)
	}
	streams, err := domain.ValidateStreamRoutes(append([]domain.StreamRoute{}, cfg.Streams...))
	if err != nil {
		return err
	}
	ingress := cfg.Settings.Ingress
	if env.NodeLocal.Ingress.IPv4 != "" || env.NodeLocal.Ingress.IPv6 != "" {
		ingress = env.NodeLocal.Ingress
	}

	if err := sqliteArchiveLocalConfiguration(v); err != nil {
		return err
	}
	if err := sqliteReplaceSettings(v, cfg.Settings, ingress); err != nil {
		return err
	}
	for _, statement := range []string{`delete from dns_records`, `delete from zones`, `delete from stream_routes`, `delete from certificates`} {
		if _, err := v.Conn().ExecContext(v.Context(), statement); err != nil {
			return err
		}
	}
	for _, zone := range cfg.Zones {
		zoneID, err := sqlitedb.ParseUUIDLookupText(zone.ID)
		if err != nil {
			return postgresInvalidUUIDError(zone.ID)
		}
		if _, err := v.Conn().ExecContext(v.Context(), `insert into zones (id,name,enabled,created_at,updated_at) values (?,?,?,?,?)`,
			zoneID, zone.Name, sqliteBool(zone.Enabled), v.Timestamp(), v.Timestamp()); err != nil {
			return err
		}
		for _, record := range zone.Records {
			if err := sqliteUpsertRecord(v, zoneID, record); err != nil {
				return err
			}
		}
	}
	for _, route := range streams {
		if err := sqliteUpsertStream(v, route); err != nil {
			return err
		}
	}
	for _, cert := range cfg.Certificates {
		if cert.SecretID != "" {
			secretID, err := sqliteCertificateLookupID(cert.SecretID)
			if err != nil {
				return err
			}
			var present int
			if err := v.Conn().QueryRowContext(v.Context(), `select count(*) from secrets where id = ?`, secretID).Scan(&present); err != nil {
				return err
			}
			if present == 0 {
				cert.SecretID = ""
			}
		}
		if _, err := sqliteInsertCertificate(v, cert); err != nil {
			return err
		}
	}
	return nil
}

// sqliteReplaceSettings writes the replicated settings over the node's row.
// Unlike UpdateSettings it is a replacement, not a coalescing patch: an absent
// default pool becomes NULL. Retention limits are kept when the primary does not
// carry positive values (the local row's CHECKed invariant is >= 1).
func sqliteReplaceSettings(v sqlitedb.WriteValue, settings domain.Settings, ingress domain.Ingress) error {
	row, _, err := (&SQLiteSettingsStore{}).settings(v, domain.Ingress{})
	if err != nil {
		return err
	}
	var pool any
	if settings.DefaultPool != nil {
		validated, err := domain.ValidateResolverPool(*settings.DefaultPool)
		if err != nil {
			return err
		}
		encoded, err := json.Marshal(validated)
		if err != nil {
			return err
		}
		pool = string(encoded)
	}
	rules, err := domain.ValidateForwardingRules(settings.ForwardingRules)
	if err != nil {
		return err
	}
	if rules == nil {
		rules = []domain.ForwardingRule{}
	}
	encodedRules, err := json.Marshal(rules)
	if err != nil {
		return err
	}
	maxAge, maxSize := row.retentionMaxAgeDays, row.retentionMaxSizeMb
	if settings.RetentionMaxAgeDays > 0 {
		maxAge = settings.RetentionMaxAgeDays
	}
	if settings.RetentionMaxSizeMb > 0 {
		maxSize = settings.RetentionMaxSizeMb
	}
	_, err = v.Conn().ExecContext(v.Context(), `update installation_settings set
		ingress_ipv4=?,ingress_ipv6=?,default_resolver_pool=?,forwarding_rules=?,
		retention_max_age_days=?,retention_max_size_mb=?,updated_at=? where id=?`,
		nullableString(ingress.IPv4), nullableString(ingress.IPv6), pool, string(encodedRules),
		maxAge, maxSize, v.Timestamp(), installationID)
	return err
}

// sqliteArchiveLocalConfiguration preserves the configuration that the import
// is about to replace as a standalone archive row (same 30-day TTL as the
// importer's archive). Nothing is archived when the node has no zones, streams
// or certificates, so a pristine installation leaves no empty archive behind.
func sqliteArchiveLocalConfiguration(v sqlitedb.WriteValue) error {
	row, _, err := (&SQLiteSettingsStore{}).settings(v, domain.Ingress{})
	if err != nil {
		return err
	}
	local, err := sqliteAssembleSnapshot(v.Context(), v.Conn(), row)
	if err != nil {
		return err
	}
	if len(local.Zones) == 0 && len(local.Streams) == 0 && len(local.Certificates) == 0 {
		return nil
	}
	blob, err := json.Marshal(local)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	captured, err := sqliteEnrollmentTokenTime(now)
	if err != nil {
		return err
	}
	expires, err := sqliteEnrollmentTokenTime(now.Add(replicationsnapshot.ArchiveTTLDefault))
	if err != nil {
		return err
	}
	_, err = v.Conn().ExecContext(v.Context(), `insert into standalone_archives
		(id, archive_blob, capture_reason, captured_at, expires_at, created_at)
		values (?, ?, ?, ?, ?, ?)`,
		uuid.NewString(), string(blob), preImportArchiveReason, captured, expires, v.Timestamp())
	return err
}
