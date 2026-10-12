package enrollment

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
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

type primaryGrantResult struct {
	response PrimaryGrantResponse
	err      error
}
