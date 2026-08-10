package configuration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/secrets"
)

// AddRecord validates, upserts a record, and enqueues an apply.
func (s *Store) AddRecord(ctx context.Context, zoneID string, input domain.DNSRecordInput, actorUserID string) (AutoApplyResult[domain.DNSRecord], error) {
	var result AutoApplyResult[domain.DNSRecord]
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return result, err
	}
	defer tx.Rollback(ctx)

	var zoneName string
	err = tx.QueryRow(ctx, `select name from zones where id = $1 for update`, zoneID).Scan(&zoneName)
	if errors.Is(err, pgx.ErrNoRows) {
		return result, errors.New("Zone not found")
	}
	if err != nil {
		return result, err
	}

	grouped, err := recordsGroupedByZone(ctx, tx, zoneID)
	if err != nil {
		return result, err
	}
	current := grouped[zoneID]

	certificates, err := listCertificatesOrdered(ctx, tx, "created_at desc")
	if err != nil {
		return result, err
	}

	settingsRow, err := ensureSettings(ctx, tx)
	if err != nil {
		return result, err
	}
	ingress := domain.Ingress{IPv4: deref(settingsRow.ingressIPv4), IPv6: deref(settingsRow.ingressIPv6)}

	recordID := input.ID
	if recordID == "" {
		recordID = newUUID()
	}
	var existing *domain.DNSRecord
	for i := range current {
		if current[i].ID == recordID {
			existing = &current[i]
			break
		}
	}
	var existingProxy map[string]any
	if existing != nil {
		existingProxy = existing.Proxy
	}
	proxy, err := resolveProxySettingsInput(ctx, input.Proxy, s.secrets, existingProxy)
	if err != nil {
		return result, err
	}

	ttl := 0
	if input.TTL != nil {
		ttl = *input.TTL
	}
	record := domain.DNSRecord{
		ID:      recordID,
		Name:    input.Name,
		Type:    input.Type,
		Value:   input.Value,
		TTL:     ttl,
		Enabled: input.Enabled,
		Comment: input.Comment,
		Proxied: input.Proxied,
		Proxy:   proxy,
	}

	others := make([]domain.DNSRecord, 0, len(current))
	for _, item := range current {
		if item.ID != recordID {
			others = append(others, item)
		}
	}
	validated, err := domain.ValidateRecordSet(append(others, record), domain.RecordSetOptions{
		ZoneName:     zoneName,
		Ingress:      &ingress,
		Certificates: certificates,
	})
	if err != nil {
		return result, err
	}

	for _, item := range validated {
		if err := upsertRecord(ctx, tx, zoneID, item); err != nil {
			return result, err
		}
	}
	apply, err := createApplyJobInTransaction(ctx, tx, actorUserID)
	if err != nil {
		return result, err
	}
	if err := tx.Commit(ctx); err != nil {
		return result, err
	}

	for _, item := range validated {
		if item.ID == recordID {
			result.Value = item
		}
	}
	result.Apply = apply
	return result, nil
}

func upsertRecord(ctx context.Context, tx querier, zoneID string, record domain.DNSRecord) error {
	valueJSON, err := json.Marshal(record.Value)
	if err != nil {
		return err
	}
	proxyJSON, err := marshalOrNull(record.Proxy)
	if err != nil {
		return err
	}
	ttl := record.TTL
	if ttl == 0 {
		ttl = domain.DefaultTTL
	}
	_, err = tx.Exec(ctx, `
		insert into dns_records (id, zone_id, name, type, value, ttl, enabled, proxied, proxy_settings, comment)
		values ($1, $2, $3, $4::proxycore_record_type, $5, $6, $7, $8, $9, $10)
		on conflict (id) do update set
			name = excluded.name,
			type = excluded.type,
			value = excluded.value,
			ttl = excluded.ttl,
			enabled = excluded.enabled,
			proxied = excluded.proxied,
			proxy_settings = excluded.proxy_settings,
			comment = excluded.comment,
			updated_at = now()
	`, record.ID, zoneID, record.Name, record.Type, valueJSON, ttl, record.Enabled, record.Proxied, proxyJSON, nullablePtr(record.Comment))
	return err
}

func nullablePtr(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

// resolveProxySettingsInput mirrors the certificates basic-auth resolver: it
// hashes plaintext passwords into secrets and never persists them in cleartext.
func resolveProxySettingsInput(ctx context.Context, input map[string]any, secretStore secrets.Store, existing map[string]any) (map[string]any, error) {
	if input == nil {
		return nil, nil
	}
	rest := map[string]any{}
	for key, value := range input {
		if key == "basicAuth" {
			continue
		}
		rest[key] = value
	}
	basicAuthRaw, hasBasicAuth := input["basicAuth"]
	if !hasBasicAuth || basicAuthRaw == nil {
		return rest, nil
	}
	basicAuth, ok := basicAuthRaw.(map[string]any)
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
		secretID, err := secretStore.Put(ctx, "basic-auth-password", passwordHash)
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
		value, err := secretStore.Get(ctx, passwordSecretID)
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
