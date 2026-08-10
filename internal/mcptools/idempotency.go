package mcptools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"
)

// Process-local mutation idempotency cache. Entries expire so a long-lived
// process cannot grow without bound. Distinct params under the same key are a
// conflict; identical params return the cached success payload. Concurrent
// same-key requests single-flight: waiters observe the first outcome.
const (
	idempotencyTTL         = 15 * time.Minute
	maxIdempotencyEntries  = 1024
	maxIdempotencyPayload  = 256 << 10
	maxIdempotencyKeyBytes = 512
)

type idempotencyEntry struct {
	paramsHash string
	payload    string
	expires    time.Time
	pending    bool
	done       chan struct{}
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

// namespacedIdempotencyKey isolates keys per tool so calendar and contacts
// cannot collide on a caller-chosen string.
func namespacedIdempotencyKey(tool, key string) string {
	if tool == "" || key == "" {
		return ""
	}
	return tool + "\x00" + key
}

// begin claims a key for mutation or returns a prior outcome.
//
//	hit:      payload is the cached success body
//	conflict: same key was used with different params
//	ready:    caller must mutate, then complete or abort
//
// When another goroutine holds the same key+params, begin waits for it and
// re-evaluates (single-flight).
func (s *idempotencyStore) begin(key, paramsHash string) (payload string, conflict, hit, ready bool) {
	return s.beginContext(context.Background(), key, paramsHash)
}

func (s *idempotencyStore) beginContext(ctx context.Context, key, paramsHash string) (payload string, conflict, hit, ready bool) {
	if ctx == nil {
		ctx = context.Background()
	}
	if key == "" || paramsHash == "" || len(key) > maxIdempotencyKeyBytes {
		return "", false, false, false
	}
	for {
		s.mu.Lock()
		s.purgeLocked()
		entry, ok := s.entries[key]
		if ok && !entry.pending {
			if entry.paramsHash != paramsHash {
				s.mu.Unlock()
				return "", true, true, false
			}
			payload = entry.payload
			s.mu.Unlock()
			return payload, false, true, false
		}
		if ok && entry.pending {
			if entry.paramsHash != paramsHash {
				s.mu.Unlock()
				return "", true, true, false
			}
			wait := entry.done
			s.mu.Unlock()
			// Bound waiter so a stuck/panicked holder cannot hang agents forever.
			timer := time.NewTimer(idempotencyTTL)
			select {
			case <-wait:
				timer.Stop()
			case <-ctx.Done():
				timer.Stop()
				return "", false, false, false
			case <-timer.C:
				return "", false, false, false
			}
			continue
		}
		if len(s.entries) >= maxIdempotencyEntries {
			s.purgeLocked()
			if len(s.entries) >= maxIdempotencyEntries {
				s.mu.Unlock()
				return "", false, false, false
			}
		}
		s.entries[key] = idempotencyEntry{
			paramsHash: paramsHash,
			pending:    true,
			done:       make(chan struct{}),
			expires:    s.now().Add(idempotencyTTL),
		}
		s.mu.Unlock()
		return "", false, false, true
	}
}

// complete stores a deliverable success payload and releases waiters.
// Empty or oversized payloads abort the claim instead.
func (s *idempotencyStore) complete(key, paramsHash, payload string) {
	if key == "" || paramsHash == "" || payload == "" || len(payload) > maxIdempotencyPayload {
		s.abort(key, paramsHash)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.entries[key]
	if !ok || !entry.pending || entry.paramsHash != paramsHash {
		return
	}
	done := entry.done
	s.entries[key] = idempotencyEntry{
		paramsHash: paramsHash,
		payload:    payload,
		expires:    s.now().Add(idempotencyTTL),
	}
	if done != nil {
		close(done)
	}
}

// abort releases a pending claim without caching a success.
func (s *idempotencyStore) abort(key, paramsHash string) {
	if key == "" || paramsHash == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.entries[key]
	if !ok || !entry.pending || entry.paramsHash != paramsHash {
		return
	}
	done := entry.done
	delete(s.entries, key)
	if done != nil {
		close(done)
	}
}

// lookup is retained for tests that inspect completed entries only.
func (s *idempotencyStore) lookup(key, paramsHash string) (payload string, conflict, found bool) {
	if key == "" || paramsHash == "" {
		return "", false, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.purgeLocked()
	entry, ok := s.entries[key]
	if !ok || entry.pending {
		return "", false, false
	}
	if entry.paramsHash != paramsHash {
		return "", true, true
	}
	return entry.payload, false, true
}

func (s *idempotencyStore) purgeLocked() {
	now := s.now()
	for key, entry := range s.entries {
		if !entry.expires.After(now) {
			if entry.pending && entry.done != nil {
				// Best-effort release of stale waiters; close only once.
				select {
				case <-entry.done:
				default:
					close(entry.done)
				}
			}
			delete(s.entries, key)
		}
	}
}
