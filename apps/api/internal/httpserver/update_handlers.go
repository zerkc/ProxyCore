package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/zerkc/ProxyCore/apps/api/internal/auth"
	"github.com/zerkc/ProxyCore/apps/api/internal/update"
	"github.com/zerkc/ProxyCore/apps/api/internal/version"
)

// UpdaterClient sends an apply request to the internal updater service.
type UpdaterClient interface {
	Apply(ctx context.Context, targetVersion string) (int, error)
}

// UpdaterStatusClient optionally reads the current update status from the
// internal updater service.
type UpdaterStatusClient interface {
	Status(ctx context.Context) (UpdaterStatus, error)
}

// UpdaterStatus is the subset of updater state needed by the public updates
// endpoint.
type UpdaterStatus struct {
	Status        string `json:"status"`
	TargetVersion string `json:"targetVersion"`
}

// UpdaterHTTPClient forwards requests to the real updater service.
type UpdaterHTTPClient struct {
	URL       string
	StatusURL string
	Client    *http.Client
}

func (c *UpdaterHTTPClient) Apply(ctx context.Context, targetVersion string) (int, error) {
	payload, err := json.Marshal(map[string]string{"targetVersion": targetVersion})
	if err != nil {
		return 0, fmt.Errorf("marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(payload))
	if err != nil {
		return 0, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("updater request: %w", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}

func (c *UpdaterHTTPClient) Status(ctx context.Context) (UpdaterStatus, error) {
	endpoint, err := c.statusURL()
	if err != nil {
		return UpdaterStatus{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return UpdaterStatus{}, fmt.Errorf("create status request: %w", err)
	}
	resp, err := c.Client.Do(req)
	if err != nil {
		return UpdaterStatus{}, fmt.Errorf("updater status request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return UpdaterStatus{}, fmt.Errorf("updater status returned %s", resp.Status)
	}

	var status UpdaterStatus
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 1<<20))
	if err := decoder.Decode(&status); err != nil {
		return UpdaterStatus{}, fmt.Errorf("decode updater status: %w", err)
	}
	return status, nil
}

func (c *UpdaterHTTPClient) statusURL() (string, error) {
	if statusURL := strings.TrimSpace(c.StatusURL); statusURL != "" {
		return statusURL, nil
	}

	applyURL := strings.TrimSpace(c.URL)
	parsed, err := url.Parse(applyURL)
	if err != nil {
		return "", fmt.Errorf("derive updater status URL: %w", err)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("derive updater status URL: invalid apply URL %q", applyURL)
	}
	path := strings.TrimRight(parsed.Path, "/")
	const applyPath = "/internal/apply"
	if !strings.HasSuffix(path, applyPath) {
		return "", fmt.Errorf("derive updater status URL: apply URL %q has no %s suffix", applyURL, applyPath)
	}
	parsed.Path = strings.TrimSuffix(path, "/apply") + "/status"
	parsed.RawPath = ""
	return parsed.String(), nil
}

func (s *Server) handleUpdates(w http.ResponseWriter, r *http.Request) {
	var result update.Result
	if s.updates == nil {
		result = update.Result{
			Status:         update.StatusDisabled,
			CurrentVersion: version.Version,
		}
	} else {
		result = s.updates.Check(r.Context())
	}

	if client, ok := s.updaterClient.(UpdaterStatusClient); ok {
		status, err := client.Status(r.Context())
		if err == nil && status.Status == "running" {
			targetVersion := normalizeUpdaterTarget(status.TargetVersion)
			if targetVersion != "" {
				result.UpdateInProgress = true
				result.TargetVersion = targetVersion
			}
		}
	}
	writeJSON(w, http.StatusOK, result)
}

func normalizeUpdaterTarget(raw string) string {
	return strings.TrimPrefix(strings.TrimSpace(raw), "v")
}

func (s *Server) handleUpdateApply(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireUser(w, r, auth.RoleOwner); !ok {
		return
	}

	var body struct {
		TargetVersion string `json:"targetVersion"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid JSON body"})
		return
	}
	if body.TargetVersion == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "targetVersion is required"})
		return
	}

	if s.updaterClient == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"ok":    false,
			"error": "updater is not configured",
		})
		return
	}

	statusCode, err := s.updaterClient.Apply(r.Context(), body.TargetVersion)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			writeJSON(w, http.StatusGatewayTimeout, map[string]any{"ok": false, "error": "updater request timed out"})
			return
		}
		s.log.Printf("updater apply: %v", err)
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": "failed to contact updater"})
		return
	}

	// The updater returns 202 Accepted for an accepted update and 409 Conflict
	// if an update is already in flight.
	if statusCode == http.StatusAccepted {
		writeJSON(w, http.StatusAccepted, map[string]any{
			"ok":            true,
			"targetVersion": body.TargetVersion,
			"message":       "update accepted; poll GET /api/updates for status",
		})
		return
	}
	if statusCode == http.StatusConflict {
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "error": "update already in progress"})
		return
	}
	writeJSON(w, http.StatusBadGateway, map[string]any{
		"ok":    false,
		"error": fmt.Sprintf("updater returned unexpected status %d", statusCode),
	})
}
