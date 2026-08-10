package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Addr              string
	DatabaseURL       string
	UIDist            string
	MasterKeyBase64   string
	SessionCookieName string
	SessionTTL        time.Duration
	SecureCookies     bool
	// ProxyIngress addresses seed the configuration store's ingress on first use.
	ProxyIngressIPv4 string
	ProxyIngressIPv6 string
	// ACME directory URLs for Let's Encrypt issuance (staging vs production).
	ACMEDirectoryURL           string
	ACMEProductionDirectoryURL string
	// Optional ACME contact email for issuance and automatic renewal.
	AcmeEmail string
	// How often the API scans for Let's Encrypt certificates due for renewal.
	CertRenewalInterval time.Duration
}

func Load() (Config, error) {
	sessionTTLSeconds, err := envInt("SESSION_TTL_SECONDS", 28_800)
	if err != nil {
		return Config{}, err
	}
	cfg := Config{
		Addr:              env("PROXYCORE_API_ADDR", ":3000"),
		DatabaseURL:       os.Getenv("DATABASE_URL"),
		UIDist:            env("PROXYCORE_UI_DIST", "apps/ui/dist"),
		MasterKeyBase64:   os.Getenv("PROXYCORE_MASTER_KEY_BASE64"),
		SessionCookieName: env("SESSION_COOKIE_NAME", "proxycore_session"),
		SessionTTL:        time.Duration(sessionTTLSeconds) * time.Second,
		// Homelab UI is often reached over plain HTTP (LAN IP). Secure cookies
		// are opt-in so sessions work without TLS; set PROXYCORE_SECURE_COOKIES=1
		// when terminating HTTPS in front of the API.
		SecureCookies:              envBool("PROXYCORE_SECURE_COOKIES", false),
		ProxyIngressIPv4:           strings.TrimSpace(os.Getenv("PROXY_INGRESS_IPV4")),
		ProxyIngressIPv6:           strings.TrimSpace(os.Getenv("PROXY_INGRESS_IPV6")),
		ACMEDirectoryURL:           env("ACME_DIRECTORY_URL", "https://acme-staging-v02.api.letsencrypt.org/directory"),
		ACMEProductionDirectoryURL: env("ACME_PRODUCTION_DIRECTORY_URL", "https://acme-v02.api.letsencrypt.org/directory"),
		AcmeEmail:                  strings.TrimSpace(os.Getenv("PROXYCORE_ACME_EMAIL")),
		CertRenewalInterval:        envDuration("PROXYCORE_CERT_RENEWAL_INTERVAL", time.Hour),
	}
	if !strings.HasPrefix(cfg.Addr, ":") && !strings.Contains(cfg.Addr, ":") {
		port, err := strconv.Atoi(cfg.Addr)
		if err != nil {
			return Config{}, fmt.Errorf("PROXYCORE_API_ADDR: %w", err)
		}
		cfg.Addr = fmt.Sprintf(":%d", port)
	}
	return cfg, nil
}

func env(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func envInt(key string, fallback int) (int, error) {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	if parsed <= 0 {
		return 0, fmt.Errorf("%s must be positive", key)
	}
	return parsed, nil
}

func envBool(key string, fallback bool) bool {
	value := strings.TrimSpace(strings.ToLower(os.Getenv(key)))
	switch value {
	case "":
		return fallback
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}

func envDuration(key string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}
