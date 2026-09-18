package enrollment

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
	nodesync "github.com/zerkc/ProxyCore/apps/api/internal/sync"
)

func TestPostgresPrimaryGrantPersistsOneHashOnlySealedEnvelope(t *testing.T) {
	fixture := newPostgresPrimaryFixture(t, 20*time.Minute)
	response, err := fixture.service.IssueGrant(context.Background(), fixture.request)
	if err != nil {
		t.Fatalf("IssueGrant: %v", err)
	}
	var attemptRows int
	if err := fixture.pool.QueryRow(context.Background(), `select count(*) from enrollment_attempts`).Scan(&attemptRows); err != nil {
		t.Fatalf("count primary attempt rows: %v", err)
	}
	if attemptRows != 0 {
		t.Fatalf("primary grant created %d node-local attempt rows", attemptRows)
	}
	grant := readPostgresGrant(t, fixture.pool, fixture.request.AttemptID)
	if grant.installationID != fixture.request.TargetInstallationID || grant.nodeID != fixture.request.TargetNodeID ||
		grant.primaryID != fixture.request.ExpectedPrimaryInstallationID || grant.generation != int64(fixture.request.ExpectedLeadershipGeneration) {
		t.Fatalf("grant binding = %+v", grant)
	}
	if grant.recipient != string(mustWire(t, fixture.request.RecipientPublicKey)) {
		t.Fatal("grant did not persist the exact canonical recipient key")
	}
	if grant.previewDigest != hexDigest(fixture.request.VerifiedPreviewDigest) {
		t.Fatalf("preview digest = %q, want %q", grant.previewDigest, hexDigest(fixture.request.VerifiedPreviewDigest))
	}
	if grant.payload != string(response.SealedBootstrapPayload) {
		t.Fatal("persisted envelope differs from response bytes")
	}
	sum := sha256.Sum256(response.SealedBootstrapPayload)
	if grant.payloadHash != fmt.Sprintf("%x", sum[:]) || response.PayloadHash != grant.payloadHash {
		t.Fatalf("payload hash = %q, want %x", grant.payloadHash, sum)
	}
	var role string
	if err := fixture.pool.QueryRow(context.Background(), `select role::text from installation_identity where id = 'default'`).Scan(&role); err != nil {
		t.Fatalf("read primary role: %v", err)
	}
	if role != "primary-with-nodes" {
		t.Fatalf("primary role = %q, want primary-with-nodes", role)
	}
	var consumedBy string
	if err := fixture.pool.QueryRow(context.Background(), `
		select consumed_by_attempt_id::text from enrollment_tokens where token_selector = $1
	`, tokenSelector(t, fixture.request.EnrollmentToken)).Scan(&consumedBy); err != nil {
		t.Fatalf("read consumed token: %v", err)
	}
	if consumedBy != fixture.request.AttemptID {
		t.Fatalf("consumed attempt = %q, want %q", consumedBy, fixture.request.AttemptID)
	}

	opened, err := fixture.recipient.OpenBootstrap(testBindingFromRequest(fixture.request), response.SealedBootstrapPayload)
	if err != nil {
		t.Fatalf("open persisted grant: %v", err)
	}
	defer opened.Destroy()
	openedCredential := opened.Secrets.NodeCredential.CopyBytes()
	openedKEK := opened.Secrets.ClusterKEK.CopyBytes()
	defer zeroBytes(openedCredential)
	defer zeroBytes(openedKEK)
	credentialBearer := findBearerForPersistedCredential(t, fixture.pool, opened.Metadata.CredentialID, append([]byte(nil), openedCredential...))
	var credentialHash, hashVersion, credentialNodeID string
	if err := fixture.pool.QueryRow(context.Background(), `
		select credential_hash, hash_version, node_id::text from node_credentials where id = $1
	`, opened.Metadata.CredentialID).Scan(&credentialHash, &hashVersion, &credentialNodeID); err != nil {
		t.Fatalf("read credential hash: %v", err)
	}
	if credentialNodeID != fixture.request.TargetNodeID || hashVersion != nodesync.NodeCredentialHashVersion {
		t.Fatalf("credential binding/version = %q/%q", credentialNodeID, hashVersion)
	}
	if _, err := nodesync.AuthenticateNodeCredential(credentialBearer, &nodesync.NodeCredentialRecord{
		ID: opened.Metadata.CredentialID, NodeID: credentialNodeID, Hash: credentialHash, HashVersion: hashVersion,
	}); err != nil {
		t.Fatalf("persisted credential did not authenticate: %v", err)
	}
	assertPostgresGrantSecretsAbsent(t, fixture.pool, fixture.request.EnrollmentToken, credentialBearer,
		string(openedCredential), string(openedKEK))
}

func TestPostgresPrimaryGrantExactRetrySurvivesNewService(t *testing.T) {
	fixture := newPostgresPrimaryFixture(t, 20*time.Minute)
	first, err := fixture.service.IssueGrant(context.Background(), fixture.request)
	if err != nil {
		t.Fatalf("first IssueGrant: %v", err)
	}
	callsBefore := fixture.issuerCalls
	restarted := NewPrimaryService(
		configuration.NewPhase2StoreWithMasterKey(fixture.pool, fixture.masterKey),
		PrimaryServiceOptions{
			Now: func() time.Time { return fixture.now },
			CredentialIssuer: testNodeCredentialIssuer(func(context.Context) (NodeCredentialMaterial, error) {
				t.Fatal("exact retry regenerated a credential")
				return NodeCredentialMaterial{}, ErrEnrollmentDenied
			}),
		},
	)
	second, err := restarted.IssueGrant(context.Background(), fixture.request)
	if err != nil {
		t.Fatalf("restart exact retry: %v", err)
	}
	if !bytes.Equal(first.SealedBootstrapPayload, second.SealedBootstrapPayload) || first.PayloadHash != second.PayloadHash {
		t.Fatal("restart exact retry changed the persisted envelope")
	}
	if fixture.issuerCalls != callsBefore {
		t.Fatalf("issuer calls changed during exact retry: %d -> %d", callsBefore, fixture.issuerCalls)
	}
}

func TestPostgresPrimaryGrantRejectsExpiredRevokedAndExpiredGrant(t *testing.T) {
	t.Run("expired token", func(t *testing.T) {
		fixture := newPostgresPrimaryFixture(t, 20*time.Minute)
		late := NewPrimaryService(fixture.store, PrimaryServiceOptions{
			Now:              func() time.Time { return fixture.now.Add(11 * time.Minute) },
			CredentialIssuer: fixture.service.credentialIssuer,
		})
		if _, err := late.IssueGrant(context.Background(), fixture.request); !errors.Is(err, ErrEnrollmentDenied) {
			t.Fatalf("expired token error = %v", err)
		}
		assertCount(t, fixture.pool, "enrollment_grants", 0)
	})
	t.Run("revoked token", func(t *testing.T) {
		fixture := newPostgresPrimaryFixture(t, 20*time.Minute)
		if err := fixture.tokenStore.RevokeToken(context.Background(), fixture.owner, tokenID(t, fixture.request.EnrollmentToken, fixture.pool)); err != nil {
			t.Fatalf("revoke token: %v", err)
		}
		if _, err := fixture.service.IssueGrant(context.Background(), fixture.request); !errors.Is(err, ErrEnrollmentDenied) {
			t.Fatalf("revoked token error = %v", err)
		}
		assertCount(t, fixture.pool, "enrollment_grants", 0)
	})
	t.Run("expired grant", func(t *testing.T) {
		fixture := newPostgresPrimaryFixture(t, time.Minute)
		if _, err := fixture.service.IssueGrant(context.Background(), fixture.request); err != nil {
			t.Fatalf("first IssueGrant: %v", err)
		}
		late := NewPrimaryService(fixture.store, PrimaryServiceOptions{
			Now: func() time.Time { return fixture.now.Add(2 * time.Minute) },
			CredentialIssuer: testNodeCredentialIssuer(func(context.Context) (NodeCredentialMaterial, error) {
				t.Fatal("expired retry regenerated a credential")
				return NodeCredentialMaterial{}, ErrEnrollmentDenied
			}),
		})
		if _, err := late.IssueGrant(context.Background(), fixture.request); !errors.Is(err, ErrEnrollmentDenied) {
			t.Fatalf("expired grant error = %v", err)
		}
	})
}
