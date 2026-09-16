package main

import "testing"

func TestLoadConfigDefaultsIncludeNginxRecoveryOwners(t *testing.T) {
	t.Setenv("PROXYCORE_UPDATER_SERVICES", "")
	t.Setenv("PROXYCORE_HEALTH_TIMEOUT", "90s")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig returned error: %v", err)
	}

	want := []string{"api", "worker", "control", "nginx"}
	if len(cfg.Services) != len(want) {
		t.Fatalf("services=%v want %v", cfg.Services, want)
	}
	for i, service := range want {
		if cfg.Services[i] != service {
			t.Errorf("services[%d]=%q want %q", i, cfg.Services[i], service)
		}
	}
}
