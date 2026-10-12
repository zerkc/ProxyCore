package configuration

import (
	"encoding/json"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
)

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
		&job.ID, &job.RevisionID, &job.ActorUserID, &job.Target, &job.Status,
		&job.Source, &job.SourcePrimaryID, &job.SourceNodeID, &job.SourceRevisionID,
		&job.SnapshotContentHash, &job.SnapshotVersion, &job.ReplicationVersion,
		&job.LeadershipGeneration, &job.CorrelationID,
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

// SettingsPatch carries a partial settings update.
type SettingsPatch struct {
	Ingress             *domain.Ingress
	DefaultPool         *domain.ResolverPool
	ForwardingRules     []domain.ForwardingRule
	RetentionMaxAgeDays *int
	RetentionMaxSizeMb  *int
}
