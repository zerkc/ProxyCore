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
	// buildNodeConverter only returns nil here because of the store guard.
	identitySvc := identity.NewService(&startupIdentityStore{})
	if _, _, err := identitySvc.EnsureBootstrapped(context.Background()); err != nil {
		t.Fatalf("bootstrap identity: %v", err)
	}
	if got := buildNodeConverter(true, (*configuration.SQLitePhase2Store)(nil), identitySvc, nil); got != nil {
		t.Fatal("buildNodeConverter built a converter from a typed-nil store")
	}
	if got := productionSnapshotByTokenStore((*configuration.SQLitePhase2Store)(nil)); got != nil {
		t.Fatal("productionSnapshotByTokenStore returned a store for a typed-nil Phase2 store")
	}
	if got := productionSnapshotByTokenStore(nil); got != nil {
		t.Fatal("productionSnapshotByTokenStore returned a store for a nil reader")
	}
}

func TestEnrollmentTLSConfigurationAdapterTreatsTypedNilStoreAsUnconfigured(t *testing.T) {
	for name, store := range map[string]enrollmentTLSConfigurationStore{
		"nil interface": nil,
		"typed nil":     (*configuration.SQLiteConfigurationBackend)(nil),
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
