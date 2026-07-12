package icloud

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// countingService is an instrumented Service for GuardedService tests:
// counts calls and can simulate N failures before succeeding.
type countingService struct {
	mu sync.Mutex

	listCalls int
	listFailN int

	searchCalls int

	createCalls int
	createErr   error

	updateCalls int
	updateErr   error

	deleteCalls int
	deleteFailN int
}

var _ Service = (*countingService)(nil)

func (s *countingService) ListCalendars(ctx context.Context) ([]Calendar, error) {
	s.mu.Lock()
	s.listCalls++
	calls := s.listCalls
	s.mu.Unlock()
	if calls <= s.listFailN {
		return nil, fmt.Errorf("simulated failure %d", calls)
	}
	return []Calendar{{Path: "/cal/"}}, nil
}

func (s *countingService) SearchEvents(ctx context.Context, calendarPath string, start, end time.Time) ([]Event, error) {
	s.mu.Lock()
	s.searchCalls++
	s.mu.Unlock()
	return nil, nil
}

func (s *countingService) CreateEvent(ctx context.Context, calendarPath string, ev *NewEvent) (string, error) {
	s.mu.Lock()
	s.createCalls++
	s.mu.Unlock()
	if s.createErr != nil {
		return "", s.createErr
	}
	return "uid", nil
}

func (s *countingService) UpdateEvent(ctx context.Context, calendarPath, uid string, up *EventUpdate) error {
	s.mu.Lock()
	s.updateCalls++
	s.mu.Unlock()
	return s.updateErr
}

func (s *countingService) DeleteEvent(ctx context.Context, calendarPath, uid string) (string, error) {
	s.mu.Lock()
	s.deleteCalls++
	calls := s.deleteCalls
	s.mu.Unlock()
	if calls <= s.deleteFailN {
		return "", fmt.Errorf("simulated failure %d", calls)
	}
	return "deleted-title", nil
}

func TestGuardedService_ReadRateLimitBlocksAfterBurst(t *testing.T) {
	inner := &countingService{}
	g := NewGuardedService(inner, 0, time.Millisecond)

	// Exhaust the read burst (10).
	for i := 0; i < 10; i++ {
		if _, err := g.ListCalendars(context.Background()); err != nil {
			t.Fatalf("call %d: unexpected error: %v", i, err)
		}
	}

	// The 11th call must wait ~1s for a fresh token (60/min); a context
	// with a very short deadline must therefore fail.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	if _, err := g.ListCalendars(ctx); err == nil {
		t.Fatal("expected a rate limit error (read burst exhausted)")
	}
}

func TestGuardedService_WriteBudgetIndependentFromRead(t *testing.T) {
	inner := &countingService{}
	g := NewGuardedService(inner, 0, time.Millisecond)

	for i := 0; i < 10; i++ {
		if _, err := g.ListCalendars(context.Background()); err != nil {
			t.Fatalf("read %d: unexpected error: %v", i, err)
		}
	}

	// The write budget is independent: this must succeed immediately even
	// though the read burst is exhausted.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := g.CreateEvent(ctx, "/cal/", &NewEvent{Title: "x"}); err != nil {
		t.Fatalf("CreateEvent unexpected error: %v", err)
	}
}

func TestGuardedService_RetrySucceedsAfterFailures(t *testing.T) {
	inner := &countingService{listFailN: 2}
	g := NewGuardedService(inner, 2, time.Millisecond)

	if _, err := g.ListCalendars(context.Background()); err != nil {
		t.Fatalf("unexpected error after retries: %v", err)
	}
	if inner.listCalls != 3 {
		t.Errorf("listCalls = %d, want 3 (1 attempt + 2 retries)", inner.listCalls)
	}
}

func TestGuardedService_RetryExhausted(t *testing.T) {
	inner := &countingService{listFailN: 10}
	g := NewGuardedService(inner, 2, time.Millisecond)

	if _, err := g.ListCalendars(context.Background()); err == nil {
		t.Fatal("expected an error after retries were exhausted")
	}
	if inner.listCalls != 3 {
		t.Errorf("listCalls = %d, want 3 (1 attempt + 2 retries, all failing)", inner.listCalls)
	}
}

func TestGuardedService_CreateEventNeverRetried(t *testing.T) {
	inner := &countingService{createErr: fmt.Errorf("failure")}
	g := NewGuardedService(inner, 2, time.Millisecond)

	if _, err := g.CreateEvent(context.Background(), "/cal/", &NewEvent{Title: "x"}); err == nil {
		t.Fatal("expected an error")
	}
	if inner.createCalls != 1 {
		t.Errorf("createCalls = %d, want 1 (CreateEvent is never retried, non-idempotent)", inner.createCalls)
	}
}

func TestGuardedService_UpdateEventNeverRetried(t *testing.T) {
	inner := &countingService{updateErr: fmt.Errorf("failure")}
	g := NewGuardedService(inner, 2, time.Millisecond)

	if err := g.UpdateEvent(context.Background(), "/cal/", "uid", &EventUpdate{}); err == nil {
		t.Fatal("expected an error")
	}
	if inner.updateCalls != 1 {
		t.Errorf("updateCalls = %d, want 1 (UpdateEvent is never retried, non-idempotent)", inner.updateCalls)
	}
}

func TestGuardedService_DeleteEventRetried(t *testing.T) {
	inner := &countingService{deleteFailN: 1}
	g := NewGuardedService(inner, 2, time.Millisecond)

	title, err := g.DeleteEvent(context.Background(), "/cal/", "uid")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if title != "deleted-title" {
		t.Errorf("title = %q, want %q", title, "deleted-title")
	}
	if inner.deleteCalls != 2 {
		t.Errorf("deleteCalls = %d, want 2 (1 failure + 1 successful retry; DeleteEvent is idempotent)", inner.deleteCalls)
	}
}
