package httpserver_test

import (
	"bytes"
	"context"
	"net/http"
	"testing"

	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
)

// This contract test is intentionally database-free: PNE-6.7 owns the
// PostgreSQL implementation. It keeps the handler's production-shaped store
// seam exercised without pretending that the current phase2 store is token-aware.
func TestPostgresEnrollmentSnapshotByTokenStoreSeamPreservesCanonicalBody(t *testing.T) {
	fixture := newSnapshotByTokenFixture(t, []byte("postgres-shaped-canonical-body"))
	var gotSelector string
	var gotIdentity configuration.SnapshotPublicationIdentityRecord
	fixture.store.read = func(_ context.Context, selector string, current configuration.SnapshotPublicationIdentityRecord) ([]byte, error) {
		gotSelector, gotIdentity = selector, current
		return append([]byte(nil), fixture.body...), nil
	}
	handler := newSnapshotByTokenHandler(fixture)
	response := fixture.request(handler, http.MethodPost, bytes.NewBufferString(`{"token":"`+fixture.token+`"}`))
	if response.Code != http.StatusOK || !bytes.Equal(response.Body.Bytes(), fixture.body) {
		t.Fatalf("status=%d body=%q want=%q", response.Code, response.Body.Bytes(), fixture.body)
	}
	if gotSelector != fixture.selector || gotIdentity.InstallationID != string(fixture.identity.current.InstallationID) ||
		gotIdentity.NodeID != string(fixture.identity.current.NodeID) || !gotIdentity.ClusterKeyUsable {
		t.Fatalf("store request selector=%q identity=%+v", gotSelector, gotIdentity)
	}
}
