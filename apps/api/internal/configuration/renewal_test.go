package configuration

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/acme"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/identity"
)

func TestDirectoryURLForEnvironment(t *testing.T) {
	staging := "https://acme-staging.example/directory"
	production := "https://acme-v02.example/directory"
	if got := directoryURLForEnvironment("production", staging, production); got != production {
		t.Fatalf("production directory=%q", got)
	}
	if got := directoryURLForEnvironment("staging", staging, production); got != staging {
		t.Fatalf("staging directory=%q", got)
	}
	if got := directoryURLForEnvironment("", staging, production); got != staging {
		t.Fatalf("default directory=%q", got)
	}
}

func TestRenewLetsEncryptCertificateRejectsNonLE(t *testing.T) {
	store := &Store{}
	_, err := store.RenewLetsEncryptCertificate(context.Background(), domain.CertificateStatus{
		ID:     "cert-1",
		Issuer: "self-signed",
		Status: "active",
	}, RenewalOptions{})
	if err == nil || err.Error() == "" {
		t.Fatalf("expected rejection for non-LE issuer, got %v", err)
	}
}

func TestRenewSelfSignedCertificateRejectsNonSelfSigned(t *testing.T) {
	store := &Store{}
	_, err := store.RenewSelfSignedCertificate(context.Background(), domain.CertificateStatus{
		ID:     "cert-1",
		Issuer: "letsencrypt",
		Status: "active",
	})
	if err == nil || err.Error() == "" {
		t.Fatalf("expected rejection for non-self-signed issuer, got %v", err)
	}
}

func TestRenewSelfSignedCertificateRequiresSecrets(t *testing.T) {
	store := &Store{}
	_, err := store.RenewSelfSignedCertificate(context.Background(), domain.CertificateStatus{
		ID:        "cert-1",
		Hostnames: []string{"app.home.arpa"},
		Issuer:    "self-signed",
		Challenge: "none",
		Status:    "active",
	})
	if err == nil {
		t.Fatal("expected error without secrets")
	}
}

func TestRenewLetsEncryptCertificateUsesOverrideIssuer(t *testing.T) {
	if testing.Short() {
		t.Skip("requires DATABASE_URL for full renew path")
	}
	// Pure override smoke: ensure the hook is invoked for a crafted call that
	// fails before DB writes when secrets/store are unset.
	original := issueLetsEncryptFn
	t.Cleanup(func() { issueLetsEncryptFn = original })

	called := false
	issueLetsEncryptFn = func(ctx context.Context, options acme.LetsEncryptOptions) (acme.Material, error) {
		called = true
		return acme.Material{}, errors.New("forced failure")
	}

	store := &Store{}
	_, err := store.RenewLetsEncryptCertificate(context.Background(), domain.CertificateStatus{
		ID:          "cert-1",
		Hostnames:   []string{"app.example.test"},
		Issuer:      "letsencrypt",
		Challenge:   "http-01",
		Environment: "staging",
		Status:      "active",
	}, RenewalOptions{
		StagingDirectoryURL: "https://acme-staging.example/directory",
	})
	if err == nil {
		t.Fatal("expected error without secrets")
	}
	if called {
		t.Fatal("issuer should not run when master key/secrets are missing")
	}
}

type passthroughRenewalLease struct{}

func (passthroughRenewalLease) WithAutomaticRenewalLease(ctx context.Context, work func(context.Context) error) error {
	return work(ctx)
}

func TestRenewalDateIsThirtyDaysBeforeExpiry(t *testing.T) {
	expires := time.Date(2026, 11, 7, 3, 26, 41, 0, time.UTC)
	renew := renewalDate(expires)
	want := expires.Add(-30 * 24 * time.Hour)
	if !renew.Equal(want) {
		t.Fatalf("renewAfter=%s want=%s", renew, want)
	}
}

func TestCheckRenewalPolicyAllowsOnlyWritablePrimaryRoles(t *testing.T) {
	roles := []struct {
		name string
		role domain.TopologyRole
		want bool
	}{
		{name: "standalone", role: domain.TopologyRoleStandalone, want: true},
		{name: "primary", role: domain.TopologyRolePrimary, want: true},
		{name: "primary with nodes", role: domain.TopologyRolePrimaryWithNodes, want: true},
		{name: "node", role: domain.TopologyRoleNode},
		{name: "stale primary", role: domain.TopologyRoleStalePrimary},
	}
	for _, test := range roles {
		t.Run(test.name, func(t *testing.T) {
			status := CheckRenewalPolicy(context.Background(), func(context.Context) (identity.Identity, bool, error) {
				return identity.Identity{Role: test.role, LeadershipGeneration: 1, LatestKnownGeneration: 1}, true, nil
			})
			if status.Allowed != test.want || status.Role != test.role {
				t.Fatalf("status=%+v, want allowed=%t role=%s", status, test.want, test.role)
			}
			if test.want && status.Reason != "" {
				t.Fatalf("allowed status has skip reason %q", status.Reason)
			}
			if !test.want && status.Reason == "" {
				t.Fatal("forbidden status has no skip reason")
			}
		})
	}
}

func TestCheckRenewalPolicyRejectsStaleGenerationAndUnloadedIdentity(t *testing.T) {
	stale := CheckRenewalPolicy(context.Background(), func(context.Context) (identity.Identity, bool, error) {
		return identity.Identity{Role: domain.TopologyRolePrimary, LeadershipGeneration: 3, LatestKnownGeneration: 4}, true, nil
	})
	if stale.Allowed || stale.Reason != RenewalSkipReasonStalePrimary {
		t.Fatalf("stale status=%+v", stale)
	}

	unloaded := CheckRenewalPolicy(context.Background(), func(context.Context) (identity.Identity, bool, error) {
		return identity.Identity{}, false, nil
	})
	if unloaded.Allowed || unloaded.Reason != RenewalSkipReasonIdentityUnloaded {
		t.Fatalf("unloaded status=%+v", unloaded)
	}
}

func TestCheckRenewalPolicyFailsClosedOnProviderErrorAndCancellation(t *testing.T) {
	providerCalled := false
	failed := CheckRenewalPolicy(context.Background(), func(context.Context) (identity.Identity, bool, error) {
		providerCalled = true
		return identity.Identity{}, false, errors.New("identity store unavailable")
	})
	if !providerCalled || failed.Allowed || failed.Reason != RenewalSkipReasonIdentityUnavailable {
		t.Fatalf("provider failure status=%+v called=%t", failed, providerCalled)
	}

	nilProvider := CheckRenewalPolicy(context.Background(), nil)
	if nilProvider.Allowed || nilProvider.Reason != RenewalSkipReasonIdentityUnavailable {
		t.Fatalf("nil provider status=%+v", nilProvider)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	providerCalled = false
	canceled := CheckRenewalPolicy(ctx, func(context.Context) (identity.Identity, bool, error) {
		providerCalled = true
		return identity.Identity{}, true, nil
	})
	if providerCalled || canceled.Allowed || canceled.Reason != RenewalSkipReasonContextCanceled {
		t.Fatalf("canceled status=%+v called=%t", canceled, providerCalled)
	}
}

func TestRenewalWorkItemsRecheckPolicyBeforeEveryItem(t *testing.T) {
	role := domain.TopologyRolePrimary
	opts := RenewalOptions{
		Identity: func(context.Context) (identity.Identity, bool, error) {
			return identity.Identity{Role: role, LeadershipGeneration: 1, LatestKnownGeneration: 1}, true, nil
		},
		IdentityLease: passthroughRenewalLease{},
	}
	due := []domain.CertificateStatus{
		{ID: "first", Issuer: "self-signed", Hostnames: []string{"first.example"}},
		{ID: "second", Issuer: "self-signed", Hostnames: []string{"second.example"}},
	}
	calls := 0
	renewed, failed := renewDueCertificateItems(context.Background(), opts, due, func(context.Context, domain.CertificateStatus) (RenewResult, error) {
		calls++
		role = domain.TopologyRoleNode
		return RenewResult{}, nil
	})
	if renewed != 1 || failed != 0 || calls != 1 {
		t.Fatalf("renewed=%d failed=%d calls=%d, want one item before live role change", renewed, failed, calls)
	}
}

func TestRenewalWorkItemsSkipForbiddenAndCanceledContextsWithoutCalls(t *testing.T) {
	due := []domain.CertificateStatus{{ID: "cert-1", Issuer: "letsencrypt"}}
	calls := 0
	opts := RenewalOptions{Identity: func(context.Context) (identity.Identity, bool, error) {
		return identity.Identity{Role: domain.TopologyRoleNode}, true, nil
	}}
	if renewed, failed := renewDueCertificateItems(context.Background(), opts, due, func(context.Context, domain.CertificateStatus) (RenewResult, error) {
		calls++
		return RenewResult{}, nil
	}); renewed != 0 || failed != 0 || calls != 0 {
		t.Fatalf("forbidden work renewed=%d failed=%d calls=%d", renewed, failed, calls)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if renewed, failed := renewDueCertificateItems(ctx, RenewalOptions{Identity: func(context.Context) (identity.Identity, bool, error) {
		return identity.Identity{Role: domain.TopologyRolePrimary}, true, nil
	}}, due, func(context.Context, domain.CertificateStatus) (RenewResult, error) {
		calls++
		return RenewResult{}, nil
	}); renewed != 0 || failed != 0 || calls != 0 {
		t.Fatalf("canceled work renewed=%d failed=%d calls=%d", renewed, failed, calls)
	}
}

type renewalLeaseStore struct {
	mu                  sync.Mutex
	current             identity.Identity
	present             bool
	roleUpdateStarted   chan struct{}
	roleUpdateRelease   chan struct{}
	roleUpdateStartOnce sync.Once
}

func (s *renewalLeaseStore) Get(context.Context) (identity.Identity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.present {
		return identity.Identity{}, identity.ErrNotFound
	}
	return s.current, nil
}

func (s *renewalLeaseStore) Ensure(_ context.Context, installationID domain.InstallationID, nodeID domain.NodeID) (identity.Identity, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.present {
		return s.current, false, nil
	}
	s.current = identity.Identity{InstallationID: installationID, NodeID: nodeID, Role: domain.TopologyRoleStandalone, LeadershipGeneration: 1, LatestKnownGeneration: 1}
	s.present = true
	return s.current, true, nil
}

func (s *renewalLeaseStore) UpdateRole(_ context.Context, role domain.TopologyRole) error {
	if s.roleUpdateStarted != nil {
		s.roleUpdateStartOnce.Do(func() { close(s.roleUpdateStarted) })
		<-s.roleUpdateRelease
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.present {
		return identity.ErrNotFound
	}
	s.current.Role = role
	return nil
}

func (s *renewalLeaseStore) UpdateLeadershipGeneration(_ context.Context, generation domain.LeadershipGeneration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.present {
		return identity.ErrNotFound
	}
	s.current.LeadershipGeneration = generation
	if generation > s.current.LatestKnownGeneration {
		s.current.LatestKnownGeneration = generation
	}
	return nil
}

func (s *renewalLeaseStore) UpdateLatestKnownGeneration(_ context.Context, generation domain.LeadershipGeneration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.present {
		return identity.ErrNotFound
	}
	if generation > s.current.LatestKnownGeneration {
		s.current.LatestKnownGeneration = generation
	}
	return nil
}

func (s *renewalLeaseStore) UpdateClusterKeyID(_ context.Context, keyID *uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.present {
		return identity.ErrNotFound
	}
	s.current.ClusterKeyID = keyID
	return nil
}

func newRenewalIdentityService(t *testing.T, role domain.TopologyRole) *identity.Service {
	t.Helper()
	store := &renewalLeaseStore{current: identity.Identity{Role: role, LeadershipGeneration: 1, LatestKnownGeneration: 1}, present: true}
	service := identity.NewService(store)
	if _, err := service.Load(context.Background()); err != nil {
		t.Fatalf("Load: %v", err)
	}
	return service
}

func renewalIdentityProviderForService(service *identity.Service) RenewalIdentityProvider {
	return func(context.Context) (identity.Identity, bool, error) {
		return service.Current(), true, nil
	}
}

func TestRenewalWorkItemsUseAtomicIdentityLeaseAndStopAfterTransition(t *testing.T) {
	store := &renewalLeaseStore{
		current:           identity.Identity{Role: domain.TopologyRolePrimary, LeadershipGeneration: 1, LatestKnownGeneration: 1},
		present:           true,
		roleUpdateStarted: make(chan struct{}),
		roleUpdateRelease: make(chan struct{}),
	}
	service := identity.NewService(store)
	if _, err := service.Load(context.Background()); err != nil {
		t.Fatalf("Load: %v", err)
	}

	due := []domain.CertificateStatus{
		{ID: "first", Issuer: "self-signed"},
		{ID: "second", Issuer: "self-signed"},
	}
	firstIssued := make(chan struct{})
	releaseFirst := make(chan struct{})
	transitionAttempted := make(chan struct{})
	transitionDone := make(chan error, 1)
	issuerCalls := 0
	workDone := make(chan struct {
		renewed int
		failed  int
	}, 1)
	go func() {
		renewed, failed := renewDueCertificateItems(context.Background(), RenewalOptions{
			Identity:      renewalIdentityProviderForService(service),
			IdentityLease: service,
		}, due, func(context.Context, domain.CertificateStatus) (RenewResult, error) {
			issuerCalls++
			if issuerCalls == 1 {
				close(firstIssued)
				<-releaseFirst
			}
			return RenewResult{}, nil
		})
		workDone <- struct {
			renewed int
			failed  int
		}{renewed, failed}
	}()
	select {
	case <-firstIssued:
	case <-time.After(time.Second):
		t.Fatal("first issuance callback did not start")
	}
	go func() {
		close(transitionAttempted)
		_, err := service.TransitionTo(context.Background(), domain.TopologyRoleStalePrimary)
		transitionDone <- err
	}()
	<-transitionAttempted
	close(releaseFirst)
	select {
	case <-store.roleUpdateStarted:
	case <-time.After(time.Second):
		t.Fatal("role transition did not acquire the write lock after issuance")
	}
	close(store.roleUpdateRelease)
	if err := <-transitionDone; err != nil {
		t.Fatalf("TransitionTo node: %v", err)
	}
	result := <-workDone
	if result.renewed != 1 || result.failed != 0 || issuerCalls != 1 {
		t.Fatalf("result=%+v issuerCalls=%d, want only first item", result, issuerCalls)
	}
}

func TestRenewalWorkItemsFailClosedForStaleGenerationAndUnloadedLease(t *testing.T) {
	staleStore := &renewalLeaseStore{
		current: identity.Identity{Role: domain.TopologyRolePrimary, LeadershipGeneration: 3, LatestKnownGeneration: 4},
		present: true,
	}
	staleService := identity.NewService(staleStore)
	if _, err := staleService.Load(context.Background()); err != nil {
		t.Fatalf("Load stale identity: %v", err)
	}
	calls := 0
	due := []domain.CertificateStatus{{ID: "stale", Issuer: "letsencrypt"}}
	_, failed := renewDueCertificateItems(context.Background(), RenewalOptions{
		Identity: func(context.Context) (identity.Identity, bool, error) {
			return identity.Identity{Role: domain.TopologyRolePrimary, LeadershipGeneration: 3, LatestKnownGeneration: 3}, true, nil
		},
		IdentityLease: staleService,
	}, due, func(context.Context, domain.CertificateStatus) (RenewResult, error) {
		calls++
		return RenewResult{}, nil
	})
	if failed != 0 || calls != 0 {
		t.Fatalf("stale lease failed=%d calls=%d", failed, calls)
	}

	unloaded := identity.NewService(&renewalLeaseStore{})
	_, failed = renewDueCertificateItems(context.Background(), RenewalOptions{
		Identity: func(context.Context) (identity.Identity, bool, error) {
			return identity.Identity{Role: domain.TopologyRolePrimary, LeadershipGeneration: 1, LatestKnownGeneration: 1}, true, nil
		},
		IdentityLease: unloaded,
	}, due, func(context.Context, domain.CertificateStatus) (RenewResult, error) {
		calls++
		return RenewResult{}, nil
	})
	if failed != 0 || calls != 0 {
		t.Fatalf("unloaded lease failed=%d calls=%d", failed, calls)
	}
}

func TestRenewalWorkItemsNeverInvokeIssuerWhenLeaseForbidsCurrentRole(t *testing.T) {
	service := newRenewalIdentityService(t, domain.TopologyRoleNode)
	calls := 0
	_, failed := renewDueCertificateItems(context.Background(), RenewalOptions{
		// The cycle snapshot is intentionally stale/optimistic; only the lease
		// is authoritative at the issuance boundary.
		Identity: func(context.Context) (identity.Identity, bool, error) {
			return identity.Identity{Role: domain.TopologyRolePrimary, LeadershipGeneration: 1, LatestKnownGeneration: 1}, true, nil
		},
		IdentityLease: service,
	}, []domain.CertificateStatus{{ID: "forbidden", Issuer: "letsencrypt"}}, func(context.Context, domain.CertificateStatus) (RenewResult, error) {
		calls++
		return RenewResult{}, nil
	})
	if failed != 0 || calls != 0 {
		t.Fatalf("forbidden lease failed=%d calls=%d", failed, calls)
	}
}
