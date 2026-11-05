package mcptools

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
)

type trackingCalendarService struct {
	*icloud.MockService
	delay  time.Duration
	active atomic.Int32
	max    atomic.Int32
}

func (s *trackingCalendarService) SearchEvents(ctx context.Context, path string, start, end time.Time, opts *icloud.SearchOptions) (icloud.SearchResult, error) {
	active := s.active.Add(1)
	for {
		max := s.max.Load()
		if active <= max || s.max.CompareAndSwap(max, active) {
			break
		}
	}
	defer s.active.Add(-1)
	select {
	case <-time.After(s.delay):
	case <-ctx.Done():
		return icloud.SearchResult{}, ctx.Err()
	}
	return s.MockService.SearchEvents(ctx, path, start, end, opts)
}

func trackingDeps(service icloud.Service) Deps {
	deps := testDeps(&icloud.MockService{})
	deps.Service = service
	return deps
}

func TestSearchEventsUsesBoundedParallelCalendarReads(t *testing.T) {
	start := time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)
	paths := []string{"/cal/a/", "/cal/b/", "/cal/c/", "/cal/d/", "/cal/e/", "/cal/f/"}
	byPath := make(map[string][]icloud.Event, len(paths))
	for index, path := range paths {
		title := string(rune('A' + index))
		byPath[path] = []icloud.Event{{UID: title, Title: title, StartTime: start, EndTime: start.Add(time.Hour)}}
	}
	service := &trackingCalendarService{
		MockService: &icloud.MockService{Calendars: func() []icloud.Calendar {
			calendars := make([]icloud.Calendar, 0, len(paths))
			for _, path := range paths {
				calendars = append(calendars, icloud.Calendar{Path: path})
			}
			return calendars
		}(), EventsByPath: byPath},
		delay: 40 * time.Millisecond,
	}
	result, err := searchEventsHandler(trackingDeps(service))(context.Background(), newReq(map[string]any{
		"start": "2026-07-01T00:00:00Z", "end": "2026-07-08T00:00:00Z",
	}))
	if err != nil || result.IsError {
		t.Fatalf("search result error: %v %s", err, resultText(t, result))
	}
	var payload searchEventsResponse
	decodeResult(t, result, &payload)
	if len(payload.Events) != len(paths) {
		t.Fatalf("events = %d, want %d", len(payload.Events), len(paths))
	}
	for index, event := range payload.Events {
		want := string(rune('A' + index))
		if event.Title != want {
			t.Fatalf("event order[%d] = %q, want %q", index, event.Title, want)
		}
	}
	if max := service.max.Load(); max < 2 || max > calendarReadConcurrency {
		t.Fatalf("maximum concurrent reads = %d, want 2..%d", max, calendarReadConcurrency)
	}
}

func TestFindFreeSlotsUsesBoundedParallelCalendarReads(t *testing.T) {
	start := time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)
	paths := []string{"/cal/a/", "/cal/b/", "/cal/c/", "/cal/d/"}
	byPath := make(map[string][]icloud.Event, len(paths))
	for _, path := range paths {
		byPath[path] = nil
	}
	service := &trackingCalendarService{
		MockService: &icloud.MockService{EventsByPath: byPath},
		delay:       40 * time.Millisecond,
	}
	result, err := findFreeSlotsHandler(trackingDeps(service))(context.Background(), freeSlotsTestRequest(start, start.Add(2*time.Hour), map[string]any{
		"calendars": "/cal/a/,/cal/b/,/cal/c/,/cal/d/", "duration_minutes": 30,
	}))
	if err != nil || result.IsError {
		t.Fatalf("free slots error: %v %s", err, resultText(t, result))
	}
	if max := service.max.Load(); max < 2 || max > calendarReadConcurrency {
		t.Fatalf("maximum concurrent reads = %d, want 2..%d", max, calendarReadConcurrency)
	}
}
