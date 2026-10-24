package config_test

import (
	"testing"

	"github.com/zerkc/ProxyCore/apps/api/internal/renderers"
	"github.com/zerkc/ProxyCore/apps/api/internal/worker/config"
)

func lookup(env map[string]string) config.Lookup {
	return func(key string) (string, bool) {
		value, ok := env[key]
		return value, ok
	}
}

func TestLoadAppliesDefaults(t *testing.T) {
	got, err := config.Load(lookup(map[string]string{"PROXYCORE_SQLITE_PATH": "/data/db.sqlite"}))
	if err != nil {
		t.Fatal(err)
	}
	want := config.Config{
		SQLitePath: "/data/db.sqlite", SocketPath: "/run/proxycore/control.sock",
		HeartbeatPath: "/run/proxycore/worker.heartbeat", CandidateRoot: "/var/lib/proxycore/candidates",
		ReconciliationIntervalMs: 300000, JobLeaseMs: 120000,
	}
	if got != want {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

func TestLoadRequiresTheSQLitePathAndRejectsEmptySocketPath(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"missing":      {},
		"empty":        {"PROXYCORE_SQLITE_PATH": ""},
		"empty socket": {"PROXYCORE_SQLITE_PATH": "/db", "WORKER_SOCKET_PATH": ""},
	} {
		if _, err := config.Load(lookup(env)); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	_, err := config.Load(lookup(nil))
	if err == nil || err.Error() != "PROXYCORE_SQLITE_PATH is required" {
		t.Fatalf("err = %v", err)
	}
}

func TestLoadReadsEveryVariable(t *testing.T) {
	got, err := config.Load(lookup(map[string]string{
		"PROXYCORE_SQLITE_PATH": "/db", "PROXYCORE_MASTER_KEY_BASE64": "a2V5",
		"WORKER_SOCKET_PATH": "/s.sock", "WORKER_HEARTBEAT_PATH": "/hb", "WORKER_CANDIDATE_ROOT": "/c",
		"WORKER_RECONCILIATION_INTERVAL_MS": "1500", "WORKER_JOB_LEASE_MS": "9000",
		"NGINX_ACME_UPSTREAM": "http://api:3000", "NGINX_HTTP3_MODULE": "1",
		"NGINX_TCP_443_PUBLISHED": "1",
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := config.Config{
		SQLitePath: "/db", MasterKeyBase64: "a2V5", SocketPath: "/s.sock", HeartbeatPath: "/hb", CandidateRoot: "/c",
		ReconciliationIntervalMs: 1500, JobLeaseMs: 9000, ACMEUpstream: "http://api:3000",
		Capabilities: renderers.Http3Capabilities{Http3Module: true, TCP443Published: true},
	}
	if got != want {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

func TestLoadHostNetworkPublishesBothPortsAndOtherFlagValuesAreOff(t *testing.T) {
	host, _ := config.Load(lookup(map[string]string{"PROXYCORE_SQLITE_PATH": "/db", "NGINX_NETWORK_MODE": "host"}))
	if !host.Capabilities.TCP443Published || !host.Capabilities.UDP443Published || host.Capabilities.Http3Module {
		t.Fatalf("host = %+v", host.Capabilities)
	}
	other, _ := config.Load(lookup(map[string]string{
		"PROXYCORE_SQLITE_PATH": "/db", "NGINX_NETWORK_MODE": "bridge", "NGINX_HTTP3_MODULE": "true", "NGINX_TCP_443_PUBLISHED": "yes",
	}))
	if other.Capabilities != (renderers.Http3Capabilities{}) {
		t.Fatalf("other = %+v", other.Capabilities)
	}
}

func TestReconciliationIntervalFallbacks(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want int64
	}{
		{"legacy poll interval", map[string]string{"WORKER_POLL_INTERVAL_MS": "7000"}, 7000},
		{"reconciliation wins", map[string]string{"WORKER_RECONCILIATION_INTERVAL_MS": "2000", "WORKER_POLL_INTERVAL_MS": "7000"}, 2000},
		{"set but empty does not fall through", map[string]string{"WORKER_RECONCILIATION_INTERVAL_MS": "", "WORKER_POLL_INTERVAL_MS": "7000"}, 300000},
		{"zero", map[string]string{"WORKER_RECONCILIATION_INTERVAL_MS": "0"}, 300000},
		{"negative", map[string]string{"WORKER_RECONCILIATION_INTERVAL_MS": "-5"}, 300000},
		{"fraction", map[string]string{"WORKER_RECONCILIATION_INTERVAL_MS": "1.5"}, 300000},
		{"garbage", map[string]string{"WORKER_RECONCILIATION_INTERVAL_MS": "soon"}, 300000},
		{"infinity", map[string]string{"WORKER_RECONCILIATION_INTERVAL_MS": "Infinity"}, 300000},
		{"exponent", map[string]string{"WORKER_RECONCILIATION_INTERVAL_MS": "1e3"}, 1000},
		{"whitespace", map[string]string{"WORKER_RECONCILIATION_INTERVAL_MS": " 4000 "}, 4000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.env["PROXYCORE_SQLITE_PATH"] = "/db"
			got, err := config.Load(lookup(tt.env))
			if err != nil || got.ReconciliationIntervalMs != tt.want {
				t.Fatalf("interval = %d, %v; want %d", got.ReconciliationIntervalMs, err, tt.want)
			}
		})
	}
}
