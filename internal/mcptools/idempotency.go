package mcptools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

// The process-local cache retains known outcomes for 15 minutes after completion.
// Pending and uncertain outcomes never expire. The entry limit bounds memory.
// Callers must reconcile uncertain outcomes before they select a new key.
const (
	idempotencyTTL         = 15 * time.Minute
	maxIdempotencyEntries  = 1024
	maxIdempotencyPayload  = 256 << 10
	maxIdempotencyKeyBytes = 512
)

type idempotencyOutcome struct {
	payload string
	isError bool
}

// result keeps IsError and applies the domain's redaction and result size guards.
func (o idempotencyOutcome) result(write func(string) *mcp.CallToolResult) *mcp.CallToolResult {
	result := write(o.payload)
	result.IsError = result.IsError || o.isError
	return result
}

type idempotencyEntry struct {
	paramsHash string
	outcome    idempotencyOutcome
	expires    time.Time
	pending    bool
	done       chan struct{}
}

type idempotencyStore struct {
	mu      sync.Mutex
	entries map[string]*idempotencyEntry
	now     func() time.Time
}

func newIdempotencyStore() *idempotencyStore {
	return &idempotencyStore{
		entries: make(map[string]*idempotencyEntry),
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
//	hit:      outcome contains the cached text and error status
//	conflict: same key was used with different params
//	ready:    caller must complete the claim after the service call
//
// Concurrent same-key requests wait for the original owner's outcome.
func (s *idempotencyStore) begin(key, paramsHash string) (outcome idempotencyOutcome, conflict, hit, ready bool) {
	return s.beginContext(context.Background(), key, paramsHash)
}

func (s *idempotencyStore) beginContext(ctx context.Context, key, paramsHash string) (outcome idempotencyOutcome, conflict, hit, ready bool) {
	if ctx == nil {
		ctx = context.Background()
	}
	if key == "" || paramsHash == "" || len(key) > maxIdempotencyKeyBytes {
		return idempotencyOutcome{}, false, false, false
	}
	for {
		s.mu.Lock()
		if ctx.Err() != nil {
			s.mu.Unlock()
			return idempotencyOutcome{}, false, false, false
		}
		s.purgeLocked()
		entry, ok := s.entries[key]
		if ok && !entry.pending {
			if entry.paramsHash != paramsHash {
				s.mu.Unlock()
				return idempotencyOutcome{}, true, true, false
			}
			outcome = entry.outcome
			s.mu.Unlock()
			return outcome, false, true, false
		}
		if ok && entry.pending {
			if entry.paramsHash != paramsHash {
				s.mu.Unlock()
				return idempotencyOutcome{}, true, true, false
			}
			wait := entry.done
			s.mu.Unlock()
			// A timeout releases only this waiter, never the owner's claim.
			timer := time.NewTimer(idempotencyTTL)
			select {
			case <-wait:
				timer.Stop()
				s.mu.Lock()
				outcome, completed := entry.outcome, !entry.pending
				s.mu.Unlock()
				if ctx.Err() != nil {
					return idempotencyOutcome{}, false, false, false
				}
				// Keep the owner's result even if another request purged its entry.
				if completed {
					return outcome, false, true, false
				}
			case <-ctx.Done():
				timer.Stop()
				return idempotencyOutcome{}, false, false, false
			case <-timer.C:
				return idempotencyOutcome{}, false, false, false
			}
			continue
		}
		if len(s.entries) >= maxIdempotencyEntries {
			s.mu.Unlock()
			return idempotencyOutcome{}, false, false, false
		}
		s.entries[key] = &idempotencyEntry{
			paramsHash: paramsHash,
			pending:    true,
			done:       make(chan struct{}),
		}
		s.mu.Unlock()
		return idempotencyOutcome{}, false, false, true
	}
}

// complete stores the guarded result and releases waiters. Uncertain outcomes
// stay until process exit. Missing or invalid results also retain the claim.
func (s *idempotencyStore) complete(key, paramsHash string, result *mcp.CallToolResult, retain bool) {
	if key == "" || paramsHash == "" {
		return
	}
	outcome := idempotencyOutcome{
		payload: `{"code":"outcome_unknown","message":"The mutation outcome could not be recorded.","reconciliation":"Read the resource before using a new idempotency key."}`,
		isError: true,
	}
	valid := false
	if result != nil && len(result.Content) == 1 {
		if text, ok := mcp.AsTextContent(result.Content[0]); ok && text.Text != "" && len(text.Text) <= maxIdempotencyPayload {
			outcome = idempotencyOutcome{payload: text.Text, isError: result.IsError}
			valid = true
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.entries[key]
	if !ok || !entry.pending || entry.paramsHash != paramsHash {
		return
	}
	entry.outcome = outcome
	entry.pending = false
	if valid && !retain {
		entry.expires = s.now().Add(idempotencyTTL)
	}
	if entry.done != nil {
		close(entry.done)
	}
}

// abort releases a claim only before a service call can dispatch a mutation.
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
	return entry.outcome.payload, false, true
}

func (s *idempotencyStore) purgeLocked() {
	now := s.now()
	for key, entry := range s.entries {
		if !entry.pending && !entry.expires.IsZero() && !entry.expires.After(now) {
			delete(s.entries, key)
		}
	}
}
