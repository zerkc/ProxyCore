package enrollment

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	replicationsnapshot "github.com/zerkc/ProxyCore/apps/api/internal/snapshot"
)

func TestDraftCacheStoresValidatedEnvelopeBySelectorWithTTL(t *testing.T) {
	now := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	cache := NewDraftCache(DraftCacheOptions{
		Capacity: 2,
		TTL:      time.Minute,
		Now:      func() time.Time { return now },
	})
	envelope := draftCacheTestEnvelope(t)
	draft, err := cache.Put("0123456789abcdef0123456789abcdef", envelope, NodeLocalOverlay{
		NodeID:  domain.NewNodeID(),
		Ingress: domain.Ingress{IPv4: "192.0.2.10"},
	})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if draft.DraftID == "" || !draft.ExpiresAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("draft metadata = %+v", draft)
	}
	got, ok := cache.GetByID(draft.DraftID)
	if !ok || got.Envelope == nil || got.Envelope.ContentHash != envelope.ContentHash {
		t.Fatalf("GetByID = %+v, ok=%t", got, ok)
	}
	got.Envelope.ContentHash = "mutated"
	again, ok := cache.GetByID(draft.DraftID)
	if !ok || again.Envelope.ContentHash != envelope.ContentHash {
		t.Fatalf("cache returned mutable envelope = %+v, ok=%t", again, ok)
	}

	now = now.Add(time.Minute)
	if _, ok := cache.GetByID(draft.DraftID); ok {
		t.Fatal("expired draft remained available")
	}
}

func TestDraftCacheRejectsNonSelectorKeys(t *testing.T) {
	cache := NewDraftCache(DraftCacheOptions{})
	for _, key := range []string{"plaintext-token", "sha256-v1:deadbeef", "0123456789abcdef0123456789ABCDEf"} {
		if _, err := cache.Put(key, draftCacheTestEnvelope(t), NodeLocalOverlay{NodeID: domain.NewNodeID()}); err == nil {
			t.Fatalf("accepted non-canonical selector %q", key)
		}
	}
}

func TestDraftCacheEvictsLeastRecentlyUsedEntry(t *testing.T) {
	now := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	cache := NewDraftCache(DraftCacheOptions{Capacity: 2, TTL: time.Hour, Now: func() time.Time { return now }})
	first := mustPutDraft(t, cache, "0123456789abcdef0123456789abcdef")
	second := mustPutDraft(t, cache, "1123456789abcdef0123456789abcdef")
	if _, ok := cache.GetByID(first.DraftID); !ok {
		t.Fatal("first draft missing before eviction")
	}
	third := mustPutDraft(t, cache, "2123456789abcdef0123456789abcdef")
	if _, ok := cache.GetByID(first.DraftID); !ok {
		t.Fatal("recently used draft was evicted")
	}
	if _, ok := cache.GetByID(second.DraftID); ok {
		t.Fatal("least recently used draft remained after eviction")
	}
	if _, ok := cache.GetByID(third.DraftID); !ok {
		t.Fatal("new draft missing after eviction")
	}
}

func mustPutDraft(t *testing.T, cache *DraftCache, selector string) Draft {
	t.Helper()
	draft, err := cache.Put(selector, draftCacheTestEnvelope(t), NodeLocalOverlay{NodeID: domain.NewNodeID()})
	if err != nil {
		t.Fatal(err)
	}
	return draft
}

func draftCacheTestEnvelope(t *testing.T) *replicationsnapshot.Envelope {
	t.Helper()
	envelope := &replicationsnapshot.Envelope{
		Transient: replicationsnapshot.TransientFields{
			SnapshotVersion:      domain.SnapshotVersionV1,
			ReplicationVersion:   domain.ReplicationVersionV1,
			SourcePrimaryID:      uuid.New(),
			LeadershipGeneration: 4,
			CapturedAt:           time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC),
		},
		NodeLocal: replicationsnapshot.NodeLocalFields{
			NodeID: domain.NewNodeID(),
			Role:   domain.TopologyRolePrimary,
		},
		Replicated: replicationsnapshot.ReplicatedFields{Configuration: map[string]any{"settings": map[string]any{}}},
	}
	hash, err := envelope.ExpectedHash()
	if err != nil {
		t.Fatal(err)
	}
	envelope.ContentHash = hash
	return envelope
}
