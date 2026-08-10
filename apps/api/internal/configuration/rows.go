package configuration

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
)

type scanner interface {
	Scan(dest ...any) error
}

type settingsRow struct {
	id                       string
	ingressIPv4              *string
	ingressIPv6              *string
	defaultResolverPool      []byte
	forwardingRules          []byte
	retentionMaxAgeDays      int
	retentionMaxSizeMb       int
	currentDesiredRevisionID *string
	currentAppliedRevisionID *string
}

func ensureSettings(ctx context.Context, q querier) (settingsRow, error) {
	row, found, err := selectSettings(ctx, q)
	if err != nil {
		return settingsRow{}, err
	}
	if found {
		return row, nil
	}
	if _, err := q.Exec(ctx, `
		insert into installation_settings (id, forwarding_rules, default_resolver_pool)
		values ($1, '[]'::jsonb, null)
		on conflict (id) do nothing
	`, installationID); err != nil {
		return settingsRow{}, err
	}
	row, found, err = selectSettings(ctx, q)
	if err != nil {
		return settingsRow{}, err
	}
	if !found {
		return settingsRow{}, errors.New("Installation settings could not be initialized")
	}
	return row, nil
}

func selectSettings(ctx context.Context, q querier) (settingsRow, bool, error) {
	var row settingsRow
	err := q.QueryRow(ctx, `
		select id, ingress_ipv4, ingress_ipv6, default_resolver_pool, forwarding_rules,
			retention_max_age_days, retention_max_size_mb,
			current_desired_revision_id, current_applied_revision_id
		from installation_settings where id = $1
	`, installationID).Scan(
		&row.id, &row.ingressIPv4, &row.ingressIPv6, &row.defaultResolverPool, &row.forwardingRules,
		&row.retentionMaxAgeDays, &row.retentionMaxSizeMb,
		&row.currentDesiredRevisionID, &row.currentAppliedRevisionID,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return settingsRow{}, false, nil
	}
	if err != nil {
		return settingsRow{}, false, err
	}
	return row, true, nil
}

func toSettings(row settingsRow) (domain.Settings, error) {
	pool, err := parseDefaultPool(row.defaultResolverPool)
	if err != nil {
		return domain.Settings{}, err
	}
	rules, err := parseForwardingRules(row.forwardingRules)
	if err != nil {
		return domain.Settings{}, err
	}
	return domain.Settings{
		Ingress:             domain.Ingress{IPv4: deref(row.ingressIPv4), IPv6: deref(row.ingressIPv6)},
		DefaultPool:         pool,
		ForwardingRules:     rules,
		RetentionMaxAgeDays: row.retentionMaxAgeDays,
		RetentionMaxSizeMb:  row.retentionMaxSizeMb,
	}, nil
}

func parseDefaultPool(raw []byte) (*domain.ResolverPool, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var pool domain.ResolverPool
	if err := json.Unmarshal(raw, &pool); err != nil {
		return nil, nil
	}
	if pool.ID == "" || len(pool.Endpoints) == 0 {
		return nil, nil
	}
	validated, err := domain.ValidateResolverPool(pool)
	if err != nil {
		return nil, nil
	}
	return &validated, nil
}

func parseForwardingRules(raw []byte) ([]domain.ForwardingRule, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return []domain.ForwardingRule{}, nil
	}
	var rules []domain.ForwardingRule
	if err := json.Unmarshal(raw, &rules); err != nil {
		return []domain.ForwardingRule{}, nil
	}
	validated, err := domain.ValidateForwardingRules(rules)
	if err != nil {
		return []domain.ForwardingRule{}, nil
	}
	return validated, nil
}

func listZones(ctx context.Context, q querier) ([]domain.ZoneState, error) {
	zoneRows, err := q.Query(ctx, `select id::text, name, enabled from zones order by name asc`)
	if err != nil {
		return nil, err
	}
	type zoneMeta struct {
		id      string
		name    string
		enabled bool
	}
	var metas []zoneMeta
	for zoneRows.Next() {
		var m zoneMeta
		if err := zoneRows.Scan(&m.id, &m.name, &m.enabled); err != nil {
			zoneRows.Close()
			return nil, err
		}
		metas = append(metas, m)
	}
	zoneRows.Close()
	if err := zoneRows.Err(); err != nil {
		return nil, err
	}

	recordsByZone, err := recordsGroupedByZone(ctx, q, "")
	if err != nil {
		return nil, err
	}
	zones := make([]domain.ZoneState, 0, len(metas))
	for _, m := range metas {
		records := recordsByZone[m.id]
		if records == nil {
			records = []domain.DNSRecord{}
		}
		zones = append(zones, domain.ZoneState{ID: m.id, Name: m.name, Enabled: m.enabled, Records: records})
	}
	return zones, nil
}

func recordsGroupedByZone(ctx context.Context, q querier, zoneID string) (map[string][]domain.DNSRecord, error) {
	query := `select id::text, zone_id::text, name, type::text, value, ttl, enabled, proxied, proxy_settings, comment from dns_records`
	args := []any{}
	if zoneID != "" {
		query += ` where zone_id = $1`
		args = append(args, zoneID)
	}
	query += ` order by name asc`
	rows, err := q.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	grouped := map[string][]domain.DNSRecord{}
	for rows.Next() {
		record, zid, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		grouped[zid] = append(grouped[zid], record)
	}
	return grouped, rows.Err()
}

func scanRecord(row scanner) (domain.DNSRecord, string, error) {
	var (
		record    domain.DNSRecord
		zoneID    string
		valueRaw  []byte
		proxyRaw  []byte
		comment   *string
		recordTyp string
	)
	if err := row.Scan(&record.ID, &zoneID, &record.Name, &recordTyp, &valueRaw, &record.TTL, &record.Enabled, &record.Proxied, &proxyRaw, &comment); err != nil {
		return domain.DNSRecord{}, "", err
	}
	record.Type = recordTyp
	record.Comment = comment
	if len(valueRaw) > 0 {
		if err := json.Unmarshal(valueRaw, &record.Value); err != nil {
			return domain.DNSRecord{}, "", err
		}
	}
	if len(proxyRaw) > 0 && string(proxyRaw) != "null" {
		var proxy map[string]any
		if err := json.Unmarshal(proxyRaw, &proxy); err != nil {
			return domain.DNSRecord{}, "", err
		}
		record.Proxy = proxy
	}
	return record, zoneID, nil
}

func getZone(ctx context.Context, q querier, id string) (domain.ZoneState, error) {
	var zone domain.ZoneState
	err := q.QueryRow(ctx, `select id::text, name, enabled from zones where id = $1`, id).Scan(&zone.ID, &zone.Name, &zone.Enabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ZoneState{}, errors.New("Zone not found")
	}
	if err != nil {
		return domain.ZoneState{}, err
	}
	grouped, err := recordsGroupedByZone(ctx, q, id)
	if err != nil {
		return domain.ZoneState{}, err
	}
	zone.Records = grouped[id]
	if zone.Records == nil {
		zone.Records = []domain.DNSRecord{}
	}
	return zone, nil
}

func scanStream(row scanner) (domain.StreamRoute, error) {
	var (
		route       domain.StreamRoute
		upstreamRaw []byte
	)
	if err := row.Scan(&route.ID, &route.Enabled, &route.Protocol, &route.ListenAddress, &route.ListenPort, &upstreamRaw); err != nil {
		return domain.StreamRoute{}, err
	}
	if len(upstreamRaw) > 0 {
		if err := json.Unmarshal(upstreamRaw, &route.Upstream); err != nil {
			return domain.StreamRoute{}, err
		}
	}
	return route, nil
}

func scanJob(row scanner) (JobRecord, error) {
	var (
		job          JobRecord
		validation   []byte
		applyOut     []byte
		healthOut    []byte
		errorMessage *string
	)
	if err := row.Scan(
		&job.ID, &job.RevisionID, &job.ActorUserID, &job.Target, &job.Status, &job.CorrelationID,
		&job.CreatedAt, &job.ClaimedAt, &job.StartedAt, &job.FinishedAt,
		&validation, &applyOut, &healthOut, &errorMessage,
	); err != nil {
		return JobRecord{}, err
	}
	job.ValidationOutput = rawJSON(validation)
	job.ApplyOutput = rawJSON(applyOut)
	job.HealthOutput = rawJSON(healthOut)
	job.ErrorMessage = errorMessage
	return job, nil
}

func listJobs(ctx context.Context, q querier) ([]JobRecord, error) {
	rows, err := q.Query(ctx, `
		select id::text, revision_id::text, actor_user_id::text, target::text, status::text, correlation_id,
			created_at, claimed_at, started_at, finished_at, validation_output, apply_output, health_output, error_message
		from apply_jobs order by created_at desc
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	jobs := []JobRecord{}
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

func getRevision(ctx context.Context, q querier, id string) (*RevisionRecord, error) {
	var (
		rev         RevisionRecord
		snapshotRaw []byte
	)
	err := q.QueryRow(ctx, `
		select id::text, revision_number, checksum, snapshot, actor_user_id::text, created_at, applied_at
		from config_revisions where id = $1
	`, id).Scan(&rev.ID, &rev.RevisionNumber, &rev.Checksum, &snapshotRaw, &rev.ActorUserID, &rev.CreatedAt, &rev.AppliedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(snapshotRaw) > 0 {
		_ = json.Unmarshal(snapshotRaw, &rev.Snapshot)
	}
	return &rev, nil
}

func readDesiredSnapshot(ctx context.Context, q querier) (domain.ConfigurationSnapshot, error) {
	settingsRow, err := ensureSettings(ctx, q)
	if err != nil {
		return domain.ConfigurationSnapshot{}, err
	}
	settings, err := toSettings(settingsRow)
	if err != nil {
		return domain.ConfigurationSnapshot{}, err
	}
	zones, err := listZones(ctx, q)
	if err != nil {
		return domain.ConfigurationSnapshot{}, err
	}
	streamRows, err := q.Query(ctx, `
		select id::text, enabled, protocol::text, listen_address, listen_port, upstream
		from stream_routes order by listen_port asc
	`)
	if err != nil {
		return domain.ConfigurationSnapshot{}, err
	}
	streams := []domain.StreamRoute{}
	for streamRows.Next() {
		route, err := scanStream(streamRows)
		if err != nil {
			streamRows.Close()
			return domain.ConfigurationSnapshot{}, err
		}
		streams = append(streams, route)
	}
	streamRows.Close()
	if err := streamRows.Err(); err != nil {
		return domain.ConfigurationSnapshot{}, err
	}
	validatedStreams, err := domain.ValidateStreamRoutes(streams)
	if err != nil {
		return domain.ConfigurationSnapshot{}, err
	}
	certificates, err := listCertificatesOrdered(ctx, q, "id")
	if err != nil {
		return domain.ConfigurationSnapshot{}, err
	}
	return domain.ConfigurationSnapshot{
		Settings:     settings,
		Zones:        zones,
		Streams:      validatedStreams,
		Certificates: certificates,
	}, nil
}

func rawJSON(value []byte) json.RawMessage {
	if len(value) == 0 || string(value) == "null" {
		return nil
	}
	return json.RawMessage(value)
}

func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
