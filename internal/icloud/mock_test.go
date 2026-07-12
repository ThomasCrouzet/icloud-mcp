package icloud

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestMockService_ConcurrentAccessIsRaceFree, FIX-7. Les handlers MCP
// peuvent être invoqués depuis des goroutines concurrentes (GuardedService
// fait des retries avec backoff, potentiellement en parallèle d'autres
// appels) : MockService doit rester race-free sous appel concurrent. Ce
// test ne prouve quelque chose que lancé avec `go test -race`, sans ce
// flag, une race sur des int/pointeurs non protégés n'est pas fiablement
// détectée à chaque run.
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
		t.Errorf("ListCallCount = %d, want %d (compteur perdu sous accès concurrent non protégé)", svc.ListCallCount, n)
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
