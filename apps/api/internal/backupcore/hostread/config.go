package hostread

import (
	"os"
	"path/filepath"
	"time"
)

// Config controls the host paths and clock used by a host snapshot reader.
type Config struct {
	// EnvPath is the deployed .env path. When empty, it is resolved from
	// PROXYCORE_HOST_HOME and then the current working directory.
	EnvPath string
	// CandidateRoot is the root containing per-revision candidate directories.
	// When empty, it is resolved from PROXYCORE_HOST_HOME and then the current
	// working directory.
	CandidateRoot string
	// Now is injectable for callers that need a stable clock.
	Now func() time.Time
}

// ApplyDefaults fills unset configuration fields without changing explicit
// paths or an injected clock.
func (c *Config) ApplyDefaults(env func(string) string) {
	if c == nil {
		return
	}
	if env == nil {
		env = os.Getenv
	}

	home := env("PROXYCORE_HOST_HOME")
	if home == "" {
		home, _ = os.Getwd()
		if home == "" {
			home = "."
		}
	}
	if c.EnvPath == "" {
		c.EnvPath = filepath.Join(home, ".env")
	}
	if c.CandidateRoot == "" {
		c.CandidateRoot = filepath.Join(home, "candidates")
	}
	if c.Now == nil {
		c.Now = time.Now
	}
}
