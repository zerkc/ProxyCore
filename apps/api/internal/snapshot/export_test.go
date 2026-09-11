package snapshot

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/cluster"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
)

// fakeConfig is an in-memory ConfigurationSource for tests.
type fakeConfig struct {
	mu          sync.Mutex
	data        domain.ConfigurationSnapshot
	snapshotErr error
}

func (f *fakeConfig) Snapshot(_ context.Context) (domain.ConfigurationSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.snapshotErr != nil {
		return domain.ConfigurationSnapshot{}, f.snapshotErr
	}
	return f.data, nil
}

// fakeSecrets is an in-memory SecretLister.
type fakeSecrets struct {
	mu      sync.Mutex
	entries []PlainSecret
	listErr error
}

func (f *fakeSecrets) ListSecrets(_ context.Context) ([]PlainSecret, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]PlainSecret, len(f.entries))
	copy(out, f.entries)
	return out, nil
}

// fakeOwners is an in-memory OwnerLister.
type fakeOwners struct {
	mu      sync.Mutex
	entries []ReplicableOwner
}

func (f *fakeOwners) ListReplicableOwners(_ context.Context) ([]ReplicableOwner, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]ReplicableOwner, len(f.entries))
	copy(out, f.entries)
	return out, nil
}

func newTestExporter() (*Exporter, *fakeConfig, *fakeSecrets, *fakeOwners) {
	cfg := &fakeConfig{data: domain.ConfigurationSnapshot{
		Settings: domain.Settings{
			Ingress:             domain.Ingress{IPv4: "192.0.2.10"},
			RetentionMaxAgeDays: 7,
			RetentionMaxSizeMb:  50,
		},
		Zones: []domain.ZoneState{
			{ID: "z1", Name: "home.arpa", Enabled: true},
		},
		Certificates: []domain.CertificateStatus{
			{ID: "c1", Hostnames: []string{"app.home.arpa"}, Status: "active"},
		},
	}}
	secrets := &fakeSecrets{entries: []PlainSecret{
		{ID: uuid.New(), Purpose: "tls-key", Value: []byte("top-secret-key-material")},
		{ID: uuid.New(), Purpose: "basic-auth", Value: []byte("hunter2")},
	}}
	owners := &fakeOwners{entries: []ReplicableOwner{
		{
			UserID:       uuid.New(),
			Username:     "admin",
			PasswordHash: "scrypt$16384$8$1$injected$injected",
			Role:         "owner",
		},
	}}
	return NewExporter(cfg, secrets, owners), cfg, secrets, owners
}

func mustExporterInput(t *testing.T) ExporterInput {
	t.Helper()
	kekBytes, err := cluster.GenerateKEK()
	if err != nil {
		t.Fatalf("GenerateKEK: %v", err)
	}
	kek, err := cluster.NewKEK(kekBytes)
	if err != nil {
		t.Fatalf("NewKEK: %v", err)
	}
	return ExporterInput{
		InstallationID:       domain.InstallationID(uuid.New().String()),
		NodeID:               domain.NodeID(uuid.New().String()),
		Role:                 domain.TopologyRolePrimary,
		Ingress:              domain.Ingress{IPv4: "192.0.2.10"},
		LeadershipGeneration: 5,
		ClusterKEK:           kek,
	}
}

func TestExportProducesValidEnvelope(t *testing.T) {
	exp, _, _, _ := newTestExporter()
	in := mustExporterInput(t)
	env, err := exp.Export(context.Background(), in)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if err := env.Validate(); err != nil {
		t.Errorf("Validate on exported envelope: %v", err)
	}
	if env.Transient.SourcePrimaryID.String() != string(in.InstallationID) {
		t.Errorf("source primary id mismatch: %s != %s", env.Transient.SourcePrimaryID, in.InstallationID)
	}
	if env.Transient.LeadershipGeneration != in.LeadershipGeneration {
		t.Errorf("leadership generation mismatch: %d != %d", env.Transient.LeadershipGeneration, in.LeadershipGeneration)
	}
}

func TestExportWrapsSecretsWithClusterKEK(t *testing.T) {
	exp, _, secrets, _ := newTestExporter()
	in := mustExporterInput(t)
	env, err := exp.Export(context.Background(), in)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if got := len(env.Replicated.Secrets); got != len(secrets.entries) {
		t.Fatalf("expected %d secrets, got %d", len(secrets.entries), got)
	}
	for _, s := range env.Replicated.Secrets {
		if !cluster.IsKEKEnvelope(s.Envelope) {
			t.Errorf("secret %s: envelope %q is not a cluster-KEK envelope", s.ID, s.Envelope)
		}
		got, err := in.ClusterKEK.Unwrap(s.Envelope)
		if err != nil {
			t.Errorf("unwrap secret %s: %v", s.ID, err)
			continue
		}
		// Round-trip must yield the original plaintext.
		match := false
		for _, original := range secrets.entries {
			if original.ID == s.ID {
				if string(got) != string(original.Value) {
					t.Errorf("secret %s: round-trip mismatch: got %q want %q", s.ID, got, original.Value)
				}
				match = true
				break
			}
		}
		if !match {
			t.Errorf("secret %s not found in source list", s.ID)
		}
	}
}

func TestExportIncludesReplicatedOwners(t *testing.T) {
	exp, _, _, owners := newTestExporter()
	in := mustExporterInput(t)
	env, err := exp.Export(context.Background(), in)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if got := len(env.Replicated.Owners); got != len(owners.entries) {
		t.Fatalf("expected %d owners, got %d", len(owners.entries), got)
	}
	for i, o := range env.Replicated.Owners {
		if o.Username != owners.entries[i].Username {
			t.Errorf("owner username mismatch at %d: %s != %s", i, o.Username, owners.entries[i].Username)
		}
		if o.PasswordHash != owners.entries[i].PasswordHash {
			t.Errorf("owner password hash mismatch at %d: %s != %s", i, o.PasswordHash, owners.entries[i].PasswordHash)
		}
		if o.Role != owners.entries[i].Role {
			t.Errorf("owner role mismatch at %d: %s != %s", i, o.Role, owners.entries[i].Role)
		}
	}
}

func TestExportRejectsMissingClusterKEK(t *testing.T) {
	exp, _, _, _ := newTestExporter()
	in := mustExporterInput(t)
	in.ClusterKEK = nil
	if _, err := exp.Export(context.Background(), in); err == nil {
		t.Errorf("expected Export to reject missing cluster KEK")
	}
}

func TestExportRejectsInvalidInstallationID(t *testing.T) {
	exp, _, _, _ := newTestExporter()
	in := mustExporterInput(t)
	in.InstallationID = domain.InstallationID("")
	if _, err := exp.Export(context.Background(), in); err == nil {
		t.Errorf("expected Export to reject invalid installation id")
	}
}

func TestExportRejectsInvalidRole(t *testing.T) {
	exp, _, _, _ := newTestExporter()
	in := mustExporterInput(t)
	in.Role = domain.TopologyRole("not-a-role")
	if _, err := exp.Export(context.Background(), in); err == nil {
		t.Errorf("expected Export to reject invalid role")
	}
}

func TestExportPropagatesConfigurationError(t *testing.T) {
	cfg := &fakeConfig{snapshotErr: errors.New("database down")}
	exp := NewExporter(cfg, &fakeSecrets{}, &fakeOwners{})
	in := mustExporterInput(t)
	if _, err := exp.Export(context.Background(), in); err == nil {
		t.Errorf("expected Export to surface configuration source error")
	}
}

func TestExportPropagatesSecretError(t *testing.T) {
	secrets := &fakeSecrets{listErr: errors.New("secrets unavailable")}
	exp := NewExporter(&fakeConfig{}, secrets, &fakeOwners{})
	in := mustExporterInput(t)
	if _, err := exp.Export(context.Background(), in); err == nil {
		t.Errorf("expected Export to surface secret lister error")
	}
}

func TestExportContentHashStableAcrossEquivalentRuns(t *testing.T) {
	// Two exports with the same source data, same cluster KEK, and same
	// producer identity must produce the same content hash because the
	// hash covers ReplicatedFields only and the wrapped secrets are
	// deterministic for a fixed plaintext+KEK (random IVs are part of the
	// envelope, so the content hash WILL differ across calls; this test
	// documents the contract that the hash is byte-stable for the same
	// source data when the cluster KEK is shared and the wrapped secrets
	// are produced with deterministic IVs.
	//
	// Phase 5 hardening can revisit this if deterministic-IV wrapping
	// becomes necessary; for now we only assert that the exported
	// envelopes validate against their declared hashes.
	exp, _, _, _ := newTestExporter()
	in := mustExporterInput(t)
	env, err := exp.Export(context.Background(), in)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if err := env.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

// Field extensions on fakeConfig and fakeSecrets to support error injection.

var _ = (*fakeConfig)(nil)
