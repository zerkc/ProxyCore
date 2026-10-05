package configuration

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/secrets"
	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
)

// sqliteSecretWriter is the part of secrets.SQLiteStore that runs inside a
// caller-owned writer transaction. It is deliberately not secrets.Store: a
// secret created here must commit or roll back with the record that uses it.
type sqliteSecretWriter interface {
	PutInWrite(v sqlitedb.WriteValue, purpose, plaintext string) (string, error)
	GetInWrite(v sqlitedb.WriteValue, id string) (string, error)
}

var _ sqliteSecretWriter = (*secrets.SQLiteStore)(nil)

// SQLiteRecordWriteStore extends the unwired SQLiteZoneWriteStore with record
// mutations. It exists as a wrapper because AddRecord needs a secret writer and
// SQLiteZoneWriteStore carries none; a nil secret writer behaves like PG's nil
// secrets.Store (Basic Auth passwords then require a configured master key).
type SQLiteRecordWriteStore struct {
	*SQLiteZoneWriteStore
	secrets sqliteSecretWriter
}

func NewSQLiteRecordWriteStore(writer *sqlitedb.Writer, secretWriter sqliteSecretWriter) *SQLiteRecordWriteStore {
	return &SQLiteRecordWriteStore{SQLiteZoneWriteStore: NewSQLiteZoneWriteStore(writer), secrets: secretWriter}
}

// AddRecord mirrors Store.AddRecord inside one writer transaction. Intentional
// difference from PostgreSQL: a Basic Auth secret is inserted in the same
// transaction as the record, so any later failure rolls the secret back too and
// no orphan secrets remain (PG commits them independently).
//
// PG quirks kept on purpose: the sibling records are indexed by the caller's
// zone id spelling, so any non-canonical spelling (uppercase, braced, hyphen-less)
// sees no siblings: they are neither validated nor rewritten, and the
// proxied-name and CNAME-coexistence rules are not enforced against them (observed
// on postgres:17, pinned by TestSQLiteAddRecordZoneAliasMirrorsPostgres). The
// conflict update never rewrites zone_id, so reusing an id from another zone
// updates that row in place.
//
// Body ids follow PG's uuid cast (observed on postgres:17): sqliteUpsertRecord
// canonicalizes uppercase, braced, hyphen-less and grouped spellings through
// ParseUUIDLookupText, so an alias updates the stored row in place. Siblings are
// still matched by the caller's spelling like PG, so an alias row is upserted
// twice (as a sibling, then as the new record, which wins); the final row is
// correct and no duplicate row or id can appear, so the quirk is replicated.
// A malformed id fails closed at the same point as PG's cast (after validation
// and the secret write) with PG's exact error text (postgresInvalidUUIDError);
// the transaction, secret included, rolls back. Remaining divergence: a malformed
// zone id still returns the shared "invalid UUID lookup text" (PG's text comes from
// its first query), as in the other SQLite zone adapters. A body id containing a
// NUL byte also differs in text only: PG reports SQLSTATE 22021 (invalid byte
// sequence for encoding "UTF8") while SQLite reports the 22P02 uuid text; both
// fail closed and roll back.
func (s *SQLiteRecordWriteStore) AddRecord(ctx context.Context, zoneID string, input domain.DNSRecordInput, actorUserID string) (AutoApplyResult[domain.DNSRecord], error) {
	var result AutoApplyResult[domain.DNSRecord]
	err := s.writer.Write(ctx, func(v sqlitedb.WriteValue) error {
		canonicalZone, err := sqlitedb.ParseUUIDLookupText(zoneID)
		if err != nil {
			return err
		}
		var zoneName string
		err = v.Conn().QueryRowContext(v.Context(), `select name from zones where id = ?`, canonicalZone).Scan(&zoneName)
		if errors.Is(err, sql.ErrNoRows) {
			return errors.New("Zone not found")
		}
		if err != nil {
			return err
		}

		current, err := sqliteZoneRecords(v, canonicalZone)
		if err != nil {
			return err
		}
		if zoneID != canonicalZone {
			current = nil
		}
		certificates, err := sqliteCertificatesByID(v)
		if err != nil {
			return err
		}
		settingsRow, _, err := (&SQLiteSettingsStore{}).settings(v, domain.Ingress{})
		if err != nil {
			return err
		}
		ingress := domain.Ingress{IPv4: deref(settingsRow.ingressIPv4), IPv6: deref(settingsRow.ingressIPv6)}

		recordID := input.ID
		if recordID == "" {
			if recordID, err = v.NewUUID(); err != nil {
				return err
			}
		}
		var existingProxy map[string]any
		others := make([]domain.DNSRecord, 0, len(current))
		for _, item := range current {
			if item.ID == recordID {
				existingProxy = item.Proxy
				continue
			}
			others = append(others, item)
		}
		proxy, err := sqliteResolveProxySettingsInput(v, input.Proxy, s.secrets, existingProxy)
		if err != nil {
			return err
		}

		ttl := 0
		if input.TTL != nil {
			ttl = *input.TTL
		}
		record := domain.DNSRecord{
			ID: recordID, Name: input.Name, Type: input.Type, Value: input.Value, TTL: ttl,
			Enabled: input.Enabled, Comment: input.Comment, Proxied: input.Proxied, Proxy: proxy,
		}
		validated, err := domain.ValidateRecordSet(append(others, record), domain.RecordSetOptions{
			ZoneName: zoneName, Ingress: &ingress, Certificates: certificates,
		})
		if err != nil {
			return err
		}
		for _, item := range validated {
			if err := sqliteUpsertRecord(v, canonicalZone, item); err != nil {
				return err
			}
		}
		apply, err := sqliteCreateApplyJobInTransaction(v, actorUserID)
		if err != nil {
			return err
		}
		for _, item := range validated {
			if item.ID == recordID {
				result.Value = item
			}
		}
		result.Apply = apply
		return nil
	})
	if err != nil {
		return AutoApplyResult[domain.DNSRecord]{}, err
	}
	return result, nil
}

// sqliteZoneRecords reads on the writer connection and closes the cursor before
// the next statement; it follows PG's name-only ordering.
func sqliteZoneRecords(v sqlitedb.WriteValue, zoneID string) ([]domain.DNSRecord, error) {
	rows, err := v.Conn().QueryContext(v.Context(), `
		select id, zone_id, name, type, value, ttl, enabled, proxied, proxy_settings, comment
		from dns_records where zone_id = ? order by name asc`, zoneID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []domain.DNSRecord
	for rows.Next() {
		record, _, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

// sqliteCertificatesByID is non-nil even when empty: ValidateRecordSet only
// looks certificates up by id, but nil would skip the existence checks.
func sqliteCertificatesByID(v sqlitedb.WriteValue) ([]domain.CertificateStatus, error) {
	rows, err := v.Conn().QueryContext(v.Context(), `select id,hostnames,issuer,challenge,environment,status,
  expires_at,renew_after,key_secret_id,certificate_pem,failure_reason from certificates order by id asc`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	certificates := []domain.CertificateStatus{}
	for rows.Next() {
		cert, err := scanCertificate(sqliteCertificateScanner{rows})
		if err != nil {
			return nil, err
		}
		certificates = append(certificates, cert)
	}
	return certificates, rows.Err()
}

// postgresInvalidUUIDError reproduces the text pgconn renders for a failed uuid
// cast, observed on postgres:17: the id is echoed raw between double quotes
// (not %q-escaped). Callers only print err.Error(), so no typed error is needed.
func postgresInvalidUUIDError(id string) error {
	return fmt.Errorf("ERROR: invalid input syntax for type uuid: \"%s\" (SQLSTATE 22P02)", id)
}

// sqliteUpsertRecord keeps created_at and zone_id on conflict, like PG.
func sqliteUpsertRecord(v sqlitedb.WriteValue, zoneID string, record domain.DNSRecord) error {
	storedID, err := sqlitedb.ParseUUIDLookupText(record.ID)
	if err != nil {
		return postgresInvalidUUIDError(record.ID)
	}
	valueJSON, err := json.Marshal(record.Value)
	if err != nil {
		return err
	}
	var proxyJSON any
	if record.Proxy != nil {
		encoded, err := json.Marshal(record.Proxy)
		if err != nil {
			return err
		}
		proxyJSON = string(encoded)
	}
	ttl := record.TTL
	if ttl == 0 {
		ttl = domain.DefaultTTL
	}
	_, err = v.Conn().ExecContext(v.Context(), `
		insert into dns_records (id, zone_id, name, type, value, ttl, enabled, proxied, proxy_settings, comment, created_at, updated_at)
		values (?,?,?,?,?,?,?,?,?,?,?,?)
		on conflict(id) do update set
			name = excluded.name,
			type = excluded.type,
			value = excluded.value,
			ttl = excluded.ttl,
			enabled = excluded.enabled,
			proxied = excluded.proxied,
			proxy_settings = excluded.proxy_settings,
			comment = excluded.comment,
			updated_at = excluded.updated_at`,
		storedID, zoneID, record.Name, record.Type, string(valueJSON), ttl,
		sqliteBool(record.Enabled), sqliteBool(record.Proxied), proxyJSON, nullablePtr(record.Comment),
		v.Timestamp(), v.Timestamp())
	return err
}

func sqliteBool(value bool) int {
	if value {
		return 1
	}
	return 0
}

// sqliteResolveProxySettingsInput is resolveProxySettingsInput with the secret
// read and written on the transaction's connection. Error strings match PG.
func sqliteResolveProxySettingsInput(v sqlitedb.WriteValue, input map[string]any, secretStore sqliteSecretWriter, existing map[string]any) (map[string]any, error) {
	if input == nil {
		return nil, nil
	}
	rest := map[string]any{}
	for key, value := range input {
		if key != "basicAuth" {
			rest[key] = value
		}
	}
	basicAuth, ok := input["basicAuth"].(map[string]any)
	if !ok {
		return rest, nil
	}
	username, _ := basicAuth["username"].(string)
	password, _ := basicAuth["password"].(string)

	if password != "" {
		if secretStore == nil {
			return nil, errors.New("Basic Auth requires a configured master key")
		}
		passwordHash, err := secrets.HashBasicAuthPassword(password)
		if err != nil {
			return nil, err
		}
		secretID, err := secretStore.PutInWrite(v, "basic-auth-password", passwordHash)
		if err != nil {
			return nil, err
		}
		rest["basicAuth"] = map[string]any{"username": username, "passwordSecretId": secretID}
		return rest, nil
	}

	passwordSecretID, _ := basicAuth["passwordSecretId"].(string)
	if passwordSecretID == "" && existing != nil {
		if existingAuth, ok := existing["basicAuth"].(map[string]any); ok {
			passwordSecretID, _ = existingAuth["passwordSecretId"].(string)
		}
	}
	if passwordSecretID == "" {
		return nil, errors.New("Basic Auth password is required")
	}
	if secretStore != nil {
		value, err := secretStore.GetInWrite(v, passwordSecretID)
		if err != nil {
			return nil, err
		}
		if value == "" {
			return nil, fmt.Errorf("Basic Auth secret not found: %s", passwordSecretID)
		}
	}
	rest["basicAuth"] = map[string]any{"username": username, "passwordSecretId": passwordSecretID}
	return rest, nil
}
