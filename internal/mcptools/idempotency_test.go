package mcptools

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

func TestIdempotencyStoreBeginCompleteConflict(t *testing.T) {
	s := newIdempotencyStore()
	const key = "k1"
	h1, err := hashIdempotencyParams(map[string]string{"a": "1"})
	if err != nil {
		t.Fatal(err)
	}
	h2, err := hashIdempotencyParams(map[string]string{"a": "2"})
	if err != nil {
		t.Fatal(err)
	}

	payload, conflict, hit, ready := s.begin(key, h1)
	if payload != (idempotencyOutcome{}) || conflict || hit || !ready {
		t.Fatalf("first begin = (%+v,%v,%v,%v)", payload, conflict, hit, ready)
	}
	s.complete(key, h1, mcp.NewToolResultText(`{"ok":true}`), false)

	payload, conflict, hit, ready = s.begin(key, h1)
	if !hit || conflict || ready || payload.payload != `{"ok":true}` || payload.isError {
		t.Fatalf("same params = (%+v,%v,%v,%v)", payload, conflict, hit, ready)
	}

	payload, conflict, hit, ready = s.begin(key, h2)
	if !conflict || !hit || ready || payload != (idempotencyOutcome{}) {
		t.Fatalf("different params = (%+v,%v,%v,%v)", payload, conflict, hit, ready)
	}
}

func TestIdempotencyStoreIgnoresEmpty(t *testing.T) {
	s := newIdempotencyStore()
	s.complete("", "h", mcp.NewToolResultText("p"), false)
	s.complete("k", "", mcp.NewToolResultText("p"), false)
	s.complete("k", "h", mcp.NewToolResultText(""), false)
	if _, _, found := s.lookup("k", "h"); found {
		t.Fatal("empty inputs must not store")
	}
	if _, _, found := s.lookup("", "h"); found {
		t.Fatal("empty key lookup must miss")
	}
}

func TestIdempotencyStoreExpires(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	s := newIdempotencyStore()
	s.now = func() time.Time { return now }

	h, err := hashIdempotencyParams("body")
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, ready := s.begin("exp", h)
	if !ready {
		t.Fatal("expected ready")
	}
	s.complete("exp", h, mcp.NewToolResultText("cached"), false)
	if _, _, found := s.lookup("exp", h); !found {
		t.Fatal("expected live entry")
	}

	s.now = func() time.Time { return now.Add(idempotencyTTL + time.Second) }
	if payload, conflict, found := s.lookup("exp", h); found || conflict || payload != "" {
		t.Fatalf("expired lookup = (%q, %v, %v)", payload, conflict, found)
	}
}

func TestIdempotencyHashStable(t *testing.T) {
	a, err := hashIdempotencyParams(struct {
		X string `json:"x"`
		N int    `json:"n"`
	}{X: "v", N: 2})
	if err != nil {
		t.Fatal(err)
	}
	b, err := hashIdempotencyParams(struct {
		X string `json:"x"`
		N int    `json:"n"`
	}{X: "v", N: 2})
	if err != nil {
		t.Fatal(err)
	}
	if a != b || len(a) != 64 {
		t.Fatalf("hash stability failed: %q vs %q", a, b)
	}
}

func TestIdempotencyStoreSingleFlight(t *testing.T) {
	s := newIdempotencyStore()
	h, err := hashIdempotencyParams("concurrent")
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, ready := s.begin("shared", h)
	if !ready {
		t.Fatal("leader should be ready")
	}

	var wg sync.WaitGroup
	results := make(chan string, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			payload, conflict, hit, ready := s.begin("shared", h)
			if conflict || ready {
				t.Errorf("waiter unexpected conflict=%v ready=%v", conflict, ready)
				return
			}
			if !hit || payload.payload != "payload" || payload.isError {
				t.Errorf("waiter hit=%v payload=%+v", hit, payload)
				return
			}
			results <- payload.payload
		}()
	}
	time.Sleep(20 * time.Millisecond)
	s.complete("shared", h, mcp.NewToolResultText("payload"), false)
	wg.Wait()
	close(results)
	count := 0
	for range results {
		count++
	}
	if count != 8 {
		t.Fatalf("waiters = %d, want 8", count)
	}
}

func TestIdempotencyStoreAbort(t *testing.T) {
	s := newIdempotencyStore()
	h, _ := hashIdempotencyParams("x")
	_, _, _, ready := s.begin("a", h)
	if !ready {
		t.Fatal("expected ready")
	}
	s.abort("a", h)
	_, _, _, ready = s.begin("a", h)
	if !ready {
		t.Fatal("after abort should be ready again")
	}
	s.abort("a", h)
}

func TestIdempotencyStoreWaitHonorsContext(t *testing.T) {
	s := newIdempotencyStore()
	h, _ := hashIdempotencyParams("x")
	_, _, _, ready := s.begin("shared", h)
	if !ready {
		t.Fatal("leader should be ready")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	payload, conflict, hit, ready := s.beginContext(ctx, "shared", h)
	if payload != (idempotencyOutcome{}) || conflict || hit || ready {
		t.Fatalf("canceled begin = (%+v,%v,%v,%v)", payload, conflict, hit, ready)
	}
	if time.Since(started) > time.Second {
		t.Fatal("canceled idempotency wait did not return promptly")
	}
	s.abort("shared", h)
}

func TestNamespacedIdempotencyKey(t *testing.T) {
	if namespacedIdempotencyKey("update_event", "k") != "update_event\x00k" {
		t.Fatal("namespace mismatch")
	}
	if namespacedIdempotencyKey("", "k") != "" || namespacedIdempotencyKey("t", "") != "" {
		t.Fatal("empty parts must yield empty key")
	}
}
