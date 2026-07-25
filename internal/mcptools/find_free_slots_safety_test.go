package mcptools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
)

type freeSlotsTestSearch struct {
	path  string
	start time.Time
	end   time.Time
}

type freeSlotsTestService struct {
	icloud.Service

	calendars  []icloud.Calendar
	events     map[string][]icloud.Event
	searchErrs map[string]error
	truncated  map[string]bool
	cancel     context.CancelFunc

	listCalls int
	searches  []freeSlotsTestSearch
}

func (s *freeSlotsTestService) ListCalendars(context.Context) ([]icloud.Calendar, error) {
	s.listCalls++
	return s.calendars, nil
}

func (s *freeSlotsTestService) SearchEvents(_ context.Context, path string, start, end time.Time, _ *icloud.SearchOptions) (icloud.SearchResult, error) {
	s.searches = append(s.searches, freeSlotsTestSearch{path: path, start: start, end: end})
	if s.cancel != nil {
		s.cancel()
	}
	if err := s.searchErrs[path]; err != nil {
		return icloud.SearchResult{}, err
	}
	events := make([]icloud.Event, 0, len(s.events[path]))
	for _, event := range s.events[path] {
		if event.EndTime.After(start) && event.StartTime.Before(end) {
			events = append(events, event)
		}
	}
	return icloud.SearchResult{Events: events, TruncatedByExpansion: s.truncated[path]}, nil
}

func freeSlotsTestDeps(service icloud.Service) Deps {
	deps := testDeps(&icloud.MockService{})
	deps.Service = service
	deps.DefaultLocation = time.UTC
	return deps
}

func freeSlotsTestRequest(start, end time.Time, extra map[string]any) mcp.CallToolRequest {
	args := map[string]any{
		"start":            start.Format(time.RFC3339),
		"end":              end.Format(time.RFC3339),
		"duration_minutes": 30,
	}
	for key, value := range extra {
		args[key] = value
	}
	return mcp.CallToolRequest{Params: mcp.CallToolParams{Arguments: args}}
}

func TestFindFreeSlotsHandlerFailsClosedOnIncompleteSearch(t *testing.T) {
	start := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)

	t.Run("calendar failure", func(t *testing.T) {
		service := &freeSlotsTestService{
			events:     map[string][]icloud.Event{"/cal/good/": nil},
			searchErrs: map[string]error{"/cal/bad/": errors.New("calendar unavailable")},
		}
		result, err := findFreeSlotsHandler(freeSlotsTestDeps(service))(
			context.Background(),
			freeSlotsTestRequest(start, start.Add(time.Hour), map[string]any{
				"calendars": "/cal/good/,/cal/bad/",
			}),
		)
		if err != nil {
			t.Fatalf("protocol error: %v", err)
		}
		if !result.IsError {
			t.Fatalf("expected error result, got %s", resultText(t, result))
		}
		if !strings.Contains(resultText(t, result), "calendar unavailable") {
			t.Fatalf("unexpected error: %s", resultText(t, result))
		}
	})

	t.Run("truncated recurrence expansion", func(t *testing.T) {
		service := &freeSlotsTestService{
			events:    map[string][]icloud.Event{"/cal/home/": nil},
			truncated: map[string]bool{"/cal/home/": true},
		}
		result, err := findFreeSlotsHandler(freeSlotsTestDeps(service))(
			context.Background(),
			freeSlotsTestRequest(start, start.Add(time.Hour), map[string]any{
				"calendar": "/cal/home/",
			}),
		)
		if err != nil {
			t.Fatalf("protocol error: %v", err)
		}
		if !result.IsError || !strings.Contains(resultText(t, result), "truncated") {
			t.Fatalf("expected truncation error, got %s", resultText(t, result))
		}
	})
}

func TestFindFreeSlotsHandlerUsesBufferedQueryAndClipsOutput(t *testing.T) {
	start := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	end := start.Add(2 * time.Hour)
	service := &freeSlotsTestService{events: map[string][]icloud.Event{
		"/cal/home/": {
			{UID: "before", StartTime: start.Add(-20 * time.Minute), EndTime: start.Add(-10 * time.Minute)},
			{UID: "after", StartTime: end.Add(10 * time.Minute), EndTime: end.Add(20 * time.Minute)},
		},
	}}

	result, err := findFreeSlotsHandler(freeSlotsTestDeps(service))(
		context.Background(),
		freeSlotsTestRequest(start, end, map[string]any{
			"calendar":              "/cal/home/",
			"buffer_before_minutes": 30,
			"buffer_after_minutes":  30,
		}),
	)
	if err != nil || result.IsError {
		t.Fatalf("err=%v result=%s", err, resultText(t, result))
	}
	if len(service.searches) != 1 {
		t.Fatalf("searches = %d, want 1", len(service.searches))
	}
	if got, want := service.searches[0].start, start.Add(-30*time.Minute); !got.Equal(want) {
		t.Errorf("query start = %s, want %s", got, want)
	}
	if got, want := service.searches[0].end, end.Add(30*time.Minute); !got.Equal(want) {
		t.Errorf("query end = %s, want %s", got, want)
	}

	var payload freeSlotsResponse
	decodeResult(t, result, &payload)
	if payload.Count != 2 {
		t.Fatalf("slots = %+v, want 2 slots", payload.Slots)
	}
	if payload.Slots[0].Start != start.Add(20*time.Minute).Format(time.RFC3339) {
		t.Errorf("first slot starts at %s", payload.Slots[0].Start)
	}
	for _, slot := range payload.Slots {
		slotStart, parseStartErr := time.Parse(time.RFC3339, slot.Start)
		slotEnd, parseEndErr := time.Parse(time.RFC3339, slot.End)
		if parseStartErr != nil || parseEndErr != nil {
			t.Fatalf("invalid slot %+v: startErr=%v endErr=%v", slot, parseStartErr, parseEndErr)
		}
		if slotStart.Before(start) || slotEnd.After(end) {
			t.Errorf("slot outside original range: %+v", slot)
		}
	}
}

func TestFindFreeSlotsHandlerRejectsUnsafeBufferedRange(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	service := &freeSlotsTestService{}
	result, err := findFreeSlotsHandler(freeSlotsTestDeps(service))(
		context.Background(),
		freeSlotsTestRequest(start, start.Add(366*24*time.Hour), map[string]any{
			"calendar":             "/cal/home/",
			"buffer_after_minutes": 1,
		}),
	)
	if err != nil {
		t.Fatalf("protocol error: %v", err)
	}
	if !result.IsError || !strings.Contains(resultText(t, result), "buffered search range") {
		t.Fatalf("expected buffered range error, got %s", resultText(t, result))
	}
	if len(service.searches) != 0 {
		t.Fatalf("unsafe range performed %d searches", len(service.searches))
	}
}

func TestResolveFreeSlotCalendarPathsBoundsExplicitSelections(t *testing.T) {
	service := &freeSlotsTestService{}
	deps := freeSlotsTestDeps(service)

	paths, err := resolveCalendarPaths(context.Background(), deps, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Arguments: map[string]any{
			"calendars": "/cal/a/, /cal/a/, /cal/b/",
			"calendar":  "/cal/b/",
		}},
	})
	if err != nil {
		t.Fatalf("deduplicating paths: %v", err)
	}
	if got, want := strings.Join(paths, ","), "/cal/a/,/cal/b/"; got != want {
		t.Fatalf("paths = %q, want %q", got, want)
	}

	tooMany := make([]string, 0, maxExplicitFreeSlotCalendars+1)
	for i := 0; i <= maxExplicitFreeSlotCalendars; i++ {
		tooMany = append(tooMany, fmt.Sprintf("/cal/%d/", i))
	}
	_, err = resolveCalendarPaths(context.Background(), deps, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Arguments: map[string]any{"calendars": strings.Join(tooMany, ",")}},
	})
	if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("at most %d", maxExplicitFreeSlotCalendars)) {
		t.Fatalf("expected explicit calendar cap error, got %v", err)
	}
}

func TestResolveFreeSlotCalendarPathsSupportsAllDiscoveredCalendars(t *testing.T) {
	calendars := make([]icloud.Calendar, 0, maxExplicitFreeSlotCalendars+1)
	for i := 0; i <= maxExplicitFreeSlotCalendars; i++ {
		calendars = append(calendars, icloud.Calendar{Path: fmt.Sprintf("/cal/%d/", i)})
	}
	service := &freeSlotsTestService{calendars: calendars}
	paths, err := resolveCalendarPaths(context.Background(), freeSlotsTestDeps(service), mcp.CallToolRequest{})
	if err != nil {
		t.Fatalf("discovering calendars: %v", err)
	}
	if len(paths) != len(calendars) {
		t.Fatalf("discovered paths = %d, want %d", len(paths), len(calendars))
	}
}

func TestResolveFreeSlotCalendarPathsRejectsEmptySelection(t *testing.T) {
	t.Run("explicit", func(t *testing.T) {
		service := &freeSlotsTestService{}
		_, err := resolveCalendarPaths(context.Background(), freeSlotsTestDeps(service), mcp.CallToolRequest{
			Params: mcp.CallToolParams{Arguments: map[string]any{"calendars": " , "}},
		})
		if err == nil || !strings.Contains(err.Error(), "at least one non-empty path") {
			t.Fatalf("expected empty selection error, got %v", err)
		}
		if service.listCalls != 0 {
			t.Fatal("explicit empty selection must not fall back to discovery")
		}
	})

	t.Run("discovered", func(t *testing.T) {
		service := &freeSlotsTestService{}
		_, err := resolveCalendarPaths(context.Background(), freeSlotsTestDeps(service), mcp.CallToolRequest{})
		if err == nil || !strings.Contains(err.Error(), "no calendars are available") {
			t.Fatalf("expected no calendars error, got %v", err)
		}
	})
}

func TestFindFreeSlotsCalendarSelectionHonorsCancellation(t *testing.T) {
	t.Run("during explicit selection", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := resolveCalendarPaths(ctx, freeSlotsTestDeps(&freeSlotsTestService{}), mcp.CallToolRequest{
			Params: mcp.CallToolParams{Arguments: map[string]any{"calendar": "/cal/home/"}},
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
	})

	t.Run("between searches", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		service := &freeSlotsTestService{
			events: map[string][]icloud.Event{
				"/cal/one/": nil,
				"/cal/two/": nil,
			},
			cancel: cancel,
		}
		start := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
		result, err := findFreeSlotsHandler(freeSlotsTestDeps(service))(
			ctx,
			freeSlotsTestRequest(start, start.Add(time.Hour), map[string]any{
				"calendars": "/cal/one/,/cal/two/",
			}),
		)
		if err != nil {
			t.Fatalf("protocol error: %v", err)
		}
		if !result.IsError || len(service.searches) != 1 {
			t.Fatalf("result=%s searches=%d, want cancellation after one search", resultText(t, result), len(service.searches))
		}
	})
}
