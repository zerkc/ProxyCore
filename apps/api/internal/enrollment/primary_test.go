package enrollment

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/identity"
	nodesync "github.com/zerkc/ProxyCore/apps/api/internal/sync"
)

func TestIssueGrantRejectsAnInvalidBindingGenerically(t *testing.T) {
	service := NewPrimaryService(nil, PrimaryServiceOptions{})
	_, err := service.IssueGrant(context.Background(), PrimaryGrantRequest{
		EnrollmentToken:               "not-a-token",
		AttemptID:                     "not-a-uuid",
		TargetInstallationID:          "not-a-uuid",
		TargetNodeID:                  "not-a-uuid",
		ExpectedPrimaryInstallationID: "not-a-uuid",
		ExpectedLeadershipGeneration:  1,
	})
	if !errors.Is(err, ErrEnrollmentDenied) {
		t.Fatalf("IssueGrant error = %v, want ErrEnrollmentDenied", err)
	}
}

func TestIssueGrantExactRetryReturnsPersistedEnvelopeAndDoesNotReissue(t *testing.T) {
	fixture := newPrimaryFixture(t)
	first, err := fixture.service.IssueGrant(context.Background(), fixture.request)
	if err != nil {
		t.Fatalf("first IssueGrant: %v", err)
	}
	second, err := fixture.service.IssueGrant(context.Background(), fixture.request)
	if err != nil {
		t.Fatalf("exact retry IssueGrant: %v", err)
	}
	if !bytes.Equal(first.SealedBootstrapPayload, second.SealedBootstrapPayload) {
		t.Fatal("exact retry returned different sealed bytes")
	}
	if first.PayloadHash != second.PayloadHash || !first.ExpiresAt.Equal(second.ExpiresAt) {
		t.Fatal("exact retry changed persisted envelope metadata")
	}
	if fixture.issuerCalls != 1 {
		t.Fatalf("credential issuer calls = %d, want 1", fixture.issuerCalls)
	}
	if fixture.tx.clusterLoads != 1 {
		t.Fatalf("cluster key loads = %d, want 1", fixture.tx.clusterLoads)
	}
	if fixture.tx.identity.Role != domain.TopologyRolePrimaryWithNodes {
		t.Fatalf("identity role = %q, want primary-with-nodes", fixture.tx.identity.Role)
	}
}

func TestIssueGrantBindsPreviewAndRecipientWithoutDisclosingReason(t *testing.T) {
	fixture := newPrimaryFixture(t)
	if _, err := fixture.service.IssueGrant(context.Background(), fixture.request); err != nil {
		t.Fatalf("first IssueGrant: %v", err)
	}
	otherRecipient, err := NewBootstrapRecipient()
	if err != nil {
		t.Fatalf("other recipient: %v", err)
	}
	defer otherRecipient.Destroy()
	otherPublicKey, err := otherRecipient.PublicKey()
	if err != nil {
		t.Fatalf("other recipient public key: %v", err)
	}
	cases := []struct {
		name string
		edit func(*PrimaryGrantRequest)
	}{
		{name: "preview digest", edit: func(request *PrimaryGrantRequest) { request.VerifiedPreviewDigest[0] ^= 1 }},
		{name: "attempt", edit: func(request *PrimaryGrantRequest) { request.AttemptID = "00112233-4455-4667-8899-aabbccddeeff" }},
		{name: "target installation", edit: func(request *PrimaryGrantRequest) {
			request.TargetInstallationID = "00112233-4455-4667-8899-aabbccddeeff"
		}},
		{name: "target node", edit: func(request *PrimaryGrantRequest) { request.TargetNodeID = "00112233-4455-4667-8899-aabbccddeeff" }},
		{name: "source generation", edit: func(request *PrimaryGrantRequest) { request.ExpectedLeadershipGeneration++ }},
		{name: "recipient public key", edit: func(request *PrimaryGrantRequest) { request.RecipientPublicKey = otherPublicKey }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := fixture.request
			request.VerifiedPreviewDigest = append([]byte(nil), fixture.request.VerifiedPreviewDigest...)
			tc.edit(&request)
			_, err := fixture.service.IssueGrant(context.Background(), request)
			if !errors.Is(err, ErrEnrollmentDenied) {
				t.Fatalf("IssueGrant error = %v, want generic denial", err)
			}
			if stringsContainAny(err.Error(), fixture.token, fixture.credentialBearer, fixture.kekText) {
				t.Fatalf("denial disclosed secret: %q", err)
			}
		})
	}
}

func TestIssueGrantRejectsStaleOrNonPrimaryIdentity(t *testing.T) {
	cases := []struct {
		name   string
		role   domain.TopologyRole
		latest uint64
	}{
		{name: "node", role: domain.TopologyRoleNode, latest: 7},
		{name: "stale primary", role: domain.TopologyRolePrimary, latest: 8},
		{name: "stale primary role", role: domain.TopologyRoleStalePrimary, latest: 7},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newPrimaryFixture(t)
			fixture.tx.identity.Role = tc.role
			fixture.tx.identity.LatestKnownGeneration = tc.latest
			if _, err := fixture.service.IssueGrant(context.Background(), fixture.request); !errors.Is(err, ErrEnrollmentDenied) {
				t.Fatalf("IssueGrant error = %v, want generic denial", err)
			}
			if fixture.issuerCalls != 0 || fixture.tx.clusterLoads != 0 {
				t.Fatalf("denied identity issued material: issuer=%d cluster=%d", fixture.issuerCalls, fixture.tx.clusterLoads)
			}
		})
	}
}

func TestIssueGrantUpdatesIdentityCacheOnlyAfterCommit(t *testing.T) {
	fixture := newPrimaryFixture(t)
	liveStore := &liveIdentityStore{current: identity.Identity{
		InstallationID:        domain.InstallationID(fixture.request.ExpectedPrimaryInstallationID),
		NodeID:                domain.NewNodeID(),
		Role:                  domain.TopologyRolePrimary,
		LeadershipGeneration:  domain.LeadershipGeneration(fixture.request.ExpectedLeadershipGeneration),
		LatestKnownGeneration: domain.LeadershipGeneration(fixture.request.ExpectedLeadershipGeneration),
	}}
	identityService := identity.NewService(liveStore)
	if _, err := identityService.Load(context.Background()); err != nil {
		t.Fatalf("identity Load: %v", err)
	}
	fixture.tx.transition = func() { liveStore.current.Role = domain.TopologyRolePrimaryWithNodes }
	fixture.service.identity = identityService
	if _, err := fixture.service.IssueGrant(context.Background(), fixture.request); err != nil {
		t.Fatalf("IssueGrant: %v", err)
	}
	if identityService.Current().Role != domain.TopologyRolePrimaryWithNodes || liveStore.current.Role != domain.TopologyRolePrimaryWithNodes {
		t.Fatalf("identity cache/durable role = %s/%s", identityService.Current().Role, liveStore.current.Role)
	}

	failed := newPrimaryFixture(t)
	failedStore := &liveIdentityStore{current: identity.Identity{
		InstallationID:        domain.InstallationID(failed.request.ExpectedPrimaryInstallationID),
		NodeID:                domain.NewNodeID(),
		Role:                  domain.TopologyRolePrimary,
		LeadershipGeneration:  domain.LeadershipGeneration(failed.request.ExpectedLeadershipGeneration),
		LatestKnownGeneration: domain.LeadershipGeneration(failed.request.ExpectedLeadershipGeneration),
	}}
	failedIdentity := identity.NewService(failedStore)
	if _, err := failedIdentity.Load(context.Background()); err != nil {
		t.Fatalf("failed identity Load: %v", err)
	}
	failed.service.identity = failedIdentity
	failed.service.credentialIssuer = testNodeCredentialIssuer(func(context.Context) (NodeCredentialMaterial, error) {
		return NodeCredentialMaterial{}, ErrEnrollmentDenied
	})
	if _, err := failed.service.IssueGrant(context.Background(), failed.request); !errors.Is(err, ErrEnrollmentDenied) {
		t.Fatalf("failed IssueGrant error = %v", err)
	}
	if failedIdentity.Current().Role != domain.TopologyRolePrimary || failedStore.current.Role != domain.TopologyRolePrimary {
		t.Fatalf("failed identity cache/durable role = %s/%s", failedIdentity.Current().Role, failedStore.current.Role)
	}
}

func TestIssueGrantEnvelopeContainsBindingAndSyncCredentialAuthenticates(t *testing.T) {
	fixture := newPrimaryFixture(t)
	response, err := fixture.service.IssueGrant(context.Background(), fixture.request)
	if err != nil {
		t.Fatalf("IssueGrant: %v", err)
	}
	recipient := fixture.recipient
	opened, err := recipient.OpenBootstrap(testBindingFromRequest(fixture.request), response.SealedBootstrapPayload)
	if err != nil {
		t.Fatalf("OpenBootstrap: %v", err)
	}
	defer opened.Destroy()
	if opened.Metadata.AttemptID != fixture.request.AttemptID ||
		opened.Metadata.TargetInstallationID != fixture.request.TargetInstallationID ||
		opened.Metadata.TargetNodeID != fixture.request.TargetNodeID ||
		opened.Metadata.SourcePrimaryInstallationID != fixture.request.ExpectedPrimaryInstallationID ||
		opened.Metadata.LeadershipGeneration != fixture.request.ExpectedLeadershipGeneration {
		t.Fatalf("opened metadata did not preserve request binding: %+v", opened.Metadata)
	}
	credential := opened.Secrets.NodeCredential.CopyBytes()
	defer zeroBytes(credential)
	bearer := fixture.credentialBearer
	principal, err := nodesync.AuthenticateNodeCredential(bearer, &nodesync.NodeCredentialRecord{
		ID:          fixture.tx.credential.ID,
		NodeID:      fixture.request.TargetNodeID,
		Hash:        fixture.tx.credential.CredentialHash,
		HashVersion: fixture.tx.credential.HashVersion,
	})
	if err != nil {
		t.Fatalf("AuthenticateNodeCredential: %v", err)
	}
	if principal.NodeID != fixture.request.TargetNodeID || !bytes.Equal(credential, fixture.credentialSecret) {
		t.Fatal("opened credential did not match the persisted hash authority")
	}
	if !bytes.Equal(opened.Secrets.ClusterKEK.CopyBytes(), fixture.clusterKey) {
		t.Fatal("opened cluster KEK mismatch")
	}
}
