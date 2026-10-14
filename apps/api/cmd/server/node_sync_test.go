package main

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/configuration"
	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	"github.com/zerkc/ProxyCore/apps/api/internal/enrollment"
	"github.com/zerkc/ProxyCore/apps/api/internal/identity"
	replicationsnapshot "github.com/zerkc/ProxyCore/apps/api/internal/snapshot"
	"github.com/zerkc/ProxyCore/apps/api/internal/sqlitedb"
	syncpublication "github.com/zerkc/ProxyCore/apps/api/internal/sync"
)

func TestNodeSyncIntervalFromEnv(t *testing.T) {
	cases := map[string]time.Duration{
		"":          syncpublication.DefaultNodeSyncInterval,
		"45s":       45 * time.Second,
		"2m":        2 * time.Minute,
		"junk":      syncpublication.DefaultNodeSyncInterval,
		"-1s":       syncpublication.DefaultNodeSyncInterval,
		"1s":        minNodeSyncInterval,
		"   10s   ": 10 * time.Second,
	}
	for value, want := range cases {
		t.Setenv(nodeSyncIntervalEnv, value)
		if got := nodeSyncIntervalFromEnv(); got != want {
			t.Fatalf("%q => %v, want %v", value, got, want)
		}
	}
}

type recordingSyncClient struct {
	bearers []string
	fetched []string
	acked   []string
}

func (c *recordingSyncClient) FetchSnapshotIfChanged(_ context.Context, url string, credential []byte, after string) (syncpublication.SnapshotFetchResult, error) {
	c.bearers = append(c.bearers, string(credential))
	c.fetched = append(c.fetched, url+"|"+after)
	return syncpublication.SnapshotFetchResult{NotModified: true, ContentHash: after, AppliedAt: time.Unix(1_800_000_000, 0).UTC()}, nil
}

func (c *recordingSyncClient) AcknowledgeSnapshot(_ context.Context, _ string, credential []byte, nodeID, hash string, _ time.Time) error {
	c.bearers = append(c.bearers, string(credential))
	c.acked = append(c.acked, nodeID+"|"+hash)
	return nil
}

type noopResyncer struct{}

func (noopResyncer) Resync(context.Context, enrollment.NodeResyncInput) (enrollment.NodeResyncResult, error) {
	return enrollment.NodeResyncResult{}, nil
}

type fakeSyncStore struct {
	state configuration.NodeSyncState
	saved []configuration.NodeSyncProgress
}

func (s *fakeSyncStore) BindNodeSync(context.Context, configuration.NodeSyncBinding, string, time.Time) error {
	return nil
}
func (s *fakeSyncStore) LoadNodeSyncState(context.Context) (configuration.NodeSyncState, bool, error) {
	return s.state, true, nil
}
func (s *fakeSyncStore) SaveNodeSyncProgress(_ context.Context, p configuration.NodeSyncProgress) error {
	s.saved = append(s.saved, p)
	return nil
}

type acceptAllValidator struct{}

func (acceptAllValidator) Validate(context.Context, replicationsnapshot.Envelope) replicationsnapshot.ValidationResult {
	return replicationsnapshot.ValidationResult{OK: true}
}

func nodeSyncTestIdentity(t *testing.T) *identity.Service {
	t.Helper()
	db, err := sqlitedb.Open(filepath.Join(t.TempDir(), "sync.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := sqlitedb.Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	service := identity.NewService(identity.NewSQLiteStore(db, sqlitedb.NewWriter(db, nil)))
	if _, _, err := service.EnsureBootstrapped(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := service.TransitionTo(context.Background(), domain.TopologyRoleNode); err != nil {
		t.Fatal(err)
	}
	return service
}

func TestNewNodeSyncerRefusesIncompleteWiring(t *testing.T) {
	if newNodeSyncer(nodeSyncDependencies{}) != nil {
		t.Fatal("a syncer without dependencies must not be created")
	}
}

func TestNodeSyncerReportsCredentialRejectedWhenNoLocalCredential(t *testing.T) {
	store := &fakeSyncStore{state: configuration.NodeSyncState{
		NodeSyncBinding:  configuration.NodeSyncBinding{PrimaryURL: "https://primary.example:3443", CredentialID: uuid.NewString()},
		NodeSyncProgress: configuration.NodeSyncProgress{AppliedHash: strings.Repeat("a", 64), PrimaryHash: strings.Repeat("a", 64)},
	}}
	client := &recordingSyncClient{}
	syncer := newNodeSyncer(nodeSyncDependencies{
		Store: store, Identity: nodeSyncTestIdentity(t), Client: client, Converter: noopResyncer{},
		Validator:      acceptAllValidator{},
		LoadCredential: func(context.Context, string) ([]byte, error) { return nil, errors.New("missing") },
	})
	err := syncer.RunOnce(context.Background())
	var failure *syncpublication.NodeSyncFailure
	if !errors.As(err, &failure) || failure.Code != "credential_rejected" || len(client.fetched) != 0 {
		t.Fatalf("err=%v fetched=%v", err, client.fetched)
	}
}

func TestNodeSyncerFetchesWithStoredCredentialAndAcknowledges(t *testing.T) {
	credentialID := uuid.NewString()
	applied := strings.Repeat("b", 64)
	store := &fakeSyncStore{state: configuration.NodeSyncState{
		NodeSyncBinding:  configuration.NodeSyncBinding{PrimaryURL: "https://primary.example:3443", CredentialID: credentialID},
		NodeSyncProgress: configuration.NodeSyncProgress{AppliedHash: applied, PrimaryHash: applied},
	}}
	client := &recordingSyncClient{}
	service := nodeSyncTestIdentity(t)
	syncer := newNodeSyncer(nodeSyncDependencies{
		Store: store, Identity: service, Client: client, Converter: noopResyncer{}, Validator: acceptAllValidator{},
		LoadCredential: func(_ context.Context, id string) ([]byte, error) {
			if id != credentialID {
				t.Fatalf("credential id = %q", id)
			}
			return bytes.Repeat([]byte{0x21}, syncpublication.NodeCredentialSecretBytes), nil
		},
		Interval: 30 * time.Second,
	})
	if err := syncer.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(client.fetched) != 1 || client.fetched[0] != "https://primary.example:3443|"+applied {
		t.Fatalf("fetched = %v", client.fetched)
	}
	if len(client.acked) != 1 || client.acked[0] != string(service.Current().NodeID)+"|"+applied {
		t.Fatalf("acked = %v", client.acked)
	}
	for _, bearer := range client.bearers {
		parsed, err := syncpublication.ParseNodeCredential(bearer)
		if err != nil || parsed.ID() != credentialID {
			t.Fatalf("bearer is not a canonical node credential for %s: %v", credentialID, err)
		}
		parsed.Destroy()
	}
	if len(store.saved) == 0 || store.saved[len(store.saved)-1].AckedHash != applied {
		t.Fatalf("saved = %+v", store.saved)
	}
}
