// Package config reads the worker's environment, mirroring loadConfig in
// packages/config and the WORKER_* parsing in apps/worker/src/index.ts.
package config

import (
	"errors"
	"math"
	"strconv"
	"strings"

	"github.com/zerkc/ProxyCore/apps/api/internal/renderers"
)

// Defaults for unset variables.
const (
	DefaultSocketPath               = "/run/proxycore/control.sock"
	DefaultHeartbeatPath            = "/run/proxycore/worker.heartbeat"
	DefaultCandidateRoot            = "/var/lib/proxycore/candidates"
	DefaultReconciliationIntervalMs = 300_000
	DefaultJobLeaseMs               = 120_000
)

// Config is the worker's resolved configuration.
type Config struct {
	SQLitePath string
	// MasterKeyBase64 is empty when no secret store should be wired.
	MasterKeyBase64          string
	SocketPath               string
	HeartbeatPath            string
	CandidateRoot            string
	ReconciliationIntervalMs int64
	JobLeaseMs               int64
	ACMEUpstream             string
	Capabilities             renderers.Http3Capabilities
}

// Lookup reads one environment variable and reports whether it is set.
type Lookup func(key string) (string, bool)

// Load resolves the configuration. PROXYCORE_SQLITE_PATH is required; the
// reconciliation interval falls back to WORKER_POLL_INTERVAL_MS only when
// WORKER_RECONCILIATION_INTERVAL_MS is unset (TS `??`), and an unusable number
// falls back to the default.
func Load(lookup Lookup) (Config, error) {
	get := func(key, fallback string) (string, error) {
		value, set := lookup(key)
		if !set {
			return fallback, nil
		}
		if value == "" {
			return "", errors.New(key + " must not be empty")
		}
		return value, nil
	}
	sqlitePath, set := lookup("PROXYCORE_SQLITE_PATH")
	if !set || sqlitePath == "" {
		return Config{}, errors.New("PROXYCORE_SQLITE_PATH is required")
	}
	socketPath, err := get("WORKER_SOCKET_PATH", DefaultSocketPath)
	if err != nil {
		return Config{}, err
	}
	masterKey, _ := lookup("PROXYCORE_MASTER_KEY_BASE64")
	heartbeatPath, _ := orSet(lookup, "WORKER_HEARTBEAT_PATH", DefaultHeartbeatPath)
	candidateRoot, _ := orSet(lookup, "WORKER_CANDIDATE_ROOT", DefaultCandidateRoot)
	interval, intervalSet := lookup("WORKER_RECONCILIATION_INTERVAL_MS")
	if !intervalSet {
		interval, _ = lookup("WORKER_POLL_INTERVAL_MS")
	}
	lease, _ := lookup("WORKER_JOB_LEASE_MS")
	acme, _ := lookup("NGINX_ACME_UPSTREAM")
	host := flag(lookup, "NGINX_NETWORK_MODE", "host")
	return Config{
		SQLitePath:               sqlitePath,
		MasterKeyBase64:          masterKey,
		SocketPath:               socketPath,
		HeartbeatPath:            heartbeatPath,
		CandidateRoot:            candidateRoot,
		ReconciliationIntervalMs: positiveInteger(interval, DefaultReconciliationIntervalMs),
		JobLeaseMs:               positiveInteger(lease, DefaultJobLeaseMs),
		ACMEUpstream:             acme,
		Capabilities: renderers.Http3Capabilities{
			Http3Module:     flag(lookup, "NGINX_HTTP3_MODULE", "1"),
			TCP443Published: host || flag(lookup, "NGINX_TCP_443_PUBLISHED", "1"),
			UDP443Published: host || flag(lookup, "NGINX_UDP_443_PUBLISHED", "1"),
		},
	}, nil
}

// orSet returns the variable when set (even empty, like TS `??`), else the fallback.
func orSet(lookup Lookup, key, fallback string) (string, bool) {
	if value, set := lookup(key); set {
		return value, true
	}
	return fallback, false
}

func flag(lookup Lookup, key, want string) bool {
	value, _ := lookup(key)
	return value == want
}

// positiveInteger parses like `Number(value)` followed by an Number.isInteger
// and > 0 check; anything else yields the fallback.
func positiveInteger(value string, fallback int64) int64 {
	parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) || parsed != math.Trunc(parsed) ||
		parsed <= 0 || parsed > 1<<53 {
		return fallback
	}
	return int64(parsed)
}
