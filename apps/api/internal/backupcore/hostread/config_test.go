package hostread

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestConfigApplyDefaults(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}

	tests := []struct {
		name          string
		home          string
		envPath       string
		candidateRoot string
		wantEnvPath   string
		wantRoot      string
		wantNowNonNil bool
	}{
		{
			name:          "host home defaults both paths",
			home:          filepath.Join(string(filepath.Separator), "srv", "proxycore"),
			wantEnvPath:   filepath.Join(string(filepath.Separator), "srv", "proxycore", ".env"),
			wantRoot:      filepath.Join(string(filepath.Separator), "srv", "proxycore", "candidates"),
			wantNowNonNil: true,
		},
		{
			name:          "working directory is fallback",
			wantEnvPath:   filepath.Join(cwd, ".env"),
			wantRoot:      filepath.Join(cwd, "candidates"),
			wantNowNonNil: true,
		},
		{
			name:          "explicit paths are preserved",
			home:          filepath.Join(string(filepath.Separator), "ignored"),
			envPath:       filepath.Join(string(filepath.Separator), "custom", ".env"),
			candidateRoot: filepath.Join(string(filepath.Separator), "custom", "candidates"),
			wantEnvPath:   filepath.Join(string(filepath.Separator), "custom", ".env"),
			wantRoot:      filepath.Join(string(filepath.Separator), "custom", "candidates"),
			wantNowNonNil: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{EnvPath: tt.envPath, CandidateRoot: tt.candidateRoot}
			cfg.ApplyDefaults(func(key string) string {
				if key == "PROXYCORE_HOST_HOME" {
					return tt.home
				}
				return ""
			})

			if cfg.EnvPath != tt.wantEnvPath {
				t.Fatalf("EnvPath = %q, want %q", cfg.EnvPath, tt.wantEnvPath)
			}
			if cfg.CandidateRoot != tt.wantRoot {
				t.Fatalf("CandidateRoot = %q, want %q", cfg.CandidateRoot, tt.wantRoot)
			}
			if (cfg.Now != nil) != tt.wantNowNonNil {
				t.Fatalf("Now non-nil = %t, want %t", cfg.Now != nil, tt.wantNowNonNil)
			}
		})
	}
}

func TestConfigApplyDefaultsPreservesInjectedClock(t *testing.T) {
	want := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	cfg := Config{Now: func() time.Time { return want }}
	cfg.ApplyDefaults(func(string) string { return "" })
	if got := cfg.Now(); !got.Equal(want) {
		t.Fatalf("Now() = %v, want %v", got, want)
	}
}
