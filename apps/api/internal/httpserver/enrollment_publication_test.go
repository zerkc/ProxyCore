package httpserver_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/zerkc/ProxyCore/apps/api/internal/cluster"
	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/httpserver"
	"github.com/zerkc/ProxyCore/apps/api/internal/identity"
	replicationsnapshot "github.com/zerkc/ProxyCore/apps/api/internal/snapshot"
	syncpublication "github.com/zerkc/ProxyCore/apps/api/internal/sync"
)

func TestSnapshotPublicationRejectsTLS12(t *testing.T) {
	handler := httpserver.NewEnrollmentSnapshotPublicationHandler(httpserver.EnrollmentSnapshotPublicationHandlerOptions{})
	request := httptest.NewRequest(http.MethodGet, httpserver.EnrollmentSnapshotPublicationPath, nil)
	request.Host = "primary.example:3443"
	request.TLS = &tls.ConnectionState{Version: tls.VersionTLS12, HandshakeComplete: true, ServerName: "primary.example"}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want %d", response.Code, http.StatusBadRequest)
	}
}

func TestSnapshotPublicationRootDoesNotRedirect(t *testing.T) {
	handler := httpserver.NewEnrollmentSnapshotPublicationHandler(httpserver.EnrollmentSnapshotPublicationHandlerOptions{})
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Host = "primary.example:3443"
	request.TLS = &tls.ConnectionState{Version: tls.VersionTLS13, HandshakeComplete: true, ServerName: "primary.example"}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound || response.Header().Get("Location") != "" {
		t.Fatalf("status=%d location=%q", response.Code, response.Header().Get("Location"))
	}
}

func TestSnapshotPublicationHappyPathReturnsExactBody(t *testing.T) {
	fixture := newPublicationFixture(t, false)
	handler := httpserver.NewEnrollmentSnapshotPublicationHandler(httpserver.EnrollmentSnapshotPublicationHandlerOptions{
		Store: fixture.store, Identity: fixture.identity, Certificate: publicationCertificate,
	})
	response := fixture.request(handler, fixture.credential.BearerCopy())
	if response.Code != http.StatusOK || !bytes.Equal(response.Body.Bytes(), fixture.body) {
		t.Fatalf("status=%d body=%x want=%x", response.Code, response.Body.Bytes(), fixture.body)
	}
	if response.Header().Get("Content-Type") != "application/octet-stream" ||
		response.Header().Get("Cache-Control") != "no-store" ||
		response.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("unsafe response headers: %v", response.Header())
	}
}

func TestSnapshotPublicationNoBodyReturnsNoContent(t *testing.T) {
	fixture := newPublicationFixture(t, false)
	fixture.store = publicationStoreFunc(func(_ context.Context, request configuration.SnapshotPublicationRequest) (configuration.SnapshotPublicationResult, error) {
		if _, err := request.Authenticate(fixture.credential.BearerCopy(), fixture.credentialRecord()); err != nil {
			return configuration.SnapshotPublicationResult{}, err
		}
		return configuration.SnapshotPublicationResult{}, configuration.ErrSnapshotPublicationStore
	})
	handler := httpserver.NewEnrollmentSnapshotPublicationHandler(httpserver.EnrollmentSnapshotPublicationHandlerOptions{
		Store: fixture.store, Identity: fixture.identity, Certificate: publicationCertificate,
	})
	response := fixture.request(handler, fixture.credential.BearerCopy())
	if response.Code != http.StatusNoContent || response.Body.Len() != 0 {
		t.Fatalf("status=%d body=%q", response.Code, response.Body.String())
	}
}

func TestSnapshotPublicationMapsRevokedAndInvalidCredentialsWithoutLeakage(t *testing.T) {
	fixture := newPublicationFixture(t, true)
	handler := httpserver.NewEnrollmentSnapshotPublicationHandler(httpserver.EnrollmentSnapshotPublicationHandlerOptions{
		Store: fixture.store, Identity: fixture.identity, Certificate: publicationCertificate,
	})
	revoked := fixture.request(handler, fixture.credential.BearerCopy())
	if revoked.Code != http.StatusGone {
		t.Fatalf("revoked status=%d want %d", revoked.Code, http.StatusGone)
	}
	invalidCredential := "pcnode1_invalid-secret"
	invalid := fixture.request(handler, invalidCredential)
	if invalid.Code != http.StatusForbidden {
		t.Fatalf("invalid status=%d want %d", invalid.Code, http.StatusForbidden)
	}
	for _, body := range []string{revoked.Body.String(), invalid.Body.String()} {
		for _, forbidden := range []string{fixture.credential.BearerCopy(), "kek", "bootstrap", hex.EncodeToString(fixture.key)} {
			if strings.Contains(strings.ToLower(body), strings.ToLower(forbidden)) {
				t.Fatalf("error response leaked %q: %s", forbidden, body)
			}
		}
	}
}

func TestSnapshotPublicationDeniesUnknownCredentialAndIneligibleRoles(t *testing.T) {
	fixture := newPublicationFixture(t, false)
	handler := httpserver.NewEnrollmentSnapshotPublicationHandler(httpserver.EnrollmentSnapshotPublicationHandlerOptions{
		Store: fixture.store, Identity: fixture.identity, Certificate: publicationCertificate,
	})
	unknown := fixture.credential.BearerCopy()[:len(fixture.credential.BearerCopy())-1] + "A"
	if response := fixture.request(handler, unknown); response.Code != http.StatusForbidden {
		t.Fatalf("unknown status=%d want %d", response.Code, http.StatusForbidden)
	}
	for _, role := range []domain.TopologyRole{
		domain.TopologyRoleNode, domain.TopologyRoleStandalone, domain.TopologyRoleStalePrimary,
	} {
		fixture.identity.current.Role = role
		if response := fixture.request(handler, fixture.credential.BearerCopy()); response.Code != http.StatusForbidden {
			t.Fatalf("role=%s status=%d want %d", role, response.Code, http.StatusForbidden)
		}
	}
	fixture.identity.current.Role = domain.TopologyRolePrimary
	fixture.identity.current.LatestKnownGeneration++
	if response := fixture.request(handler, fixture.credential.BearerCopy()); response.Code != http.StatusNotFound {
		t.Fatalf("stale generation status=%d want %d", response.Code, http.StatusNotFound)
	}
}

func TestSnapshotPublicationRejectsWrongAuthority(t *testing.T) {
	fixture := newPublicationFixture(t, false)
	handler := httpserver.NewEnrollmentSnapshotPublicationHandler(httpserver.EnrollmentSnapshotPublicationHandlerOptions{
		Store: fixture.store, Identity: fixture.identity, Certificate: publicationCertificate,
	})
	request := httptest.NewRequest(http.MethodGet, httpserver.EnrollmentSnapshotPublicationPath, nil)
	request.Host = "wrong.example:3443"
	request.Header.Set("Authorization", "Bearer "+fixture.credential.BearerCopy())
	request.TLS = &tls.ConnectionState{Version: tls.VersionTLS13, HandshakeComplete: true, ServerName: "wrong.example"}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || response.Header().Get("Location") != "" {
		t.Fatalf("status=%d location=%q", response.Code, response.Header().Get("Location"))
	}
}

type publicationStoreFunc func(context.Context, configuration.SnapshotPublicationRequest) (configuration.SnapshotPublicationResult, error)

func (f publicationStoreFunc) ReadSnapshotPublication(ctx context.Context, request configuration.SnapshotPublicationRequest) (configuration.SnapshotPublicationResult, error) {
	return f(ctx, request)
}

type publicationIdentity struct{ current identity.Identity }

func (i publicationIdentity) SnapshotPublicationIdentity() (identity.Identity, error) {
	if i.current.InstallationID == "" {
		return identity.Identity{}, identity.ErrIdentityNotLoaded
	}
	return i.current, nil
}

type publicationFixture struct {
	credential syncpublication.NodeCredential
	identity   *publicationIdentity
	store      configuration.SnapshotPublicationStore
	body       []byte
	key        []byte
	primaryID  string
	nodeID     string
	keyID      uuid.UUID
}

func newPublicationFixture(t *testing.T, revoked bool) *publicationFixture {
	t.Helper()
	credential, err := syncpublication.NewNodeCredential("11112222-3333-4444-8999-aabbccddeeff", bytes.Repeat([]byte{0x42}, syncpublication.NodeCredentialSecretBytes))
	if err != nil {
		t.Fatal(err)
	}
	primaryID := domain.NewInstallationID()
	nodeID := domain.NewNodeID()
	keyID := uuid.MustParse("aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee")
	key := bytes.Repeat([]byte{0x7a}, cluster.KEKLength)
	envelope := &replicationsnapshot.Envelope{
		Transient: replicationsnapshot.TransientFields{
			SnapshotVersion: domain.SnapshotVersionV1, ReplicationVersion: domain.ReplicationVersionV1,
			SourcePrimaryID: uuid.MustParse(string(primaryID)), LeadershipGeneration: 4,
			CapturedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		},
		NodeLocal: replicationsnapshot.NodeLocalFields{
			NodeID: domain.NodeID(nodeID), Role: domain.TopologyRolePrimary,
			LeadershipGeneration: 4, ClusterKeyRef: &keyID,
		},
		Replicated: replicationsnapshot.ReplicatedFields{Configuration: map[string]any{"settings": map[string]any{}}},
	}
	hash, err := envelope.ExpectedHash()
	if err != nil {
		t.Fatal(err)
	}
	envelope.ContentHash = hash
	body, err := replicationsnapshot.Marshal(*envelope)
	if err != nil {
		t.Fatal(err)
	}
	appliedAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	current := identity.Identity{
		InstallationID: primaryID, NodeID: nodeID, Role: domain.TopologyRolePrimary,
		LeadershipGeneration: 4, LatestKnownGeneration: 4, ClusterKeyID: &keyID,
	}
	fixture := &publicationFixture{credential: credential, identity: &publicationIdentity{current: current}, body: body, key: key, primaryID: string(primaryID), nodeID: string(nodeID), keyID: keyID}
	fixture.store = publicationStoreFunc(func(_ context.Context, request configuration.SnapshotPublicationRequest) (configuration.SnapshotPublicationResult, error) {
		principal, err := request.Authenticate(request.PresentedCredential, fixture.credentialRecordWithRevocation(revoked))
		if err != nil {
			return configuration.SnapshotPublicationResult{}, configuration.ErrSnapshotPublicationStore
		}
		return configuration.SnapshotPublicationResult{
			Principal: principal,
			Identity: configuration.SnapshotPublicationIdentityRecord{
				InstallationID: string(primaryID), NodeID: string(nodeID), Role: domain.TopologyRolePrimary,
				LeadershipGeneration: 4, LatestKnownGeneration: 4, ClusterKeyID: &keyID, ClusterKeyUsable: true,
			},
			Snapshot: configuration.SnapshotPublicationRecord{
				SnapshotID: "11112222-3333-4444-8999-aabbccddeeff", Bytes: body, ContentHash: hash,
				SnapshotVersion: 1, ReplicationVersion: 1, RevisionID: "22223333-4444-4555-8666-bbbbbbbbbbbb",
				RevisionNumber: 1, SourcePrimaryID: string(primaryID), LeadershipGeneration: 4,
				ApplyJobID: "33334444-5555-4666-8777-cccccccccccc", AppliedAt: appliedAt, ProofComplete: true,
			},
		}, nil
	})
	return fixture
}

func (f *publicationFixture) credentialRecord() configuration.SnapshotPublicationCredentialRecord {
	return f.credentialRecordWithRevocation(false)
}

func (f *publicationFixture) credentialRecordWithRevocation(revoked bool) configuration.SnapshotPublicationCredentialRecord {
	var revokedAt *time.Time
	if revoked {
		value := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
		revokedAt = &value
	}
	return configuration.SnapshotPublicationCredentialRecord{
		ID: f.credential.ID(), NodeID: f.nodeID, CredentialHash: f.credential.Hash(),
		HashVersion: syncpublication.NodeCredentialHashVersion, CredentialRevokedAt: revokedAt,
		PrimaryID: f.primaryID,
	}
}

func (f *publicationFixture) request(handler http.Handler, bearer string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, httpserver.EnrollmentSnapshotPublicationPath, nil)
	request.Host = "primary.example:3443"
	request.Header.Set("Authorization", "Bearer "+bearer)
	request.TLS = &tls.ConnectionState{Version: tls.VersionTLS13, HandshakeComplete: true, ServerName: "primary.example"}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func publicationCertificate(context.Context) (*x509.Certificate, error) {
	return &x509.Certificate{DNSNames: []string{"primary.example"}}, nil
}
