package enrollment

import (
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/zerkc/ProxyCore/apps/api/internal/domain"
	replicationsnapshot "github.com/zerkc/ProxyCore/apps/api/internal/snapshot"
)

const (
	DefaultDraftCacheCapacity = 8
	DefaultDraftCacheTTL      = 15 * time.Minute
	draftSelectorBytes        = 16
)

var ErrInvalidDraft = errors.New("invalid enrollment draft")

// NodeLocalOverlay contains only values that are authoritative on the
// receiving installation. It is deliberately separate from the replicated
// envelope so callers cannot accidentally render or persist replicated
// secrets as part of a preview.
type NodeLocalOverlay struct {
	NodeID  domain.NodeID       `json:"nodeId"`
	Ingress domain.Ingress      `json:"ingress"`
	Role    domain.TopologyRole `json:"role"`
}

// Draft is an in-memory, redaction-safe workflow record. The selector is the
// token's public routing component; the token plaintext is never stored.
type Draft struct {
	DraftID    string
	Selector   string
	PrimaryURL string
	Envelope   *replicationsnapshot.Envelope
	Overlay    NodeLocalOverlay
	// PrimaryCAPEM is the owner-loaded CA used to verify this primary. It is
	// public certificate material, kept only until the draft is confirmed.
	PrimaryCAPEM string
	CreatedAt    time.Time
	ExpiresAt    time.Time
}

type DraftCacheOptions struct {
	Capacity int
	TTL      time.Duration
	Now      func() time.Time
}

// draftCacheEntry keeps the attempt secrets next to, never inside, the Draft
// value so they cannot leak through the by-value Draft copies, JSON encoding
// or formatting. token and recipient are zeroed whenever the entry leaves the
// cache (delete, expiry, eviction, replacement).
type draftCacheEntry struct {
	draft     Draft
	used      uint64
	token     []byte
	recipient BootstrapRecipient
}

func (e *draftCacheEntry) zeroSecrets() {
	if e == nil {
		return
	}
	zeroBytes(e.token)
	e.token = nil
	e.recipient.Destroy()
}

// DraftCache is a bounded selector-keyed LRU with an injectable clock. It is
// process-local by design: a restart requires token re-entry and never
// recovers enrollment plaintext from durable storage.
type DraftCache struct {
	mu       sync.Mutex
	capacity int
	ttl      time.Duration
	now      func() time.Time
	sequence uint64
	entries  map[string]*draftCacheEntry
	byID     map[string]string
}

func NewDraftCache(opts DraftCacheOptions) *DraftCache {
	capacity := opts.Capacity
	if capacity <= 0 {
		capacity = DefaultDraftCacheCapacity
	}
	ttl := opts.TTL
	if ttl <= 0 {
		ttl = DefaultDraftCacheTTL
	}
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &DraftCache{
		capacity: capacity,
		ttl:      ttl,
		now:      now,
		entries:  make(map[string]*draftCacheEntry, capacity),
		byID:     make(map[string]string, capacity),
	}
}

// Put stores a validated envelope without a primary URL. PutWithPrimaryURL
// is used by the HTTP workflow when the URL is needed for later TLS checks.
func (c *DraftCache) Put(selector string, envelope *replicationsnapshot.Envelope, overlay NodeLocalOverlay) (Draft, error) {
	return c.PutWithPrimaryURL(selector, "", envelope, overlay)
}

func (c *DraftCache) PutWithPrimaryURL(selector, primaryURL string, envelope *replicationsnapshot.Envelope, overlay NodeLocalOverlay) (Draft, error) {
	return c.put(selector, primaryURL, envelope, overlay, "")
}

// PutWithPrimaryURLAndToken additionally retains the raw enrollment token and
// a per-draft ephemeral bootstrap recipient key in process memory only, so a
// later confirm can run the grant exchange and a retry re-requests the exact
// same grant. Neither is serialized, logged, or exposed through Draft; both are
// zeroed when the draft is deleted or expires.
func (c *DraftCache) PutWithPrimaryURLAndToken(selector, primaryURL string, envelope *replicationsnapshot.Envelope, overlay NodeLocalOverlay, token string) (Draft, error) {
	if token == "" {
		return Draft{}, ErrInvalidDraft
	}
	return c.put(selector, primaryURL, envelope, overlay, token)
}

func (c *DraftCache) put(selector, primaryURL string, envelope *replicationsnapshot.Envelope, overlay NodeLocalOverlay, token string) (Draft, error) {
	if c == nil || !validDraftSelector(selector) || envelope == nil || !overlay.NodeID.IsValid() {
		return Draft{}, ErrInvalidDraft
	}
	copyEnvelope, err := cloneDraftEnvelope(envelope)
	if err != nil {
		return Draft{}, ErrInvalidDraft
	}
	var entry draftCacheEntry
	if token != "" {
		recipient, err := NewBootstrapRecipient()
		if err != nil {
			return Draft{}, ErrInvalidDraft
		}
		entry.token = []byte(token)
		entry.recipient = recipient
	}
	now := c.now().UTC()
	draft := Draft{
		DraftID:    uuid.NewString(),
		Selector:   selector,
		PrimaryURL: primaryURL,
		Envelope:   copyEnvelope,
		Overlay:    overlay,
		CreatedAt:  now,
		ExpiresAt:  now.Add(c.ttl),
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sequence++
	if previous := c.entries[selector]; previous != nil {
		delete(c.byID, previous.draft.DraftID)
		previous.zeroSecrets()
	}
	entry.draft, entry.used = draft, c.sequence
	c.entries[selector] = &entry
	c.byID[draft.DraftID] = selector
	c.evictExpiredLocked(now)
	for len(c.entries) > c.capacity {
		c.evictLeastRecentlyUsedLocked()
	}
	return cloneDraft(draft), nil
}

func (c *DraftCache) GetBySelector(selector string) (Draft, bool) {
	if c == nil || !validDraftSelector(selector) {
		return Draft{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.getLocked(selector, c.now().UTC())
}

func (c *DraftCache) GetByID(id string) (Draft, bool) {
	if c == nil || id == "" {
		return Draft{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	selector, ok := c.byID[id]
	if !ok {
		return Draft{}, false
	}
	return c.getLocked(selector, c.now().UTC())
}

// AttachPrimaryCA records the validated CA the owner loaded for this draft's
// primary. It reports false when the draft does not exist or has expired.
func (c *DraftCache) AttachPrimaryCA(id, caPEM string) bool {
	if c == nil || id == "" {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	selector, ok := c.byID[id]
	if !ok {
		return false
	}
	entry := c.entries[selector]
	if entry == nil || !c.now().UTC().Before(entry.draft.ExpiresAt) {
		return false
	}
	entry.draft.PrimaryCAPEM = caPEM
	return true
}

func (c *DraftCache) DeleteByID(id string) {
	if c == nil || id == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if selector, ok := c.byID[id]; ok {
		delete(c.byID, id)
		c.entries[selector].zeroSecrets()
		delete(c.entries, selector)
	}
}

// AttemptSecrets returns the retained token and a caller-owned clone of the
// draft's recipient key (the caller must Destroy it). ok is false when the
// draft is unknown, expired, or was stored without a token. The token string
// is a copy made at call time; Go strings cannot be zeroed, so callers must
// keep it short-lived and never log it.
func (c *DraftCache) AttemptSecrets(id string) (token string, recipient BootstrapRecipient, ok bool) {
	if c == nil || id == "" {
		return "", BootstrapRecipient{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	selector, found := c.byID[id]
	if !found {
		return "", BootstrapRecipient{}, false
	}
	if _, live := c.getLocked(selector, c.now().UTC()); !live {
		return "", BootstrapRecipient{}, false
	}
	entry := c.entries[selector]
	if entry == nil || len(entry.token) == 0 || len(entry.recipient.private) == 0 {
		return "", BootstrapRecipient{}, false
	}
	return string(entry.token), BootstrapRecipient{private: append([]byte(nil), entry.recipient.private...)}, true
}

func (c *DraftCache) Len() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.evictExpiredLocked(c.now().UTC())
	return len(c.entries)
}

func (c *DraftCache) getLocked(selector string, now time.Time) (Draft, bool) {
	entry, ok := c.entries[selector]
	if !ok {
		return Draft{}, false
	}
	if !now.Before(entry.draft.ExpiresAt) {
		c.removeLocked(selector, entry.draft.DraftID)
		return Draft{}, false
	}
	c.sequence++
	entry.used = c.sequence
	return cloneDraft(entry.draft), true
}

func (c *DraftCache) evictExpiredLocked(now time.Time) {
	for selector, entry := range c.entries {
		if !now.Before(entry.draft.ExpiresAt) {
			c.removeLocked(selector, entry.draft.DraftID)
		}
	}
}

func (c *DraftCache) evictLeastRecentlyUsedLocked() {
	var selector, id string
	var oldest uint64
	for key, entry := range c.entries {
		if selector == "" || entry.used < oldest {
			selector, id, oldest = key, entry.draft.DraftID, entry.used
		}
	}
	if selector != "" {
		c.removeLocked(selector, id)
	}
}

func (c *DraftCache) removeLocked(selector, id string) {
	c.entries[selector].zeroSecrets()
	delete(c.entries, selector)
	delete(c.byID, id)
}

func validDraftSelector(selector string) bool {
	decoded, err := hex.DecodeString(selector)
	return err == nil && len(decoded) == draftSelectorBytes && hex.EncodeToString(decoded) == selector
}

func cloneDraft(draft Draft) Draft {
	copyDraft := draft
	if draft.Envelope != nil {
		copyDraft.Envelope, _ = cloneDraftEnvelope(draft.Envelope)
	}
	return copyDraft
}

func cloneDraftEnvelope(envelope *replicationsnapshot.Envelope) (*replicationsnapshot.Envelope, error) {
	if envelope == nil {
		return nil, ErrInvalidDraft
	}
	encoded, err := replicationsnapshot.Marshal(*envelope)
	if err != nil {
		return nil, err
	}
	copyEnvelope, err := replicationsnapshot.Unmarshal(encoded)
	if err != nil {
		return nil, err
	}
	return &copyEnvelope, nil
}
