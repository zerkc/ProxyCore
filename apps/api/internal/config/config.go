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
	// NodeAPIURL is the transitional Node configuration API (no Next.js).
	// When set, unmatched /api/* routes are reverse-proxied there.
	NodeAPIURL string
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
		SecureCookies:     os.Getenv("NODE_ENV") == "production" || os.Getenv("PROXYCORE_SECURE_COOKIES") == "1",
		NodeAPIURL:        strings.TrimRight(env("PROXYCORE_NODE_API_URL", ""), "/"),
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
