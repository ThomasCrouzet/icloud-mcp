package icloud

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestMockService_ConcurrentAccessIsRaceFree: MCP handlers may be invoked
// from concurrent goroutines (GuardedService retries with backoff,
// potentially in parallel with other calls), so MockService must stay
// race-free under concurrent calls. This test only proves something when
// run with `go test -race`; without that flag, a race on unprotected
// ints/pointers is not reliably detected on every run.
func TestMockService_ConcurrentAccessIsRaceFree(t *testing.T) {
	svc := &MockService{
		Calendars: []Calendar{{Path: "/cal/"}},
		Events:    []Event{{UID: "uid-1"}},
	}

	const n = 50
	var wg sync.WaitGroup
	wg.Add(n * 5)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_, _ = svc.ListCalendars(context.Background())
		}()
		go func() {
			defer wg.Done()
			_, _ = svc.SearchEvents(context.Background(), "/cal/", time.Now(), time.Now().Add(time.Hour))
		}()
		go func() {
			defer wg.Done()
			_, _ = svc.CreateEvent(context.Background(), "/cal/", &NewEvent{Title: "x"})
		}()
		go func() {
			defer wg.Done()
			_ = svc.UpdateEvent(context.Background(), "/cal/", "uid-1", &EventUpdate{})
		}()
		go func() {
			defer wg.Done()
			_, _ = svc.DeleteEvent(context.Background(), "/cal/", "uid-1")
		}()
	}
	wg.Wait()

	if svc.ListCallCount != n {
		t.Errorf("ListCallCount = %d, want %d (counter lost under unprotected concurrent access)", svc.ListCallCount, n)
	}
	if svc.SearchCallCount != n {
		t.Errorf("SearchCallCount = %d, want %d", svc.SearchCallCount, n)
	}
	if svc.CreateCallCount != n {
		t.Errorf("CreateCallCount = %d, want %d", svc.CreateCallCount, n)
	}
	if svc.UpdateCallCount != n {
		t.Errorf("UpdateCallCount = %d, want %d", svc.UpdateCallCount, n)
	}
	if svc.DeleteCallCount != n {
		t.Errorf("DeleteCallCount = %d, want %d", svc.DeleteCallCount, n)
	}
}
