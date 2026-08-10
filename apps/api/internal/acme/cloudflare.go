package acme

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
)

// Dns01Adapter presents and cleans up DNS-01 challenge records.
type Dns01Adapter interface {
	Present(ctx context.Context, hostname, value string) error
	Cleanup(ctx context.Context, hostname, value string) error
}

// CloudflareOptions configures the Cloudflare DNS-01 adapter.
type CloudflareOptions struct {
	APIToken string
	ZoneID   string
	ZoneName string
	Client   *http.Client
}

// CloudflareDns01Adapter manages _acme-challenge TXT records via Cloudflare.
type CloudflareDns01Adapter struct {
	options CloudflareOptions
	client  *http.Client
	zones   map[string]cloudflareZone
}

type cloudflareZone struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type cloudflareRecord struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Content string `json:"content"`
}

type cloudflareResponse[T any] struct {
	Success bool               `json:"success"`
	Result  cloudflareResult[T] `json:"result"`
	Errors  []cloudflareErr    `json:"errors"`
}

// cloudflareResult accepts Cloudflare's result as either a single object (POST/DELETE)
// or an array (list endpoints).
type cloudflareResult[T any] []T

func (r *cloudflareResult[T]) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		*r = nil
		return nil
	}
	if trimmed[0] == '[' {
		var items []T
		if err := json.Unmarshal(trimmed, &items); err != nil {
			return err
		}
		*r = items
		return nil
	}
	var item T
	if err := json.Unmarshal(trimmed, &item); err != nil {
		return err
	}
	*r = []T{item}
	return nil
}

type cloudflareErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// NewCloudflareDns01Adapter builds a Cloudflare DNS-01 adapter.
func NewCloudflareDns01Adapter(options CloudflareOptions) *CloudflareDns01Adapter {
	client := options.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &CloudflareDns01Adapter{options: options, client: client, zones: map[string]cloudflareZone{}}
}

// Present creates the challenge TXT record.
func (a *CloudflareDns01Adapter) Present(ctx context.Context, hostname, value string) error {
	zone, err := a.resolveZone(ctx, hostname)
	if err != nil {
		return err
	}
	name, err := challengeRecordName(hostname, zone.Name)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]any{"type": "TXT", "name": name, "content": value, "ttl": 120})
	_, err = a.request(ctx, http.MethodPost, "/dns_records", body, zone.ID)
	return err
}

// Cleanup removes matching challenge TXT records.
func (a *CloudflareDns01Adapter) Cleanup(ctx context.Context, hostname, value string) error {
	zone, err := a.resolveZone(ctx, hostname)
	if err != nil {
		return err
	}
	name, err := challengeRecordName(hostname, zone.Name)
	if err != nil {
		return err
	}
	records, err := a.request(ctx, http.MethodGet, "/dns_records?type=TXT&name="+url.QueryEscape(name), nil, zone.ID)
	if err != nil {
		return err
	}
	for _, record := range records {
		if record.Type == "TXT" && record.Content == value {
			if _, err := a.request(ctx, http.MethodDelete, "/dns_records/"+record.ID, nil, zone.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

func (a *CloudflareDns01Adapter) resolveZone(ctx context.Context, hostname string) (cloudflareZone, error) {
	normalizedHostname, err := domain.NormalizeDNSName(strings.TrimPrefix(hostname, "*."), false)
	if err != nil {
		return cloudflareZone{}, err
	}
	if cached, ok := a.zones[normalizedHostname]; ok {
		return cached, nil
	}
	configuredZoneID := strings.TrimSpace(a.options.ZoneID)
	configuredZoneName := strings.TrimSpace(a.options.ZoneName)
	if configuredZoneID != "" && configuredZoneName != "" {
		zoneName, err := domain.NormalizeDNSName(configuredZoneName, false)
		if err != nil {
			return cloudflareZone{}, err
		}
		zone := cloudflareZone{ID: configuredZoneID, Name: zoneName}
		a.zones[normalizedHostname] = zone
		return zone, nil
	}

	labels := strings.Split(normalizedHostname, ".")
	for index := 0; index < len(labels)-1; index++ {
		candidate := strings.Join(labels[index:], ".")
		if configuredZoneName != "" && !strings.EqualFold(candidate, configuredZoneName) {
			continue
		}
		zones, err := a.requestZones(ctx, candidate)
		if err != nil {
			return cloudflareZone{}, err
		}
		for _, zone := range zones {
			if strings.EqualFold(zone.Name, candidate) {
				resolved := cloudflareZone{ID: zone.ID, Name: zone.Name}
				if configuredZoneID != "" {
					resolved.ID = configuredZoneID
				}
				a.zones[normalizedHostname] = resolved
				return resolved, nil
			}
		}
	}
	return cloudflareZone{}, fmt.Errorf("Cloudflare zone could not be found for %s", hostname)
}

func (a *CloudflareDns01Adapter) requestZones(ctx context.Context, name string) ([]cloudflareZone, error) {
	body, err := a.do(ctx, http.MethodGet, "https://api.cloudflare.com/client/v4/zones?name="+url.QueryEscape(name)+"&status=active&per_page=50", nil)
	if err != nil {
		return nil, err
	}
	var parsed cloudflareResponse[cloudflareZone]
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, err
	}
	if !parsed.Success {
		return nil, errors.New("Cloudflare DNS-01 provider rejected the request" + formatCloudflareErrors(parsed.Errors))
	}
	return parsed.Result, nil
}

func (a *CloudflareDns01Adapter) request(ctx context.Context, method, path string, body []byte, zoneID string) ([]cloudflareRecord, error) {
	base := "https://api.cloudflare.com/client/v4"
	if zoneID != "" {
		base += "/zones/" + zoneID
	}
	raw, err := a.do(ctx, method, base+path, body)
	if err != nil {
		return nil, err
	}
	var parsed cloudflareResponse[cloudflareRecord]
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, err
	}
	if !parsed.Success {
		return nil, errors.New("Cloudflare DNS-01 provider rejected the request" + formatCloudflareErrors(parsed.Errors))
	}
	return parsed.Result, nil
}

func (a *CloudflareDns01Adapter) do(ctx context.Context, method, endpoint string, body []byte) ([]byte, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+a.options.APIToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var parsed cloudflareResponse[json.RawMessage]
		_ = json.Unmarshal(raw, &parsed)
		return nil, fmt.Errorf("Cloudflare DNS-01 request failed (%d)%s", resp.StatusCode, formatCloudflareErrors(parsed.Errors))
	}
	return raw, nil
}

func formatCloudflareErrors(errs []cloudflareErr) string {
	if len(errs) == 0 {
		return ""
	}
	details := make([]string, 0, len(errs))
	for _, e := range errs {
		message := e.Message
		if message == "" {
			message = "Unknown provider error"
		}
		if e.Code != 0 {
			details = append(details, fmt.Sprintf("[%d] %s", e.Code, message))
		} else {
			details = append(details, message)
		}
	}
	return ": " + strings.Join(details, "; ")
}

func challengeRecordName(hostname, zoneName string) (string, error) {
	raw := strings.TrimRight(strings.ToLower(strings.TrimSpace(hostname)), ".")
	hostWithoutPrefix := strings.TrimPrefix(raw, "_acme-challenge.")
	normalizedHost, err := domain.NormalizeDNSName(hostWithoutPrefix, false)
	if err != nil {
		return "", err
	}
	zone, err := domain.NormalizeDNSName(zoneName, false)
	if err != nil {
		return "", err
	}
	if normalizedHost != zone && !strings.HasSuffix(normalizedHost, "."+zone) {
		return "", errors.New("DNS-01 adapter only accepts _acme-challenge records in its zone")
	}
	return "_acme-challenge." + normalizedHost, nil
}
