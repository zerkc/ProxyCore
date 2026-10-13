package cluster

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/identity"
)

// The parity matrix runs identical LocalKEKLoader scenarios over every
// KeyLoader backend: a fake and the SQLite KeyStore. Assertions never print
// key bytes.

type loaderBackend struct {
	name    string
	loader  KeyLoader
	knownID uuid.UUID
	refKey  []byte
}

func fakeLoaderBackend(t *testing.T) loaderBackend {
	t.Helper()
	id, key := uuid.New(), seedKey(0x21)
	return loaderBackend{name: "fake", knownID: id, refKey: key, loader: keyLoaderFunc(func(_ context.Context, got uuid.UUID) (KeyMaterial, error) {
		if got != id {
			return KeyMaterial{}, ErrClusterKeyDenied
		}
		return NewKeyMaterial(id, key)
	})}
}

func sqliteLoaderBackend(t *testing.T) loaderBackend {
	t.Helper()
	f := newSQLiteKeyFixture(t)
	id, key := uuid.New(), seedKey(0x42)
	f.seedRow(t, id, key, ClusterKeyPurpose, ClusterKeyWrappingVersion, stampText(t, 1_700_000_000), nil)
	return loaderBackend{name: "sqlite", loader: f.store, knownID: id, refKey: key}
}

type keyLoaderFunc func(context.Context, uuid.UUID) (KeyMaterial, error)

func (f keyLoaderFunc) LoadActive(ctx context.Context, id uuid.UUID) (KeyMaterial, error) {
	return f(ctx, id)
}

type countingKeyLoader struct {
	inner KeyLoader
	calls int
}

func (c *countingKeyLoader) LoadActive(ctx context.Context, id uuid.UUID) (KeyMaterial, error) {
	c.calls++
	return c.inner.LoadActive(ctx, id)
}

func parityIdentity(role domain.TopologyRole, keyID *uuid.UUID) localLoaderIdentitySource {
	return localLoaderIdentitySource{current: identity.Identity{
		InstallationID: domain.NewInstallationID(), NodeID: domain.NewNodeID(), Role: role,
		LeadershipGeneration: 2, LatestKnownGeneration: 2, ClusterKeyID: keyID,
	}}
}

func TestLocalKEKLoaderParityAcrossKeyLoaders(t *testing.T) {
	backends := []func(*testing.T) loaderBackend{fakeLoaderBackend, sqliteLoaderBackend}
	for _, newBackend := range backends {
		b := newBackend(t)
		t.Run(b.name, func(t *testing.T) {
			ref, err := NewKEK(b.refKey)
			if err != nil {
				t.Fatal(err)
			}
			defer ref.Destroy()
			unknown := uuid.New()
			otherID := uuid.New()
			cases := []struct {
				name      string
				source    localLoaderIdentitySource
				wrap      func(KeyLoader) KeyLoader
				want      error
				wantCalls int
			}{
				{name: "node loads", source: parityIdentity(domain.TopologyRoleNode, &b.knownID), wantCalls: 1},
				{name: "primary with nodes loads", source: parityIdentity(domain.TopologyRolePrimaryWithNodes, &b.knownID), wantCalls: 1},
				{name: "standalone primary denied", source: parityIdentity(domain.TopologyRoleStandalone, &b.knownID), want: ErrLocalKEKLoaderDenied},
				{name: "primary denied", source: parityIdentity(domain.TopologyRolePrimary, &b.knownID), want: ErrLocalKEKLoaderDenied},
				{name: "stale primary denied", source: parityIdentity(domain.TopologyRoleStalePrimary, &b.knownID), want: ErrLocalKEKLoaderDenied},
				{name: "node without cluster key id", source: parityIdentity(domain.TopologyRoleNode, nil), want: ErrLocalKEKLoaderNotReady},
				{name: "primary with nodes without cluster key id", source: parityIdentity(domain.TopologyRolePrimaryWithNodes, nil), want: ErrLocalKEKLoaderNotReady},
				{name: "identity not ready", source: localLoaderIdentitySource{}, want: ErrLocalKEKLoaderNotReady},
				{name: "unknown key id", source: parityIdentity(domain.TopologyRoleNode, &unknown), want: ErrLocalKEKLoaderStore, wantCalls: 1},
				{name: "loaded key id differs", source: parityIdentity(domain.TopologyRoleNode, &b.knownID), want: ErrLocalKEKLoaderStore, wantCalls: 1,
					wrap: func(inner KeyLoader) KeyLoader {
						return keyLoaderFunc(func(ctx context.Context, id uuid.UUID) (KeyMaterial, error) {
							m, err := inner.LoadActive(ctx, id)
							if err != nil {
								return m, err
							}
							defer m.Destroy()
							return NewKeyMaterial(otherID, m.CopyBytes())
						})
					}},
				{name: "store failure", source: parityIdentity(domain.TopologyRoleNode, &b.knownID), want: ErrLocalKEKLoaderStore, wantCalls: 1,
					wrap: func(KeyLoader) KeyLoader {
						return keyLoaderFunc(func(context.Context, uuid.UUID) (KeyMaterial, error) { return KeyMaterial{}, errors.New("boom") })
					}},
				{name: "key denied maps to store error", source: parityIdentity(domain.TopologyRoleNode, &b.knownID), want: ErrLocalKEKLoaderStore, wantCalls: 1,
					wrap: func(KeyLoader) KeyLoader {
						return keyLoaderFunc(func(context.Context, uuid.UUID) (KeyMaterial, error) { return KeyMaterial{}, ErrClusterKeyDenied })
					}},
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					inner := b.loader
					if tc.wrap != nil {
						inner = tc.wrap(inner)
					}
					counter := &countingKeyLoader{inner: inner}
					kek, id, err := NewLocalKEKLoader(LocalKEKLoaderOptions{Keys: counter, Identity: tc.source}).LoadLocalKEK(context.Background())
					if counter.calls != tc.wantCalls {
						t.Fatalf("key loader calls=%d, want %d", counter.calls, tc.wantCalls)
					}
					if tc.want != nil {
						if !errors.Is(err, tc.want) || kek != nil || id != uuid.Nil {
							t.Fatalf("error=%v kek=%v id=%v, want %v", err, kek != nil, id, tc.want)
						}
						return
					}
					if err != nil {
						t.Fatalf("LoadLocalKEK: %v", err)
					}
					defer kek.Destroy()
					if id != b.knownID {
						t.Fatalf("loaded id=%s want %s", id, b.knownID)
					}
					wrapped, err := kek.Wrap([]byte("parity"))
					if err != nil {
						t.Fatal(err)
					}
					plain, err := ref.Unwrap(wrapped)
					if err != nil || !bytes.Equal(plain, []byte("parity")) {
						t.Fatal("loaded KEK does not match the persisted key")
					}
				})
			}
		})
	}
}

func TestLocalKEKLoaderNilKeyLoaderIsStoreError(t *testing.T) {
	id := uuid.New()
	_, _, err := NewLocalKEKLoader(LocalKEKLoaderOptions{Identity: parityIdentity(domain.TopologyRoleNode, &id)}).LoadLocalKEK(context.Background())
	if !errors.Is(err, ErrLocalKEKLoaderStore) {
		t.Fatalf("error=%v, want ErrLocalKEKLoaderStore", err)
	}
}

func TestLocalKEKLoaderSQLiteUsesIdentityClusterKeyID(t *testing.T) {
	f := newSQLiteKeyFixture(t)
	first, second := uuid.New(), uuid.New()
	f.seedRow(t, first, seedKey(0x11), ClusterKeyPurpose, ClusterKeyWrappingVersion, stampText(t, 1_700_000_000), nil)
	f.seedRow(t, second, seedKey(0x22), ClusterKeyPurpose, ClusterKeyWrappingVersion, stampText(t, 1_700_000_100), nil)
	// "second" is not the oldest row, so ignoring the identity's ClusterKeyID
	// (loading the oldest active key) would return "first".
	kek, id, err := NewLocalKEKLoader(LocalKEKLoaderOptions{Keys: f.store, Identity: parityIdentity(domain.TopologyRoleNode, &second)}).LoadLocalKEK(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer kek.Destroy()
	if id != second {
		t.Fatalf("loaded id=%s, want identity key id %s", id, second)
	}
	if f.count(t, "") != 2 {
		t.Fatal("loader created or removed key rows")
	}
}
