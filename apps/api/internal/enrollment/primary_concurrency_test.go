package enrollment

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/identity"
)

func TestConcurrentIdenticalAndCompetingBindingsUseOneMemoryGrant(t *testing.T) {
	fixture := newPrimaryFixture(t)
	store, ok := fixture.service.store.(*memoryPrimaryGrantStore)
	if !ok {
		t.Fatal("fixture store is not the memory transaction store")
	}
	store.mu = &sync.Mutex{}
	other := fixture.request
	other.VerifiedPreviewDigest = append([]byte(nil), fixture.request.VerifiedPreviewDigest...)
	other.VerifiedPreviewDigest[0] ^= 1

	results := make(chan primaryGrantResult, 2)
	var wait sync.WaitGroup
	for i := 0; i < 2; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			response, err := fixture.service.IssueGrant(context.Background(), fixture.request)
			results <- primaryGrantResult{response: response, err: err}
		}()
	}
	wait.Wait()
	close(results)

	var successes int
	var first []byte
	for result := range results {
		if result.err != nil {
			t.Fatalf("identical binding error = %v", result.err)
		}
		successes++
		if first == nil {
			first = result.response.SealedBootstrapPayload
		} else if !bytes.Equal(first, result.response.SealedBootstrapPayload) {
			t.Fatal("identical retry returned different memory envelope")
		}
	}
	if successes != 2 {
		t.Fatalf("successful identical requests = %d, want 2", successes)
	}
	if _, err := fixture.service.IssueGrant(context.Background(), other); !errors.Is(err, ErrEnrollmentDenied) {
		t.Fatalf("competing binding error = %v, want generic denial", err)
	}
	if fixture.issuerCalls != 1 || fixture.tx.clusterLoads != 1 {
		t.Fatalf("issuer/cluster calls = %d/%d, want 1/1", fixture.issuerCalls, fixture.tx.clusterLoads)
	}
}

func TestIssueGrantRejectsMalformedIssuerMaterial(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*NodeCredentialMaterial)
	}{
		{name: "hash", mutate: func(material *NodeCredentialMaterial) { material.Hash = strings.Repeat("0", 64) }},
		{name: "version", mutate: func(material *NodeCredentialMaterial) { material.HashVersion = "wrong-version" }},
		{name: "id", mutate: func(material *NodeCredentialMaterial) { material.ID = "not-a-uuid" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newPrimaryFixture(t)
			fixture.service.credentialIssuer = testNodeCredentialIssuer(func(context.Context) (NodeCredentialMaterial, error) {
				material := NodeCredentialMaterial{
					ID:          fixture.tx.credential.ID,
					Secret:      append([]byte(nil), fixture.credentialSecret...),
					Hash:        fixture.tx.credential.CredentialHash,
					HashVersion: fixture.tx.credential.HashVersion,
				}
				tc.mutate(&material)
				return material, nil
			})
			if _, err := fixture.service.IssueGrant(context.Background(), fixture.request); !errors.Is(err, ErrEnrollmentDenied) {
				t.Fatalf("malformed issuer error = %v, want generic denial", err)
			}
			if fixture.tx.grant.AttemptID != "" || fixture.tx.node.NodeID != "" {
				t.Fatal("malformed issuer left persisted grant state")
			}
		})
	}
}

func TestIssueGrantFailureBoundariesDenyWithoutPartialMemoryRows(t *testing.T) {
	t.Run("cancelled context", func(t *testing.T) {
		fixture := newPrimaryFixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := fixture.service.IssueGrant(ctx, fixture.request); !errors.Is(err, ErrEnrollmentDenied) {
			t.Fatalf("cancelled context error = %v", err)
		}
	})
	t.Run("cluster store failure", func(t *testing.T) {
		fixture := newPrimaryFixture(t)
		fixture.tx.clusterErr = configuration.ErrEnrollmentGrantStore
		if _, err := fixture.service.IssueGrant(context.Background(), fixture.request); !errors.Is(err, ErrEnrollmentDenied) {
			t.Fatalf("cluster failure error = %v", err)
		}
		if fixture.tx.grant.AttemptID != "" || fixture.tx.node.NodeID != "" {
			t.Fatal("cluster failure left partial rows")
		}
	})
	t.Run("seal failure", func(t *testing.T) {
		fixture := newPrimaryFixture(t)
		fixture.service.sealBootstrap = func(BootstrapRecipientPublicKey, BootstrapBinding, BootstrapGrant) ([]byte, error) {
			return nil, ErrBootstrapDenied
		}
		if _, err := fixture.service.IssueGrant(context.Background(), fixture.request); !errors.Is(err, ErrEnrollmentDenied) {
			t.Fatalf("seal failure error = %v", err)
		}
		if fixture.tx.grant.AttemptID != "" || fixture.tx.node.NodeID != "" {
			t.Fatal("seal failure left partial rows")
		}
	})
}

func TestPostgresPrimaryGrantRejectsMalformedPersistedRetry(t *testing.T) {
	fixture := newPostgresPrimaryFixture(t, 20*time.Minute)
	if _, err := fixture.service.IssueGrant(context.Background(), fixture.request); err != nil {
		t.Fatalf("first IssueGrant: %v", err)
	}
	issuer := testNodeCredentialIssuer(func(context.Context) (NodeCredentialMaterial, error) {
		t.Fatal("malformed retry regenerated a credential")
		return NodeCredentialMaterial{}, ErrEnrollmentDenied
	})
	retry := NewPrimaryService(fixture.store, PrimaryServiceOptions{Now: func() time.Time { return fixture.now }, CredentialIssuer: issuer})
	if _, err := fixture.pool.Exec(context.Background(), `update enrollment_grants set verified_preview_digest = null where attempt_id = $1`, fixture.request.AttemptID); err != nil {
		t.Fatalf("null preview digest: %v", err)
	}
	if _, err := retry.IssueGrant(context.Background(), fixture.request); !errors.Is(err, ErrEnrollmentDenied) {
		t.Fatalf("null preview digest error = %v", err)
	}
	if _, err := fixture.pool.Exec(context.Background(), `update enrollment_grants set verified_preview_digest = $2, sealed_bootstrap_payload = 'malformed' where attempt_id = $1`, fixture.request.AttemptID, hexDigest(fixture.request.VerifiedPreviewDigest)); err != nil {
		t.Fatalf("malformed payload: %v", err)
	}
	if _, err := retry.IssueGrant(context.Background(), fixture.request); !errors.Is(err, ErrEnrollmentDenied) {
		t.Fatalf("malformed payload error = %v", err)
	}
	if _, err := fixture.pool.Exec(context.Background(), `update enrollment_grants set sealed_bootstrap_payload = $2, payload_hash = 'malformed' where attempt_id = $1`, fixture.request.AttemptID, string([]byte("persisted"))); err != nil {
		t.Fatalf("malformed payload hash: %v", err)
	}
	if _, err := retry.IssueGrant(context.Background(), fixture.request); !errors.Is(err, ErrEnrollmentDenied) {
		t.Fatalf("malformed payload hash error = %v", err)
	}
}

func TestPostgresPrimaryGrantUpdatesLiveIdentityAfterCommit(t *testing.T) {
	fixture := newPostgresPrimaryFixture(t, 20*time.Minute)
	identityService := identity.NewService(identity.NewPgStore(fixture.pool))
	if _, err := identityService.Load(context.Background()); err != nil {
		t.Fatalf("identity Load: %v", err)
	}
	fixture.service.identity = identityService
	if _, err := fixture.service.IssueGrant(context.Background(), fixture.request); err != nil {
		t.Fatalf("IssueGrant: %v", err)
	}
	if identityService.Current().Role != domain.TopologyRolePrimaryWithNodes {
		t.Fatalf("cached identity role = %s, want primary-with-nodes", identityService.Current().Role)
	}
	var durableRole string
	if err := fixture.pool.QueryRow(context.Background(), `select role::text from installation_identity where id = 'default'`).Scan(&durableRole); err != nil {
		t.Fatalf("read durable identity role: %v", err)
	}
	if durableRole != string(domain.TopologyRolePrimaryWithNodes) {
		t.Fatalf("durable identity role = %q, want primary-with-nodes", durableRole)
	}
}

func TestPostgresPrimaryGrantCancellationRollsBack(t *testing.T) {
	fixture := newPostgresPrimaryFixture(t, 20*time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	service := NewPrimaryService(fixture.store, PrimaryServiceOptions{
		Now:              func() time.Time { return fixture.now },
		CredentialIssuer: fixture.service.credentialIssuer,
		SealBootstrap: func(recipient BootstrapRecipientPublicKey, binding BootstrapBinding, grant BootstrapGrant) ([]byte, error) {
			sealed, err := SealBootstrap(recipient, binding, grant)
			cancel()
			return sealed, err
		},
	})
	if _, err := service.IssueGrant(ctx, fixture.request); !errors.Is(err, ErrEnrollmentDenied) {
		t.Fatalf("cancelled transaction error = %v", err)
	}
	assertCount(t, fixture.pool, "enrollment_grants", 0)
	assertCount(t, fixture.pool, "node_credentials", 0)
	assertCount(t, fixture.pool, "cluster_keys", 0)
	var consumedAt *time.Time
	if err := fixture.pool.QueryRow(context.Background(), `
		select consumed_at from enrollment_tokens where token_selector = $1
	`, tokenSelector(t, fixture.request.EnrollmentToken)).Scan(&consumedAt); err != nil {
		t.Fatalf("read cancelled token: %v", err)
	}
	if consumedAt != nil {
		t.Fatal("cancelled transaction consumed the enrollment token")
	}
}

func TestPostgresPrimaryGrantConcurrentIdenticalRequestsReturnOneEnvelope(t *testing.T) {
	fixture := newPostgresPrimaryFixture(t, 20*time.Minute)
	results := make(chan primaryGrantResult, 2)
	var wait sync.WaitGroup
	for i := 0; i < 2; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			response, err := fixture.service.IssueGrant(context.Background(), fixture.request)
			results <- primaryGrantResult{response: response, err: err}
		}()
	}
	wait.Wait()
	close(results)
	var responses [][]byte
	for result := range results {
		if result.err != nil {
			t.Fatalf("identical concurrent IssueGrant: %v", result.err)
		}
		responses = append(responses, result.response.SealedBootstrapPayload)
	}
	if len(responses) != 2 || !bytes.Equal(responses[0], responses[1]) {
		t.Fatal("identical concurrent requests did not return the same envelope")
	}
	if fixture.issuerCalls != 1 {
		t.Fatalf("issuer calls = %d, want 1", fixture.issuerCalls)
	}
	assertCount(t, fixture.pool, "node_credentials", 1)
	assertCount(t, fixture.pool, "enrollment_grants", 1)
}

func TestPostgresPrimaryGrantConcurrentDifferentBindingHasOneSuccess(t *testing.T) {
	fixture := newPostgresPrimaryFixture(t, 20*time.Minute)
	otherRecipient, err := NewBootstrapRecipient()
	if err != nil {
		t.Fatalf("other recipient: %v", err)
	}
	defer otherRecipient.Destroy()
	otherPublicKey, err := otherRecipient.PublicKey()
	if err != nil {
		t.Fatalf("other public key: %v", err)
	}
	otherRequest := fixture.request
	otherRequest.RecipientPublicKey = otherPublicKey
	results := make(chan primaryGrantResult, 2)
	var wait sync.WaitGroup
	for _, request := range []PrimaryGrantRequest{fixture.request, otherRequest} {
		request := request
		wait.Add(1)
		go func() {
			defer wait.Done()
			response, err := fixture.service.IssueGrant(context.Background(), request)
			results <- primaryGrantResult{response: response, err: err}
		}()
	}
	wait.Wait()
	close(results)
	successes := 0
	for result := range results {
		if result.err == nil {
			successes++
		} else if !errors.Is(result.err, ErrEnrollmentDenied) {
			t.Fatalf("different concurrent error = %v", result.err)
		}
	}
	if successes != 1 {
		t.Fatalf("different-binding successes = %d, want 1", successes)
	}
	assertCount(t, fixture.pool, "node_credentials", 1)
	assertCount(t, fixture.pool, "enrollment_grants", 1)
}

func TestPostgresPrimaryGrantRollsBackOnDuplicateNodeBinding(t *testing.T) {
	fixture := newPostgresPrimaryFixture(t, 20*time.Minute)
	if _, err := fixture.service.IssueGrant(context.Background(), fixture.request); err != nil {
		t.Fatalf("first IssueGrant: %v", err)
	}
	secondToken, err := fixture.tokenStore.CreateToken(context.Background(), fixture.owner)
	if err != nil {
		t.Fatalf("second token: %v", err)
	}
	secondAttempt := uuidString(t)
	request := fixture.request
	request.EnrollmentToken = secondToken.Token
	request.AttemptID = secondAttempt
	if _, err := fixture.service.IssueGrant(context.Background(), request); !errors.Is(err, ErrEnrollmentDenied) {
		t.Fatalf("duplicate binding error = %v", err)
	}
	assertCount(t, fixture.pool, "node_credentials", 1)
	assertCount(t, fixture.pool, "enrollment_grants", 1)
	var consumedAt *time.Time
	if err := fixture.pool.QueryRow(context.Background(), `select consumed_at from enrollment_tokens where id = $1`, secondToken.ID).Scan(&consumedAt); err != nil {
		t.Fatalf("read rolled-back token: %v", err)
	}
	if consumedAt != nil {
		t.Fatal("duplicate binding consumed its token despite rollback")
	}
}

func TestPostgresPrimaryGrantRetrySurvivesTokenExpiryAndRevocation(t *testing.T) {
	t.Run("token expiry", func(t *testing.T) {
		fixture := newPostgresPrimaryFixture(t, 20*time.Minute)
		first, err := fixture.service.IssueGrant(context.Background(), fixture.request)
		if err != nil {
			t.Fatalf("first IssueGrant: %v", err)
		}
		late := NewPrimaryService(fixture.store, PrimaryServiceOptions{
			Now: func() time.Time { return fixture.now.Add(11 * time.Minute) },
			CredentialIssuer: testNodeCredentialIssuer(func(context.Context) (NodeCredentialMaterial, error) {
				t.Fatal("expired-token retry regenerated a credential")
				return NodeCredentialMaterial{}, ErrEnrollmentDenied
			}),
		})
		second, err := late.IssueGrant(context.Background(), fixture.request)
		if err != nil {
			t.Fatalf("exact retry after token expiry: %v", err)
		}
		if !bytes.Equal(first.SealedBootstrapPayload, second.SealedBootstrapPayload) {
			t.Fatal("token-expiry retry changed sealed bytes")
		}
	})
	t.Run("token revocation", func(t *testing.T) {
		fixture := newPostgresPrimaryFixture(t, 20*time.Minute)
		first, err := fixture.service.IssueGrant(context.Background(), fixture.request)
		if err != nil {
			t.Fatalf("first IssueGrant: %v", err)
		}
		if err := fixture.tokenStore.RevokeToken(context.Background(), fixture.owner, tokenID(t, fixture.request.EnrollmentToken, fixture.pool)); err != nil {
			t.Fatalf("revoke consumed token: %v", err)
		}
		restarted := NewPrimaryService(fixture.store, PrimaryServiceOptions{
			Now: func() time.Time { return fixture.now },
			CredentialIssuer: testNodeCredentialIssuer(func(context.Context) (NodeCredentialMaterial, error) {
				t.Fatal("revoked-token retry regenerated a credential")
				return NodeCredentialMaterial{}, ErrEnrollmentDenied
			}),
		})
		second, err := restarted.IssueGrant(context.Background(), fixture.request)
		if err != nil {
			t.Fatalf("exact retry after token revocation: %v", err)
		}
		if !bytes.Equal(first.SealedBootstrapPayload, second.SealedBootstrapPayload) {
			t.Fatal("token-revocation retry changed sealed bytes")
		}
	})
}

type primaryGrantResult struct {
	response PrimaryGrantResponse
	err      error
}
