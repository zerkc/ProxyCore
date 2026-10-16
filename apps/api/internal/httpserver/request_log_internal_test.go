package httpserver

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	syncpublication "github.com/zerkc/ProxyCore/apps/api/internal/sync"
)

func TestNoteCauseAppendsRedactedCauseToRequestLine(t *testing.T) {
	var buf bytes.Buffer
	handler := RequestLogMiddleware(log.New(&buf, "", 0), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		noteCause(r.Context(), "node_list", fmt.Errorf("db at pcenr2_SECRETSECRETSECRETSECRET: %w", syncpublication.ErrExchangeUnavailable))
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "node list unavailable"})
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/topology/nodes", nil))
	out := buf.String()
	for _, want := range []string{"status=503", `err="node list unavailable"`, "cause=[node_list", "ErrExchangeUnavailable", "*fmt.wrapError"} {
		if !strings.Contains(out, want) {
			t.Fatalf("log missing %q: %s", want, out)
		}
	}
	if strings.Contains(out, "SECRET") || strings.Contains(out, "db at") {
		t.Fatalf("log leaks error text: %s", out)
	}
}

func TestNoteCauseIsNoOpOutsideMiddleware(t *testing.T) {
	noteCause(httptest.NewRequest(http.MethodGet, "/", nil).Context(), "x", errors.New("boom"))
}
