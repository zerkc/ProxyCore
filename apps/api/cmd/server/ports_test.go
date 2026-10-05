package main

import (
	"context"
	"errors"
	"testing"

	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
	"github.com/zerkc/ProxyCore/apps/api/internal/enrollment"
	"github.com/zerkc/ProxyCore/apps/api/internal/identity"
)

func TestTypedNilStoresKeepNilSemantics(t *testing.T) {
	// A bootstrapped standalone identity is role-eligible, so buildNodeConverter
	// only returns nil here because of the store guard.
	identitySvc := identity.NewService(&startupIdentityStore{})
	if _, _, err := identitySvc.EnsureBootstrapped(context.Background()); err != nil {
		t.Fatalf("bootstrap identity: %v", err)
	}
	if !nodeConverterRoleEligible(identitySvc) {
		t.Fatal("test identity must be role-eligible to reach the store check")
	}
	if got := buildNodeConverter(true, (*configuration.Store)(nil), identitySvc, nil); got != nil {
		t.Fatal("buildNodeConverter built a converter from a typed-nil store")
	}
	if got := productionSnapshotByTokenStore((*configuration.PgPhase2Store)(nil)); got != nil {
		t.Fatal("productionSnapshotByTokenStore returned a store for a typed-nil Phase2 store")
	}
	if got := productionSnapshotByTokenStore(nil); got != nil {
		t.Fatal("productionSnapshotByTokenStore returned a store for a nil reader")
	}
}

func TestEnrollmentTLSConfigurationAdapterTreatsTypedNilStoreAsUnconfigured(t *testing.T) {
	for name, store := range map[string]enrollmentTLSConfigurationStore{
		"nil interface": nil,
		"typed nil":     (*configuration.Store)(nil),
	} {
		adapter := &enrollmentTLSConfigurationAdapter{store: store}
		if _, err := adapter.GetEnrollmentHostnames(context.Background()); !errors.Is(err, enrollment.ErrEnrollmentTLSConfigurationUnavailable) {
			t.Fatalf("%s: GetEnrollmentHostnames error = %v", name, err)
		}
		if _, err := adapter.EnsureEnrollmentTLSMaterial(context.Background()); !errors.Is(err, enrollment.ErrEnrollmentTLSMaterialUnavailable) {
			t.Fatalf("%s: EnsureEnrollmentTLSMaterial error = %v", name, err)
		}
	}
}
