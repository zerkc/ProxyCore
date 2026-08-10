package httpserver

import (
	"fmt"
	"net/http"
	"slices"
	"strconv"

	"github.com/zerkc/ProxyCore/apps/api/internal/acme"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
)

// parseRecordMutationBody validates and shapes a DNS record body (ports the Node
// parseRecordMutationBody).
func parseRecordMutationBody(body map[string]any) (domain.DNSRecordInput, error) {
	name, nameOK := body["name"].(string)
	recordType, typeOK := body["type"].(string)
	if !nameOK || !typeOK || !slices.Contains(domain.MvpRecordTypes, recordType) {
		return domain.DNSRecordInput{}, &httpError{status: http.StatusBadRequest, message: "name and a supported record type are required"}
	}
	input := domain.DNSRecordInput{
		Name:    name,
		Type:    recordType,
		Value:   body["value"],
		Enabled: true,
	}
	if id, ok := body["id"].(string); ok {
		input.ID = id
	}
	if ttl, ok := body["ttl"].(float64); ok {
		value := int(ttl)
		input.TTL = &value
	}
	if enabled, ok := body["enabled"].(bool); ok {
		input.Enabled = enabled
	}
	if comment, ok := body["comment"].(string); ok {
		input.Comment = &comment
	}
	if proxied, ok := body["proxied"].(bool); ok {
		input.Proxied = proxied
	}
	if raw, present := body["proxy"]; present {
		proxy, err := parseProxySettingsInput(raw)
		if err != nil {
			return domain.DNSRecordInput{}, err
		}
		input.Proxy = proxy
	}
	return input, nil
}

func parseProxySettingsInput(value any) (map[string]any, error) {
	proxy, ok := value.(map[string]any)
	if !ok || proxy == nil {
		return nil, &httpError{status: http.StatusBadRequest, message: "proxy settings must be an object"}
	}
	origin, ok := proxy["origin"].(map[string]any)
	if !ok || origin == nil {
		return nil, &httpError{status: http.StatusBadRequest, message: "proxy origin is required"}
	}
	ip, ipOK := origin["ip"].(string)
	port, portOK := origin["port"].(float64)
	protocol, protoOK := origin["protocol"].(string)
	if !ipOK || !portOK || !protoOK {
		return nil, &httpError{status: http.StatusBadRequest, message: "proxy origin requires ip, port, and protocol"}
	}
	tlsEnabled, tlsOK := proxy["tlsEnabled"].(bool)
	if !tlsOK {
		return nil, &httpError{status: http.StatusBadRequest, message: "proxy tlsEnabled is required"}
	}
	result := map[string]any{
		"origin":     map[string]any{"ip": ip, "port": port, "protocol": protocol},
		"tlsEnabled": tlsEnabled,
	}
	if v, ok := proxy["redirectHttpToHttps"].(bool); ok {
		result["redirectHttpToHttps"] = v
	}
	if v, ok := proxy["certificateId"].(string); ok {
		result["certificateId"] = v
	}
	if v, ok := proxy["http2"].(bool); ok {
		result["http2"] = v
	}
	if v, ok := proxy["http3"].(bool); ok {
		result["http3"] = v
	}
	if v, ok := proxy["nginxDirectives"].(string); ok {
		result["nginxDirectives"] = v
	}
	if v, ok := proxy["headers"].([]any); ok {
		result["headers"] = v
	}
	if v, ok := proxy["pathRules"].([]any); ok {
		result["pathRules"] = v
	}
	basicAuth, err := parseBasicAuthInput(proxy["basicAuth"])
	if err != nil {
		return nil, err
	}
	if basicAuth != nil {
		result["basicAuth"] = basicAuth
	}
	if v, ok := proxy["websocket"].(bool); ok {
		result["websocket"] = v
	}
	if cache, ok := proxy["cache"].(map[string]any); ok {
		result["cache"] = map[string]any{"enabled": jsTruthy(cache["enabled"])}
	}
	if v, ok := proxy["backendTlsVerify"].(bool); ok {
		result["backendTlsVerify"] = v
	}
	if timeouts, ok := proxy["timeouts"].(map[string]any); ok {
		result["timeouts"] = timeouts
	}
	return result, nil
}

func parseBasicAuthInput(value any) (map[string]any, error) {
	if value == nil {
		return nil, nil
	}
	auth, ok := value.(map[string]any)
	if !ok {
		return nil, &httpError{status: http.StatusBadRequest, message: "basicAuth must be an object"}
	}
	username, ok := auth["username"].(string)
	if !ok {
		return nil, &httpError{status: http.StatusBadRequest, message: "basicAuth username is required"}
	}
	result := map[string]any{"username": username}
	if password, ok := auth["password"].(string); ok {
		result["password"] = password
	}
	if secretID, ok := auth["passwordSecretId"].(string); ok {
		result["passwordSecretId"] = secretID
	}
	return result, nil
}

// parseStreamBody shapes a stream body into a StreamRoute (ports the Node
// parseStreamBody / POST /streams builders).
func parseStreamBody(body map[string]any, streamID string) (domain.StreamRoute, error) {
	route := domain.StreamRoute{ID: streamID, Enabled: true}
	if enabled, ok := body["enabled"].(bool); ok {
		route.Enabled = enabled
	}
	if body["protocol"] == "udp" {
		route.Protocol = "udp"
	} else {
		route.Protocol = "tcp"
	}
	route.ListenAddress = jsString(body["listenAddress"])
	route.ListenPort = int(jsNumber(body["listenPort"]))
	upstream := domain.UpstreamTarget{}
	if err := decodeInto(body["upstream"], &upstream); err != nil {
		return domain.StreamRoute{}, &httpError{status: http.StatusBadRequest, message: "stream upstream is invalid"}
	}
	route.Upstream = upstream
	return route, nil
}

// mergeStreamBody merges a PATCH body over an existing stream, mirroring Node.
func mergeStreamBody(body map[string]any, existing domain.StreamRoute) (domain.StreamRoute, error) {
	merged := map[string]any{}
	if enabled, ok := body["enabled"].(bool); ok {
		merged["enabled"] = enabled
	} else {
		merged["enabled"] = existing.Enabled
	}
	if v, ok := body["protocol"]; ok && v != nil {
		merged["protocol"] = v
	} else {
		merged["protocol"] = existing.Protocol
	}
	if v, ok := body["listenAddress"]; ok && v != nil {
		merged["listenAddress"] = v
	} else {
		merged["listenAddress"] = existing.ListenAddress
	}
	if v, ok := body["listenPort"]; ok && v != nil {
		merged["listenPort"] = v
	} else {
		merged["listenPort"] = float64(existing.ListenPort)
	}
	if v, ok := body["upstream"]; ok && v != nil {
		merged["upstream"] = v
	} else {
		merged["upstream"] = existing.Upstream
	}
	return parseStreamBody(merged, existing.ID)
}

func globalHTTP01Get(token string) (string, bool) {
	return acme.GlobalHttp01Store().Get(token)
}

// jsTruthy mirrors JavaScript's Boolean() coercion for JSON-decoded values.
func jsTruthy(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case bool:
		return v
	case float64:
		return v != 0
	case string:
		return v != ""
	default:
		return true
	}
}

// jsString mirrors String(value ?? "") for JSON-decoded values.
func jsString(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return v
	case bool:
		return strconv.FormatBool(v)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	default:
		return fmt.Sprint(v)
	}
}

// jsNumber mirrors Number(value) for JSON-decoded values.
func jsNumber(value any) float64 {
	switch v := value.(type) {
	case float64:
		return v
	case bool:
		if v {
			return 1
		}
		return 0
	case string:
		parsed, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return 0
		}
		return parsed
	default:
		return 0
	}
}
