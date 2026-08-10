package config_test

import (
	"testing"

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
