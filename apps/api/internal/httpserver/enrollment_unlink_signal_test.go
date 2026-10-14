package httpserver_test

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
	"github.com/zerkc/ProxyCore/apps/api/internal/httpserver"
	syncpublication "github.com/zerkc/ProxyCore/apps/api/internal/sync"
)

const nodeUnlinkedBody = `{"error":"node unlinked","code":"NODE_UNLINKED"}`

func unlinkedRecord(record configuration.SnapshotPublicationCredentialRecord, unlinked bool) configuration.SnapshotPublicationCredentialRecord {
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	record.CredentialRevokedAt, record.NodeRevokedAt = &at, &at
	if unlinked {
		record.NodeUnlinkedAt = &at
	}
	return record
}

func wrongSecretBearer(t *testing.T, id string) string {
	t.Helper()
	other, err := syncpublication.NewNodeCredential(id, bytes.Repeat([]byte{0x24}, syncpublication.NodeCredentialSecretBytes))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Destroy()
	return other.BearerCopy()
}

func TestSnapshotPublicationSignalsNodeUnlinkedOnlyToSecretHolder(t *testing.T) {
	for _, tc := range []struct {
		name     string
		unlinked bool
		wrong    bool
		wantCode string
	}{
		{name: "unlinked holder", unlinked: true, wantCode: "NODE_UNLINKED"},
		{name: "plain revocation", unlinked: false},
		{name: "unlinked but wrong secret", unlinked: true, wrong: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newPublicationFixture(t, false)
			fixture.store = publicationStoreFunc(func(_ context.Context, request configuration.SnapshotPublicationRequest) (configuration.SnapshotPublicationResult, error) {
				_, err := request.Authenticate(request.PresentedCredential, unlinkedRecord(fixture.credentialRecord(), tc.unlinked))
				if err == nil {
					t.Fatal("revoked credential must not authenticate")
				}
				return configuration.SnapshotPublicationResult{}, configuration.ErrSnapshotPublicationStore
			})
			handler := httpserver.NewEnrollmentSnapshotPublicationHandler(httpserver.EnrollmentSnapshotPublicationHandlerOptions{
				Store: fixture.store, Identity: fixture.identity, Certificate: publicationCertificate,
			})
			bearer := fixture.credential.BearerCopy()
			if tc.wrong {
				bearer = wrongSecretBearer(t, fixture.credential.ID())
			}
			response := fixture.request(handler, bearer)
			if response.Code != http.StatusGone {
				t.Fatalf("status=%d", response.Code)
			}
			if got := strings.Contains(response.Body.String(), "NODE_UNLINKED"); got != (tc.wantCode != "") {
				t.Fatalf("NODE_UNLINKED present=%v body=%s", got, response.Body.String())
			}
			if tc.wantCode != "" && response.Body.String() != nodeUnlinkedBody {
				t.Fatalf("body=%s", response.Body.String())
			}
		})
	}
}

func TestSnapshotPublicationStoreErrorIsNeverNodeUnlinked(t *testing.T) {
	fixture := newPublicationFixture(t, false)
	fixture.store = publicationStoreFunc(func(context.Context, configuration.SnapshotPublicationRequest) (configuration.SnapshotPublicationResult, error) {
		return configuration.SnapshotPublicationResult{}, configuration.ErrSnapshotPublicationStore
	})
	handler := httpserver.NewEnrollmentSnapshotPublicationHandler(httpserver.EnrollmentSnapshotPublicationHandlerOptions{
		Store: fixture.store, Identity: fixture.identity, Certificate: publicationCertificate,
	})
	response := fixture.request(handler, fixture.credential.BearerCopy())
	if response.Code == http.StatusGone || strings.Contains(response.Body.String(), "NODE_UNLINKED") {
		t.Fatalf("store failure answered status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestEnrollmentAcknowledgmentSignalsNodeUnlinkedOnlyToSecretHolder(t *testing.T) {
	for _, tc := range []struct {
		name     string
		unlinked bool
		wrong    bool
		want     bool
	}{
		{name: "unlinked holder", unlinked: true, want: true},
		{name: "plain revocation"},
		{name: "unlinked but wrong secret", unlinked: true, wrong: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newAcknowledgementFixture(t)
			store := acknowledgementAuthorizerFunc(func(_ context.Context, request configuration.SnapshotAcknowledgementRequest) error {
				_, _ = request.Authenticate(request.PresentedCredential, unlinkedRecord(fixture.credentialRecord(), tc.unlinked))
				return configuration.ErrSnapshotAcknowledgementDenied
			})
			handler := httpserver.NewEnrollmentSnapshotAcknowledgementHandler(httpserver.EnrollmentSnapshotAcknowledgementHandlerOptions{
				Authorizer: store, Identity: fixture.identity, Certificate: acknowledgementCertificate,
			})
			bearer := fixture.credential.BearerCopy()
			if tc.wrong {
				bearer = wrongSecretBearer(t, fixture.credential.ID())
			}
			response := fixture.request(handler, "https://primary.example:3443/", bearer)
			if response.Code != http.StatusGone {
				t.Fatalf("status=%d", response.Code)
			}
			if tc.want != (response.Body.String() == nodeUnlinkedBody) || (!tc.want && strings.Contains(response.Body.String(), "NODE_UNLINKED")) {
				t.Fatalf("body=%s", response.Body.String())
			}
		})
	}
}

func TestEnrollmentAcknowledgmentStoreErrorIsNeverNodeUnlinked(t *testing.T) {
	fixture := newAcknowledgementFixture(t)
	store := acknowledgementAuthorizerFunc(func(context.Context, configuration.SnapshotAcknowledgementRequest) error {
		return configuration.ErrSnapshotAcknowledgementUnavailable
	})
	handler := httpserver.NewEnrollmentSnapshotAcknowledgementHandler(httpserver.EnrollmentSnapshotAcknowledgementHandlerOptions{
		Authorizer: store, Identity: fixture.identity, Certificate: acknowledgementCertificate,
	})
	response := fixture.request(handler, "https://primary.example:3443/", fixture.credential.BearerCopy())
	if response.Code != http.StatusServiceUnavailable || strings.Contains(response.Body.String(), "NODE_UNLINKED") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}
