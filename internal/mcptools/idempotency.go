package mcptools

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"
)

// Process-local mutation idempotency cache. Entries expire so a long-lived
// process cannot grow without bound. Distinct params under the same key are a
// conflict; identical params return the cached success payload.
const idempotencyTTL = 15 * time.Minute

type idempotencyEntry struct {
	paramsHash string
	payload    string
	expires    time.Time
}

type idempotencyStore struct {
	mu      sync.Mutex
	entries map[string]idempotencyEntry
	now     func() time.Time
}

func newIdempotencyStore() *idempotencyStore {
	return &idempotencyStore{
		entries: make(map[string]idempotencyEntry),
		now:     time.Now,
	}
}

var defaultIdempotency = newIdempotencyStore()

func hashIdempotencyParams(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// lookupIdempotency returns (payload, conflict, found).
// conflict means the key exists with a different params hash.
func (s *idempotencyStore) lookup(key, paramsHash string) (payload string, conflict, found bool) {
	if key == "" || paramsHash == "" {
		return "", false, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.purgeLocked()
	entry, ok := s.entries[key]
	if !ok {
		return "", false, false
	}
	if entry.paramsHash != paramsHash {
		return "", true, true
	}
	return entry.payload, false, true
}

func (s *idempotencyStore) store(key, paramsHash, payload string) {
	if key == "" || paramsHash == "" || payload == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.purgeLocked()
	s.entries[key] = idempotencyEntry{
		paramsHash: paramsHash,
		payload:    payload,
		expires:    s.now().Add(idempotencyTTL),
	}
}

func (s *idempotencyStore) purgeLocked() {
	now := s.now()
	for key, entry := range s.entries {
		if !entry.expires.After(now) {
			delete(s.entries, key)
		}
	}
}
