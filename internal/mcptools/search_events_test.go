package mcptools

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
)

func TestSearchEventsHandler_HappyPath(t *testing.T) {
	svc := &icloud.MockService{Events: []icloud.Event{
		{UID: "1", Title: "Meeting", StartTime: time.Date(2026, 7, 2, 9, 0, 0, 0, time.UTC), EndTime: time.Date(2026, 7, 2, 10, 0, 0, 0, time.UTC)},
		{UID: "2", Title: "Lunch", StartTime: time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC), EndTime: time.Date(2026, 7, 1, 13, 0, 0, 0, time.UTC)},
	}}
	handler := searchEventsHandler(testDeps(svc))

	res, err := handler(context.Background(), newReq(map[string]any{
		"start":    "2026-07-01T00:00:00Z",
		"end":      "2026-07-08T00:00:00Z",
		"calendar": "/cal/home/",
	}))
	if err != nil {
		t.Fatalf("unexpected protocol error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %s", resultText(t, res))
	}

	var payload searchEventsResponse
	decodeResult(t, res, &payload)
	if payload.Total != 2 || payload.Count != 2 {
		t.Fatalf("payload = %+v", payload)
	}
	// Sorted by StartTime: Lunch (07-01) before Meeting (07-02).
	if payload.Events[0].Title != "Lunch" || payload.Events[1].Title != "Meeting" {
		t.Errorf("unexpected order: %+v", payload.Events)
	}
	if svc.LastSearchPath != "/cal/home/" {
		t.Errorf("LastSearchPath = %q", svc.LastSearchPath)
	}
}

func TestSearchEventsHandler_MissingCalendarSearchesAll(t *testing.T) {
	svc := &icloud.MockService{
		Calendars: []icloud.Calendar{{Path: "/cal/a/"}, {Path: "/cal/b/"}},
		Events:    []icloud.Event{{UID: "1", Title: "x", StartTime: time.Now(), EndTime: time.Now().Add(time.Hour)}},
	}
	handler := searchEventsHandler(testDeps(svc))

	res, err := handler(context.Background(), newReq(map[string]any{
		"start": "2026-07-01T00:00:00Z",
		"end":   "2026-07-08T00:00:00Z",
	}))
	if err != nil {
		t.Fatalf("unexpected protocol error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %s", resultText(t, res))
	}
	if svc.ListCallCount != 1 {
		t.Errorf("ListCallCount = %d, want 1", svc.ListCallCount)
	}
	if svc.SearchCallCount != 2 {
		t.Errorf("SearchCallCount = %d, want 2 (one search per calendar)", svc.SearchCallCount)
	}
}

func TestSearchEventsHandler_QueryFilter(t *testing.T) {
	svc := &icloud.MockService{
		Calendars: []icloud.Calendar{{Path: "/cal/home/"}},
		Events: []icloud.Event{
			{UID: "1", Title: "Team meeting", StartTime: time.Now(), EndTime: time.Now().Add(time.Hour)},
			{UID: "2", Title: "Dentist", Location: "Medical office", StartTime: time.Now(), EndTime: time.Now().Add(time.Hour)},
		},
	}
	handler := searchEventsHandler(testDeps(svc))

	res, err := handler(context.Background(), newReq(map[string]any{
		"start": "2026-07-01T00:00:00Z",
		"end":   "2026-07-08T00:00:00Z",
		"query": "MEETING",
	}))
	if err != nil {
		t.Fatalf("unexpected protocol error: %v", err)
	}
	var payload searchEventsResponse
	decodeResult(t, res, &payload)
	if payload.Total != 1 || payload.Events[0].Title != "Team meeting" {
		t.Fatalf("query filter ineffective: %+v", payload)
	}
}

func TestSearchEventsHandler_Pagination(t *testing.T) {
	events := make([]icloud.Event, 450)
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	for i := range events {
		events[i] = icloud.Event{
			UID:       fmt.Sprintf("uid-%d", i),
			Title:     fmt.Sprintf("Event %d", i),
			StartTime: base.Add(time.Duration(i) * time.Hour),
			EndTime:   base.Add(time.Duration(i)*time.Hour + 30*time.Minute),
		}
	}
	svc := &icloud.MockService{Calendars: []icloud.Calendar{{Path: "/cal/home/"}}, Events: events}
	handler := searchEventsHandler(testDeps(svc))

	res, err := handler(context.Background(), newReq(map[string]any{
		"start": "2026-01-01T00:00:00Z",
		"end":   "2026-12-31T00:00:00Z",
		"limit": float64(400),
	}))
	if err != nil {
		t.Fatalf("unexpected protocol error: %v", err)
	}
	if res.IsError {
		t.Fatalf("unexpected error result: %s", resultText(t, res))
	}

	var payload searchEventsResponse
	decodeResult(t, res, &payload)
	if payload.Total != 450 {
		t.Errorf("Total = %d, want 450", payload.Total)
	}
	if len(payload.Events) != 400 {
		t.Errorf("len(Events) = %d, want 400 (hard cap)", len(payload.Events))
	}
	if !payload.Truncated {
		t.Errorf("Truncated = false, want true")
	}
}

func TestSearchEventsHandler_InvalidDates(t *testing.T) {
	svc := &icloud.MockService{}
	handler := searchEventsHandler(testDeps(svc))

	res, err := handler(context.Background(), newReq(map[string]any{
		"start": "not a date",
		"end":   "2026-07-08T00:00:00Z",
	}))
	if err != nil {
		t.Fatalf("unexpected protocol error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected an error result for an invalid date")
	}
}

func TestSearchEventsHandler_RangeTooLarge(t *testing.T) {
	svc := &icloud.MockService{}
	handler := searchEventsHandler(testDeps(svc))

	res, err := handler(context.Background(), newReq(map[string]any{
		"start": "2026-01-01T00:00:00Z",
		"end":   "2028-01-01T00:00:00Z",
	}))
	if err != nil {
		t.Fatalf("unexpected protocol error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected an error result for a range > 366 days")
	}
	if !strings.Contains(resultText(t, res), "366") {
		t.Errorf("expected error message mentioning 366 days: %s", resultText(t, res))
	}
}

func TestSearchEventsHandler_MissingRequiredParams(t *testing.T) {
	svc := &icloud.MockService{}
	handler := searchEventsHandler(testDeps(svc))

	res, err := handler(context.Background(), newReq(map[string]any{"end": "2026-07-08T00:00:00Z"}))
	if err != nil {
		t.Fatalf("unexpected protocol error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected an error for missing start")
	}
}
