package httpserver

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
)

func TestParseRecordMutationBody(t *testing.T) {
	t.Run("valid A record with defaults", func(t *testing.T) {
		record, err := parseRecordMutationBody(map[string]any{
			"name":  "www",
			"type":  "A",
			"value": "10.0.0.5",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if record.Name != "www" || record.Type != "A" || record.Value != "10.0.0.5" {
			t.Fatalf("record=%+v", record)
		}
		if !record.Enabled {
			t.Fatal("enabled should default to true")
		}
		if record.Proxied {
			t.Fatal("proxied should default to false")
		}
		if record.TTL != nil {
			t.Fatalf("ttl should be nil, got %v", *record.TTL)
		}
	})

	t.Run("ttl and enabled overrides", func(t *testing.T) {
		record, err := parseRecordMutationBody(map[string]any{
			"name":    "www",
			"type":    "AAAA",
			"value":   "::1",
			"ttl":     float64(600),
			"enabled": false,
			"comment": "note",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if record.TTL == nil || *record.TTL != 600 {
			t.Fatalf("ttl=%v", record.TTL)
		}
		if record.Enabled {
			t.Fatal("enabled override ignored")
		}
		if record.Comment == nil || *record.Comment != "note" {
			t.Fatalf("comment=%v", record.Comment)
		}
	})

	t.Run("missing name", func(t *testing.T) {
		_, err := parseRecordMutationBody(map[string]any{"type": "A"})
		assertHTTPStatus(t, err, http.StatusBadRequest)
	})

	t.Run("unsupported type", func(t *testing.T) {
		_, err := parseRecordMutationBody(map[string]any{"name": "www", "type": "NS"})
		assertHTTPStatus(t, err, http.StatusBadRequest)
	})

	t.Run("null proxy is rejected", func(t *testing.T) {
		_, err := parseRecordMutationBody(map[string]any{"name": "www", "type": "A", "proxy": nil})
		assertHTTPStatus(t, err, http.StatusBadRequest)
	})
}

func TestParseProxySettingsInput(t *testing.T) {
	valid := map[string]any{
		"origin":     map[string]any{"ip": "10.0.0.5", "port": float64(8080), "protocol": "http"},
		"tlsEnabled": true,
		"http2":      true,
		"cache":      map[string]any{"enabled": 1.0},
	}
	result, err := parseProxySettingsInput(valid)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result["tlsEnabled"] != true {
		t.Fatalf("tlsEnabled=%v", result["tlsEnabled"])
	}
	if result["http2"] != true {
		t.Fatalf("http2=%v", result["http2"])
	}
	cache, ok := result["cache"].(map[string]any)
	if !ok || cache["enabled"] != true {
		t.Fatalf("cache=%v", result["cache"])
	}

	t.Run("missing origin", func(t *testing.T) {
		_, err := parseProxySettingsInput(map[string]any{"tlsEnabled": true})
		assertHTTPStatus(t, err, http.StatusBadRequest)
	})
	t.Run("origin missing port", func(t *testing.T) {
		_, err := parseProxySettingsInput(map[string]any{
			"origin":     map[string]any{"ip": "10.0.0.5", "protocol": "http"},
			"tlsEnabled": true,
		})
		assertHTTPStatus(t, err, http.StatusBadRequest)
	})
	t.Run("missing tlsEnabled", func(t *testing.T) {
		_, err := parseProxySettingsInput(map[string]any{
			"origin": map[string]any{"ip": "10.0.0.5", "port": float64(80), "protocol": "http"},
		})
		assertHTTPStatus(t, err, http.StatusBadRequest)
	})
	t.Run("not an object", func(t *testing.T) {
		_, err := parseProxySettingsInput("nope")
		assertHTTPStatus(t, err, http.StatusBadRequest)
	})
}

func TestParseBasicAuthInput(t *testing.T) {
	t.Run("nil is omitted", func(t *testing.T) {
		result, err := parseBasicAuthInput(nil)
		if err != nil || result != nil {
			t.Fatalf("result=%v err=%v", result, err)
		}
	})
	t.Run("username and password", func(t *testing.T) {
		result, err := parseBasicAuthInput(map[string]any{"username": "admin", "password": "secret"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if result["username"] != "admin" || result["password"] != "secret" {
			t.Fatalf("result=%v", result)
		}
	})
	t.Run("missing username", func(t *testing.T) {
		_, err := parseBasicAuthInput(map[string]any{"password": "secret"})
		assertHTTPStatus(t, err, http.StatusBadRequest)
	})
	t.Run("array is rejected", func(t *testing.T) {
		_, err := parseBasicAuthInput([]any{"admin"})
		assertHTTPStatus(t, err, http.StatusBadRequest)
	})
}

func TestParseStreamBody(t *testing.T) {
	route, err := parseStreamBody(map[string]any{
		"protocol":      "udp",
		"listenAddress": "0.0.0.0",
		"listenPort":    float64(5353),
		"upstream":      map[string]any{"ip": "10.0.0.2", "port": float64(53), "protocol": "udp"},
	}, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if route.Protocol != "udp" || route.ListenPort != 5353 || !route.Enabled {
		t.Fatalf("route=%+v", route)
	}
	if route.Upstream.IP != "10.0.0.2" || route.Upstream.Port != 53 {
		t.Fatalf("upstream=%+v", route.Upstream)
	}

	t.Run("defaults protocol to tcp and enabled true", func(t *testing.T) {
		route, err := parseStreamBody(map[string]any{"listenPort": float64(443)}, "id-1")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if route.Protocol != "tcp" || !route.Enabled || route.ID != "id-1" {
			t.Fatalf("route=%+v", route)
		}
	})
}

func TestMergeStreamBody(t *testing.T) {
	existing := domain.StreamRoute{
		ID:            "stream-1",
		Enabled:       true,
		Protocol:      "tcp",
		ListenAddress: "0.0.0.0",
		ListenPort:    8443,
		Upstream:      domain.UpstreamTarget{IP: "10.0.0.9", Port: 443, Protocol: "tcp"},
	}
	route, err := mergeStreamBody(map[string]any{"enabled": false}, existing)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if route.Enabled {
		t.Fatal("enabled should be overridden to false")
	}
	if route.ListenPort != existing.ListenPort {
		t.Fatalf("listenPort should be preserved, got %d", route.ListenPort)
	}
	if route.Protocol != existing.Protocol {
		t.Fatalf("protocol should be preserved, got %s", route.Protocol)
	}
	if route.Upstream != existing.Upstream {
		t.Fatalf("upstream should be preserved, got %+v", route.Upstream)
	}
	if route.ID != existing.ID {
		t.Fatalf("id should be preserved, got %s", route.ID)
	}
}

func TestInferRequestIngress(t *testing.T) {
	cases := []struct {
		host     string
		wantIPv4 string
		wantIPv6 string
	}{
		{"10.0.0.5:3000", "10.0.0.5", ""},
		{"192.168.1.10", "192.168.1.10", ""},
		{"172.16.0.1:80", "172.16.0.1", ""},
		{"127.0.0.1:3000", "", ""},
		{"8.8.8.8", "", ""},
		{"0.0.0.0", "", ""},
		{"[fd00::1]:3000", "", "fd00::1"},
		{"[::1]:3000", "", ""},
		{"example.com:3000", "", ""},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodGet, "http://placeholder/api/status", nil)
		req.Host = tc.host
		got := inferRequestIngress(req)
		if got.IPv4 != tc.wantIPv4 || got.IPv6 != tc.wantIPv6 {
			t.Fatalf("host=%q got=%+v want ipv4=%q ipv6=%q", tc.host, got, tc.wantIPv4, tc.wantIPv6)
		}
	}
}

func TestJSHelpers(t *testing.T) {
	if !jsTruthy(1.0) || jsTruthy(0.0) || jsTruthy("") || !jsTruthy("x") || jsTruthy(nil) {
		t.Fatal("jsTruthy mismatch")
	}
	if jsString(nil) != "" || jsString("host") != "host" || jsString(true) != "true" {
		t.Fatal("jsString mismatch")
	}
	if jsNumber("42") != 42 || jsNumber(3.0) != 3 || jsNumber("bad") != 0 {
		t.Fatal("jsNumber mismatch")
	}
}

func assertHTTPStatus(t *testing.T, err error, status int) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error with status %d, got nil", status)
	}
	he, ok := err.(*httpError)
	if !ok {
		t.Fatalf("expected *httpError, got %T (%v)", err, err)
	}
	if he.status != status {
		t.Fatalf("status=%d want=%d (%s)", he.status, status, he.message)
	}
}
