package main

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

// These are artificial handler phase markers, not observations of SMTP DATA.
// The protocol test separately counts actual complete SMTP DATA transactions.
func TestReplyTimeoutRetainsFullHandlerCapacity(t *testing.T) {
	slots := make(chan struct{}, maxInFlightHandlers)
	started := make(chan struct{}, maxInFlightHandlers)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	var calls atomic.Int32
	h := timeoutMiddlewareWithLimit(toolTimeout, toolTimeoutGrace, slots)(func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		calls.Add(1)
		started <- struct{}{}
		<-release
		return mcp.NewToolResultText("completed"), nil
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	results := make(chan timeoutCallOutcome, maxInFlightHandlers)
	req := timeoutReplyRequest("Synthetic capacity probe")
	for range maxInFlightHandlers {
		go func() {
			res, err := h(ctx, req)
			results <- timeoutCallOutcome{res, err}
		}()
	}
	for range maxInFlightHandlers {
		timeoutAwait(t, started)
	}
	cancel()
	for range maxInFlightHandlers {
		assertMailTimeoutHint(t, timeoutCallPayload(t, timeoutAwait(t, results)))
	}
	if len(slots) != maxInFlightHandlers {
		t.Fatalf("timed-out active handlers hold %d slots, want %d", len(slots), maxInFlightHandlers)
	}
	for range maxInFlightHandlers {
		waiting, stop := context.WithTimeout(t.Context(), 5*time.Millisecond)
		res, err := h(waiting, req)
		stop()
		assertMailTimeoutHint(t, timeoutCallPayload(t, timeoutCallOutcome{res, err}))
	}
	if calls.Load() != maxInFlightHandlers || len(slots) != maxInFlightHandlers {
		t.Fatalf("overload escaped budget: handlers=%d held_slots=%d", calls.Load(), len(slots))
	}
	unblock()
	timeoutSlotsDrained(t, slots)
	res, err := h(t.Context(), req)
	if err != nil || res == nil || res.IsError {
		t.Fatalf("capacity did not recover: result=%v err=%v", res, err)
	}
	timeoutAwait(t, started)
	timeoutSlotsDrained(t, slots)
	t.Logf("capacity=%d peak_handlers=%d rejected_waiters=%d retained_until_completion=true", cap(slots), maxInFlightHandlers, maxInFlightHandlers)
}
