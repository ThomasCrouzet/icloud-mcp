package mcptools

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
)

type waveService struct {
	icloud.MockService
	calls     atomic.Int32
	inFlight  atomic.Int32
	maxFlight atomic.Int32
}

func (s *waveService) SearchEvents(_ context.Context, path string, start, _ time.Time, _ *icloud.SearchOptions) (icloud.SearchResult, error) {
	s.calls.Add(1)
	now := s.inFlight.Add(1)
	for {
		old := s.maxFlight.Load()
		if now <= old || s.maxFlight.CompareAndSwap(old, now) {
			break
		}
	}
	defer s.inFlight.Add(-1)
	events := make([]icloud.Event, 250)
	for i := range events {
		events[i] = icloud.Event{UID: fmt.Sprintf("%s-%d", path, i), Title: "Event", StartTime: start.Add(time.Duration(i) * time.Minute), EndTime: start.Add(time.Duration(i)*time.Minute + time.Second)}
	}
	return icloud.SearchResult{Events: events}, nil
}

func TestSearchEventsStopsWavesAfterMaterializationOverflow(t *testing.T) {
	svc := &waveService{}
	var calendars []icloud.Calendar
	var paths []string
	for i := 0; i < 60; i++ {
		path := fmt.Sprintf("/cal/wave-%02d/", i)
		calendars = append(calendars, icloud.Calendar{Path: path})
		paths = append(paths, path)
	}
	svc.Calendars = calendars
	scopes := mustScopes(t, "calendar.read=*")
	deps := freeSlotsTestDeps(&svc.MockService)
	deps.Service = svc
	deps.ResourceScopes = scopes
	result, err := searchEventsHandler(deps)(context.Background(), newReq(map[string]any{
		"start":     "2026-07-01T00:00:00Z",
		"end":       "2026-07-01T01:00:00Z",
		"calendars": strings.Join(paths, ","),
		"limit":     10,
	}))
	if err != nil {
		t.Fatalf("handler error = %v", err)
	}
	if !result.IsError {
		t.Fatalf("overflow was not rejected: %s", resultText(t, result))
	}
	if calls := svc.calls.Load(); calls >= 60 {
		t.Fatalf("all %d calendars were queried after a definitive overflow", calls)
	}
	if max := svc.maxFlight.Load(); max > 4 {
		t.Fatalf("concurrent calendar reads = %d, want at most 4", max)
	}
}
