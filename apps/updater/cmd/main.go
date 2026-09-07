// Command proxycore-updater runs the ProxyCore self-update orchestrator.
//
// It listens on PROXYCORE_UPDATER_ADDR (default ":8080") and exposes:
//
//	GET  /healthz             liveness probe
//	GET  /internal/status     last Apply result (JSON)
//	POST /internal/apply      kicks off an update, returns 202 + Status
//
// The updater intentionally does NOT restart itself; that is owned by an
// external bootstrap (see scripts/updater-bootstrap.sh) because a process
// cannot reliably replace its own running image. After a successful apply it
// writes a bootstrap request to the shared volume so the sidecar can pull the
// new image and recreate this container.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/zerkc/ProxyCore/apps/updater/internal/updater"
)

func main() {
	logger := log.New(os.Stdout, "updater ", log.LstdFlags|log.Lmicroseconds)

	cfg, err := loadConfig()
	if err != nil {
		logger.Fatalf("config: %v", err)
	}

	u := updater.New(updater.Options{
		UpdateMode:           cfg.UpdateMode,
		ComposeFile:          cfg.ComposeFile,
		ProjectName:          cfg.ProjectName,
		Services:             cfg.Services,
		MigrateService:       cfg.MigrateService,
		HealthURL:            cfg.HealthURL,
		HealthTimeout:        cfg.HealthTimeout,
		BootstrapRequestFile: cfg.BootstrapRequestFile,
		Logger:               logger,
	})

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("GET /internal/status", func(w http.ResponseWriter, _ *http.Request) {
		status := u.Last()
		if status == nil {
			writeJSON(w, http.StatusOK, map[string]any{"status": "idle"})
			return
		}
		writeJSON(w, http.StatusOK, status)
	})
	mux.HandleFunc("POST /internal/apply", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			TargetVersion string `json:"targetVersion"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid JSON body"})
			return
		}
		if strings.TrimSpace(body.TargetVersion) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "targetVersion is required"})
			return
		}
		if u.InFlight() {
			writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "error": updater.ErrAlreadyRunning.Error()})
			return
		}

		// Run in the background; the api will die during the restart phase and
		// we want the orchestrator to keep going. Surface an immediate 202 so
		// the caller (the api) doesn't hold an HTTP connection that will be
		// reset when its own container restarts.
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			status, err := u.Apply(ctx, body.TargetVersion)
			if err != nil {
				logger.Printf("apply: %v", err)
			}
			if status != nil {
				logger.Printf("apply finished: status=%s steps=%d", status.Status, len(status.Steps))
			}
		}()

		writeJSON(w, http.StatusAccepted, map[string]any{
			"ok":            true,
			"targetVersion": body.TargetVersion,
			"message":       "update accepted; poll /internal/status for progress",
		})
	})

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-stop
		logger.Printf("shutdown signal received")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	logger.Printf("listening on %s (project=%s mode=%s services=%v bootstrap=%q)",
		cfg.Addr, cfg.ProjectName, cfg.UpdateMode, cfg.Services, cfg.BootstrapRequestFile)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Fatalf("listen: %v", err)
	}
	logger.Printf("stopped")
}

type config struct {
	Addr                 string
	UpdateMode           string
	ComposeFile          string
	ProjectName          string
	Services             []string
	MigrateService       string
	HealthURL            string
	HealthTimeout        time.Duration
	BootstrapRequestFile string
}

func loadConfig() (*config, error) {
	cfg := &config{
		Addr:           envOr("PROXYCORE_UPDATER_ADDR", ":8080"),
		UpdateMode:     envOr("PROXYCORE_UPDATE_MODE", updater.UpdateModePull),
		ComposeFile:    envOr("PROXYCORE_COMPOSE_FILE", "/app/compose.yaml"),
		ProjectName:    envOr("PROXYCORE_COMPOSE_PROJECT", "proxycore"),
		MigrateService: envOr("PROXYCORE_MIGRATE_SERVICE", "migrate"),
		HealthURL:      envOr("PROXYCORE_HEALTH_URL", "http://api:3000/api/health"),
	}
	if s := strings.TrimSpace(os.Getenv("PROXYCORE_UPDATER_SERVICES")); s != "" {
		cfg.Services = splitNonEmpty(s, ", ")
	} else {
		cfg.Services = []string{"api", "worker"}
	}
	cfg.BootstrapRequestFile = os.Getenv("PROXYCORE_BOOTSTRAP_REQUEST_FILE")
	if d, err := time.ParseDuration(envOr("PROXYCORE_HEALTH_TIMEOUT", "90s")); err == nil {
		cfg.HealthTimeout = d
	} else {
		return nil, fmt.Errorf("PROXYCORE_HEALTH_TIMEOUT: %w", err)
	}
	if cfg.HealthTimeout < 5*time.Second {
		return nil, errors.New("PROXYCORE_HEALTH_TIMEOUT must be >= 5s")
	}
	return cfg, nil
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func splitNonEmpty(s string, seps string) []string {
	parts := strings.FieldsFunc(s, func(r rune) bool {
		return strings.ContainsRune(seps, r)
	})
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
