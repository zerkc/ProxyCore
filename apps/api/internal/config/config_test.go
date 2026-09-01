package config_test

import (
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/config"
)

func TestLoadSecureCookiesOptIn(t *testing.T) {
	t.Setenv("NODE_ENV", "production")
	t.Setenv("PROXYCORE_SECURE_COOKIES", "")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SecureCookies {
		t.Fatalf("SecureCookies should default false for HTTP homelab access, got true")
	}

	t.Setenv("PROXYCORE_SECURE_COOKIES", "1")
	cfg, err = config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.SecureCookies {
		t.Fatalf("SecureCookies=1 should enable Secure cookies")
	}
}

func TestLoadUpdateCheckConfig(t *testing.T) {
	t.Setenv("PROXYCORE_UPDATE_CHECK_ENABLED", "")
	t.Setenv("PROXYCORE_UPDATE_CHECK_INTERVAL", "")
	t.Setenv("PROXYCORE_UPDATE_CHECK_TIMEOUT", "")

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.UpdateCheckEnabled {
		t.Fatal("update checks should be enabled by default")
	}
	if cfg.UpdateCheckInterval != 6*time.Hour {
		t.Fatalf("UpdateCheckInterval=%s, want 6h", cfg.UpdateCheckInterval)
	}
	if cfg.UpdateCheckTimeout != 5*time.Second {
		t.Fatalf("UpdateCheckTimeout=%s, want 5s", cfg.UpdateCheckTimeout)
	}

	t.Setenv("PROXYCORE_UPDATE_CHECK_ENABLED", "0")
	t.Setenv("PROXYCORE_UPDATE_CHECK_INTERVAL", "2h")
	t.Setenv("PROXYCORE_UPDATE_CHECK_TIMEOUT", "750ms")
	cfg, err = config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.UpdateCheckEnabled {
		t.Fatal("PROXYCORE_UPDATE_CHECK_ENABLED=0 should disable update checks")
	}
	if cfg.UpdateCheckInterval != 2*time.Hour {
		t.Fatalf("UpdateCheckInterval=%s, want 2h", cfg.UpdateCheckInterval)
	}
	if cfg.UpdateCheckTimeout != 750*time.Millisecond {
		t.Fatalf("UpdateCheckTimeout=%s, want 750ms", cfg.UpdateCheckTimeout)
	}
}
