package config_test

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/config"
)

func TestLoadEnrollmentTLSAddr(t *testing.T) {
	unsetEnv(t, "PROXYCORE_ENROLLMENT_TLS_ADDR")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.EnrollmentTLSAddr != ":3443" {
		t.Fatalf("EnrollmentTLSAddr=%q, want default :3443", cfg.EnrollmentTLSAddr)
	}

	t.Setenv("PROXYCORE_ENROLLMENT_TLS_ADDR", "  127.0.0.1:9443  ")
	cfg, err = config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.EnrollmentTLSAddr != "127.0.0.1:9443" {
		t.Fatalf("EnrollmentTLSAddr=%q, want trimmed configured address", cfg.EnrollmentTLSAddr)
	}
}

func TestLoadRejectsEmptyEnrollmentTLSAddr(t *testing.T) {
	for _, value := range []string{"", " \t"} {
		t.Run("empty value", func(t *testing.T) {
			t.Setenv("PROXYCORE_ENROLLMENT_TLS_ADDR", value)
			_, err := config.Load()
			if err == nil || !strings.Contains(err.Error(), "PROXYCORE_ENROLLMENT_TLS_ADDR") {
				t.Fatalf("Load() error=%v, want named empty-address error", err)
			}
		})
	}
}

func unsetEnv(t *testing.T, key string) {
	t.Helper()
	value, present := os.LookupEnv(key)
	if err := os.Unsetenv(key); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if present {
			_ = os.Setenv(key, value)
		} else {
			_ = os.Unsetenv(key)
		}
	})
}

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
