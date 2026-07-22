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

	searchCalls      int
	searchFailN      int
	searchClassified bool // when true, failures are typed *Error (not retried)

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

func (s *countingService) SearchEvents(ctx context.Context, calendarPath string, start, end time.Time) (SearchResult, error) {
	s.mu.Lock()
	s.searchCalls++
	failN := s.searchFailN
	calls := s.searchCalls
	s.mu.Unlock()
	if calls <= failN {
		if s.searchClassified {
			return SearchResult{}, NewError(CodeRateLimited, 429, "rate limited", nil)
		}
		return SearchResult{}, fmt.Errorf("simulated search failure %d", calls)
	}
	return SearchResult{Events: []Event{{UID: "e1", Title: "t"}}}, nil
}

func (s *countingService) GetEvent(ctx context.Context, calendarPath, uid string) (*EventDetail, error) {
	return &EventDetail{Event: Event{UID: uid}}, nil
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

func (s *countingService) DeleteEvent(ctx context.Context, calendarPath, uid string, opts *DeleteOptions) (DeleteResult, error) {
	s.mu.Lock()
	s.deleteCalls++
	calls := s.deleteCalls
	s.mu.Unlock()
	if calls <= s.deleteFailN {
		return DeleteResult{}, fmt.Errorf("simulated failure %d", calls)
	}
	return DeleteResult{Title: "deleted-title", UID: uid}, nil
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

	res, err := g.DeleteEvent(context.Background(), "/cal/", "uid", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Title != "deleted-title" {
		t.Errorf("title = %q, want %q", res.Title, "deleted-title")
	}
	if inner.deleteCalls != 2 {
		t.Errorf("deleteCalls = %d, want 2 (1 failure + 1 successful retry; DeleteEvent is idempotent)", inner.deleteCalls)
	}
}

// TestGuardedService_RateLimitStatus_ReportsConfiguredBudgets: the health
// endpoint surfaces the live rate-limiter state (no secrets). The configured
// budgets are 60 reads/min and 20 writes/min with bursts 10 and 3.
func TestGuardedService_RateLimitStatus_ReportsConfiguredBudgets(t *testing.T) {
	g := NewGuardedService(&countingService{}, 0, time.Millisecond)

	st := g.RateLimitStatus()

	// 60 reads/min = 1 token/sec; burst 10. 20 writes/min ~= 0.333/sec; burst 3.
	if st.Read.Burst != 10 {
		t.Errorf("Read.Burst = %d, want 10", st.Read.Burst)
	}
	if st.Write.Burst != 3 {
		t.Errorf("Write.Burst = %d, want 3", st.Write.Burst)
	}
	const readPerSec = 1.0
	const writePerSec = 20.0 / 60.0
	if !nearly(st.Read.Limit, readPerSec, 1e-6) {
		t.Errorf("Read.Limit = %v, want %v", st.Read.Limit, readPerSec)
	}
	if !nearly(st.Write.Limit, writePerSec, 1e-6) {
		t.Errorf("Write.Limit = %v, want %v", st.Write.Limit, writePerSec)
	}
	// A freshly-built limiter has its full burst of tokens available.
	if st.Read.Tokens > 10 || st.Read.Tokens <= 0 {
		t.Errorf("Read.Tokens = %v, want within (0, 10] on a fresh limiter", st.Read.Tokens)
	}
	if st.Write.Tokens > 3 || st.Write.Tokens <= 0 {
		t.Errorf("Write.Tokens = %v, want within (0, 3] on a fresh limiter", st.Write.Tokens)
	}
}

// TestGuardedService_RetrySkipsClassifiedErrors: a typed *icloud.Error
// (already retried at the HTTP layer, or terminal like auth/not-found) must
// NOT be retried again by GuardedService: it should be returned immediately,
// without consuming the retry budget.
func TestGuardedService_RetrySkipsClassifiedErrors(t *testing.T) {
	classified := NewError(CodeServerUnavailable, 503, "shard down", nil)
	// A service that always returns a typed *icloud.Error (already retried by
	// the HTTP layer, or terminal): GuardedService must NOT retry it.
	inner := &classifiedService{err: classified}
	g := NewGuardedService(inner, 5, time.Millisecond)

	_, err := g.ListCalendars(context.Background())
	if err == nil {
		t.Fatal("expected the classified error to be returned, not retried")
	}
	if AsICloudError(err) == nil {
		t.Errorf("expected a typed *icloud.Error, got %v", err)
	}
	if inner.calls != 1 {
		t.Errorf("calls = %d, want 1 (classified errors must not be retried)", inner.calls)
	}
}

type classifiedService struct {
	err   error
	calls int
}

func (s *classifiedService) ListCalendars(ctx context.Context) ([]Calendar, error) {
	s.calls++
	return nil, s.err
}
func (s *classifiedService) SearchEvents(ctx context.Context, calendarPath string, start, end time.Time) (SearchResult, error) {
	return SearchResult{}, nil
}
func (s *classifiedService) GetEvent(ctx context.Context, calendarPath, uid string) (*EventDetail, error) {
	return nil, nil
}
func (s *classifiedService) CreateEvent(ctx context.Context, calendarPath string, ev *NewEvent) (string, error) {
	return "", nil
}
func (s *classifiedService) UpdateEvent(ctx context.Context, calendarPath, uid string, up *EventUpdate) error {
	return nil
}
func (s *classifiedService) DeleteEvent(ctx context.Context, calendarPath, uid string, opts *DeleteOptions) (DeleteResult, error) {
	return DeleteResult{}, nil
}

func nearly(a, b, eps float64) bool {
	if a > b {
		return a-b < eps
	}
	return b-a < eps
}

// TestGuardedService_SearchEvents_RetriesTransientThenSucceeds drives the
// real SearchEvents decorator path (was 0% coverage): transient errors are
// retried; the final events are returned.
func TestGuardedService_SearchEvents_RetriesTransientThenSucceeds(t *testing.T) {
	inner := &countingService{searchFailN: 2}
	g := NewGuardedService(inner, 2, time.Millisecond)

	res, err := g.SearchEvents(context.Background(), "/cal/", time.Now(), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("SearchEvents: %v", err)
	}
	if len(res.Events) != 1 || res.Events[0].UID != "e1" {
		t.Fatalf("result = %+v, want one event e1", res)
	}
	if inner.searchCalls != 3 {
		t.Errorf("searchCalls = %d, want 3 (2 failures + success)", inner.searchCalls)
	}
}

// TestGuardedService_SearchEvents_ClassifiedErrorNotRetried: a typed
// *icloud.Error from SearchEvents must not consume the GuardedService retry
// budget (HTTP layer already retried 429/5xx).
func TestGuardedService_SearchEvents_ClassifiedErrorNotRetried(t *testing.T) {
	inner := &countingService{searchFailN: 10, searchClassified: true}
	g := NewGuardedService(inner, 5, time.Millisecond)

	_, err := g.SearchEvents(context.Background(), "/cal/", time.Now(), time.Now().Add(time.Hour))
	if err == nil {
		t.Fatal("expected classified error")
	}
	if AsICloudError(err) == nil {
		t.Errorf("expected *icloud.Error, got %v", err)
	}
	if inner.searchCalls != 1 {
		t.Errorf("searchCalls = %d, want 1 (no GuardedService retry)", inner.searchCalls)
	}
}

// TestGuardedService_SearchEvents_RateLimitBlocksAfterBurst exercises the
// read limiter on the SearchEvents path specifically.
func TestGuardedService_SearchEvents_RateLimitBlocksAfterBurst(t *testing.T) {
	inner := &countingService{}
	g := NewGuardedService(inner, 0, time.Millisecond)
	start := time.Now()
	end := start.Add(time.Hour)
	for i := 0; i < 10; i++ {
		if _, err := g.SearchEvents(context.Background(), "/cal/", start, end); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	if _, err := g.SearchEvents(ctx, "/cal/", start, end); err == nil {
		t.Fatal("expected rate limit error after read burst exhausted")
	}
}
