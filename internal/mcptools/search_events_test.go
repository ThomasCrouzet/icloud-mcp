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

func TestSearchEventsHandler_RejectsInvalidStatus(t *testing.T) {
	svc := &icloud.MockService{}
	result, err := searchEventsHandler(testDeps(svc))(context.Background(), newReq(map[string]any{
		"start": "2026-07-01T00:00:00Z", "end": "2026-07-02T00:00:00Z",
		"calendar": "/cal/home/", "status": "MAYBE",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError || svc.SearchCallCount != 0 {
		t.Fatalf("invalid status result=%s searchCalls=%d", resultText(t, result), svc.SearchCallCount)
	}
}

func TestResolveSearchCalendarsBoundsExplicitSelections(t *testing.T) {
	svc := &icloud.MockService{}
	deps := testDeps(svc)
	paths, err := resolveSearchCalendars(context.Background(), deps, newReq(map[string]any{
		"calendar": "/cal/a/", "calendars": "/cal/a/,/cal/b/",
	}))
	if err != nil || len(paths) != 2 {
		t.Fatalf("deduplicated paths=%v err=%v", paths, err)
	}

	tooMany := make([]string, 0, maxExplicitSearchCalendars+1)
	for i := 0; i <= maxExplicitSearchCalendars; i++ {
		tooMany = append(tooMany, fmt.Sprintf("/cal/%d/", i))
	}
	_, err = resolveSearchCalendars(context.Background(), deps, newReq(map[string]any{
		"calendars": strings.Join(tooMany, ","),
	}))
	if err == nil || !strings.Contains(err.Error(), "at most") {
		t.Fatalf("explicit calendar cap error = %v", err)
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

func TestSearchEventsHandler_TruncatedByExpansion(t *testing.T) {
	svc := &icloud.MockService{
		Events: []icloud.Event{
			{UID: "a", Title: "A", StartTime: time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC), EndTime: time.Date(2026, 7, 1, 11, 0, 0, 0, time.UTC)},
		},
		SearchTruncated: true,
	}
	handler := searchEventsHandler(testDeps(svc))
	res, err := handler(context.Background(), newReq(map[string]any{
		"start": "2026-07-01T00:00:00Z", "end": "2026-07-08T00:00:00Z", "calendar": "/cal/",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("error: %s", resultText(t, res))
	}
	var payload searchEventsResponse
	decodeResult(t, res, &payload)
	if !payload.TruncatedByExpansion {
		t.Fatal("TruncatedByExpansion = false, want true from service")
	}
}

func TestSearchEventsHandler_MultiCalendarFairCap(t *testing.T) {
	// Each calendar returns MaxResults events; all calendars are still queried
	// (fairness), then the global list is capped at MaxResults after sort.
	events := make([]icloud.Event, icloud.MaxResults)
	for i := range events {
		events[i] = icloud.Event{
			UID:       "e",
			Title:     "x",
			StartTime: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Hour),
			EndTime:   time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(i)*time.Hour + time.Minute),
		}
	}
	svc := &icloud.MockService{
		Calendars: []icloud.Calendar{{Path: "/cal/a/"}, {Path: "/cal/b/"}},
		Events:    events,
	}
	handler := searchEventsHandler(testDeps(svc))
	res, err := handler(context.Background(), newReq(map[string]any{
		"start": "2026-07-01T00:00:00Z", "end": "2026-07-20T00:00:00Z",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("error: %s", resultText(t, res))
	}
	var payload searchEventsResponse
	decodeResult(t, res, &payload)
	if !payload.MultiCalendarCapped {
		t.Error("MultiCalendarCapped = false, want true")
	}
	if !payload.Truncated {
		t.Error("Truncated = false, want true when multi-calendar cap hits")
	}
	// Both calendars searched (no early-stop bias toward the first path).
	if svc.SearchCallCount != 2 {
		t.Errorf("SearchCallCount = %d, want 2 (fair multi-calendar query)", svc.SearchCallCount)
	}
	if payload.Total < icloud.MaxResults {
		t.Errorf("Total = %d, want >= %d", payload.Total, icloud.MaxResults)
	}
}

func TestSearchEventsHandler_MultiCalendarMaterializationBudget(t *testing.T) {
	// Overflow the multi-calendar materialization budget while still querying
	// every selected calendar (no early-stop bias).
	batch := make([]icloud.Event, icloud.MaxMultiSearchMaterialized/2+1)
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	for i := range batch {
		batch[i] = icloud.Event{
			UID:       "e",
			Title:     "x",
			StartTime: base.Add(time.Duration(i) * time.Hour),
			EndTime:   base.Add(time.Duration(i)*time.Hour + time.Minute),
		}
	}
	svc := &icloud.MockService{
		Calendars: []icloud.Calendar{{Path: "/cal/a/"}, {Path: "/cal/b/"}, {Path: "/cal/c/"}},
		Events:    batch,
	}
	handler := searchEventsHandler(testDeps(svc))
	res, err := handler(context.Background(), newReq(map[string]any{
		"start": "2026-07-01T00:00:00Z", "end": "2026-12-01T00:00:00Z",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("expected payload_too_large materialization budget error")
	}
	text := resultText(t, res)
	if !strings.Contains(text, "payload_too_large") && !strings.Contains(text, "materialization") {
		t.Fatalf("error text = %q, want materialization budget", text)
	}
	if svc.SearchCallCount != 3 {
		t.Errorf("SearchCallCount = %d, want 3 (all calendars queried before fail-closed)", svc.SearchCallCount)
	}
}

// TestSearchEventsHandler_QueryDoesNotEarlyStopOnNonMatches: a first calendar
// returning MaxResults non-matching events must NOT prevent searching a later
// calendar that holds matching events (query filter before 400 budget).
func TestSearchEventsHandler_QueryDoesNotEarlyStopOnNonMatches(t *testing.T) {
	noise := make([]icloud.Event, icloud.MaxResults)
	for i := range noise {
		noise[i] = icloud.Event{
			UID:       "noise",
			Title:     "unrelated",
			StartTime: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Hour),
			EndTime:   time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(i)*time.Hour + time.Minute),
		}
	}
	hit := icloud.Event{
		UID:       "hit",
		Title:     "team standup",
		StartTime: time.Date(2026, 7, 2, 9, 0, 0, 0, time.UTC),
		EndTime:   time.Date(2026, 7, 2, 10, 0, 0, 0, time.UTC),
	}
	svc := &icloud.MockService{
		Calendars: []icloud.Calendar{{Path: "/cal/a/"}, {Path: "/cal/b/"}},
		EventsByPath: map[string][]icloud.Event{
			"/cal/a/": noise,
			"/cal/b/": {hit},
		},
	}
	handler := searchEventsHandler(testDeps(svc))
	res, err := handler(context.Background(), newReq(map[string]any{
		"start": "2026-07-01T00:00:00Z",
		"end":   "2026-07-20T00:00:00Z",
		"query": "standup",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("error: %s", resultText(t, res))
	}
	var payload searchEventsResponse
	decodeResult(t, res, &payload)
	if payload.Total != 1 || len(payload.Events) != 1 || payload.Events[0].UID != "hit" {
		t.Fatalf("want the matching event from calendar b, got total=%d events=%+v", payload.Total, payload.Events)
	}
	if svc.SearchCallCount != 2 {
		t.Errorf("SearchCallCount = %d, want 2 (must search both calendars when query filters first calendar to empty)", svc.SearchCallCount)
	}
	if payload.MultiCalendarCapped {
		t.Error("MultiCalendarCapped should be false: matching set never filled the 400 cap")
	}
}
