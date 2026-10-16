package httpserver_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/httpserver"
)

func TestSQLiteEnrollmentTokenDefaultsUsePersistedIngress(t *testing.T) {
	env := newSQLiteHTTPTestServer(t, httpserver.WithDefaultIngress(domain.Ingress{}))
	if _, err := env.Config.UpdateSettings(context.Background(), configuration.SettingsPatch{Ingress: &domain.Ingress{IPv4: "198.51.100.7"}}); err != nil {
		t.Fatalf("persist ingress: %v", err)
	}
	request := httptest.NewRequest(http.MethodGet, httpserver.EnrollmentTokensPath+"/defaults", nil)
	request.AddCookie(sqliteOwnerCookie(t, env.Handler))
	response := httptest.NewRecorder()
	env.Handler.ServeHTTP(response, request)
	var body map[string]any
	_ = json.Unmarshal(response.Body.Bytes(), &body)
	if response.Code != http.StatusOK || body["primaryUrl"] != "https://198.51.100.7:3443/" || body["gatewayIPv4"] != "198.51.100.7" {
		t.Fatalf("status=%d body=%v", response.Code, body)
	}
}
