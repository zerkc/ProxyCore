package acme

import (
	"sync"
	"time"
)

// Http01Store holds pending HTTP-01 key authorizations keyed by token.
type Http01Store interface {
	Put(token, keyAuthorization string, expiresAt *time.Time)
	Get(token string) (string, bool)
	Remove(token string)
}

type http01Entry struct {
	keyAuthorization string
	expiresAt        *time.Time
}

// InMemoryHttp01Store is a concurrency-safe in-memory HTTP-01 store.
type InMemoryHttp01Store struct {
	mu     sync.Mutex
	values map[string]http01Entry
}

// NewInMemoryHttp01Store builds an empty store.
func NewInMemoryHttp01Store() *InMemoryHttp01Store {
	return &InMemoryHttp01Store{values: map[string]http01Entry{}}
}

// Put stores a key authorization for a token.
func (s *InMemoryHttp01Store) Put(token, keyAuthorization string, expiresAt *time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values[token] = http01Entry{keyAuthorization: keyAuthorization, expiresAt: expiresAt}
}

// Get returns the key authorization for a token, expiring stale entries.
func (s *InMemoryHttp01Store) Get(token string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.values[token]
	if !ok {
		return "", false
	}
	if entry.expiresAt != nil && !entry.expiresAt.After(time.Now()) {
		delete(s.values, token)
		return "", false
	}
	return entry.keyAuthorization, true
}

// Remove deletes a token.
func (s *InMemoryHttp01Store) Remove(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.values, token)
}

// processGlobalStore mirrors the Node process-global HTTP-01 store so the ACME
// challenge route and certificate issuance share the same pending tokens.
var (
	processGlobalOnce  sync.Once
	processGlobalStore *InMemoryHttp01Store
)

// GlobalHttp01Store returns the process-wide HTTP-01 challenge store.
func GlobalHttp01Store() *InMemoryHttp01Store {
	processGlobalOnce.Do(func() {
		processGlobalStore = NewInMemoryHttp01Store()
	})
	return processGlobalStore
}
