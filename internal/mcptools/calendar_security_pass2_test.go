package mcptools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
)

func TestSearchEventsResultBudgetTruncatesAtEventBoundaries(t *testing.T) {
	events := make([]icloud.Event, icloud.MaxResults)
	start := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	for i := range events {
		events[i] = icloud.Event{
			UID:       fmt.Sprintf("uid-%03d", i),
			Title:     strings.Repeat("t", icloud.MaxTitleLen),
			Notes:     strings.Repeat("n", icloud.MaxNotesLen),
			StartTime: start.Add(time.Duration(i) * time.Minute),
			EndTime:   start.Add(time.Duration(i+1) * time.Minute),
		}
	}
	handler := searchEventsHandler(testDeps(&icloud.MockService{Events: events}))
	result, err := handler(context.Background(), newReq(map[string]any{
		"start": "2026-07-01T00:00:00Z", "end": "2026-07-02T00:00:00Z",
		"calendar": "/cal/home/", "limit": float64(icloud.MaxResults),
	}))
	if err != nil || result.IsError {
		t.Fatalf("search failed: err=%v result=%s", err, resultText(t, result))
	}
	text := resultText(t, result)
	if len(text) > maxCalendarResultBytes {
		t.Fatalf("serialized search result = %d bytes, want <= %d", len(text), maxCalendarResultBytes)
	}
	var payload searchEventsResponse
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		t.Fatalf("truncated result is not valid JSON: %v", err)
	}
	if !payload.Truncated || payload.Count != len(payload.Events) || payload.Count >= len(events) {
		t.Fatalf("budget flags/count = truncated:%v count:%d events:%d total source:%d", payload.Truncated, payload.Count, len(payload.Events), len(events))
	}
	for i, event := range payload.Events {
		if event.UID != events[i].UID || len(event.Notes) != icloud.MaxNotesLen {
			t.Fatalf("event %d was partially serialized: %+v", i, event)
		}
	}
}

func TestGetEventResultBudgetTruncatesOverridesWithWarning(t *testing.T) {
	const overrideCount = 512
	overrides := make([]icloud.OccurrenceRef, overrideCount)
	start := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	for i := range overrides {
		overrides[i] = icloud.OccurrenceRef{
			RecurrenceID: start.AddDate(0, 0, i),
			StartTime:    start.AddDate(0, 0, i),
			EndTime:      start.AddDate(0, 0, i).Add(time.Hour),
			Title:        strings.Repeat("t", icloud.MaxTitleLen),
			IsOverride:   true,
		}
	}
	detail := &icloud.EventDetail{
		Event:         icloud.Event{UID: "uid-1", Title: "Master", StartTime: start, EndTime: start.Add(time.Hour)},
		OverrideCount: overrideCount,
		Overrides:     overrides,
	}
	handler := getEventHandler(testDeps(&icloud.MockService{Detail: detail}))
	result, err := handler(context.Background(), newReq(map[string]any{"calendar": "/cal/home/", "uid": "uid-1"}))
	if err != nil || result.IsError {
		t.Fatalf("get_event failed: err=%v result=%s", err, resultText(t, result))
	}
	text := resultText(t, result)
	if len(text) > maxCalendarResultBytes {
		t.Fatalf("serialized get_event result = %d bytes, want <= %d", len(text), maxCalendarResultBytes)
	}
	var payload icloud.EventDetail
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		t.Fatalf("bounded detail is not valid JSON: %v", err)
	}
	if !payload.OverridesTruncated || len(payload.Warnings) == 0 || payload.OverrideCount != overrideCount {
		t.Fatalf("missing truncation metadata: %+v", payload)
	}
	if len(payload.Overrides) == 0 || len(payload.Overrides) >= overrideCount {
		t.Fatalf("override count after result budget = %d, want 1..%d", len(payload.Overrides), overrideCount-1)
	}
}

func TestGetEventResultBudgetFailsSafelyWhenMasterCannotFit(t *testing.T) {
	detail := &icloud.EventDetail{Event: icloud.Event{
		UID:       "uid-1",
		Title:     "Master",
		Notes:     strings.Repeat("remote", maxCalendarResultBytes),
		StartTime: time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC),
		EndTime:   time.Date(2026, 7, 1, 11, 0, 0, 0, time.UTC),
	}}
	handler := getEventHandler(testDeps(&icloud.MockService{Detail: detail}))
	result, err := handler(context.Background(), newReq(map[string]any{"calendar": "/cal/home/", "uid": "uid-1"}))
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Fatal("expected oversized master detail to fail safely")
	}
	var payload toolErrorPayload
	if err := json.Unmarshal([]byte(resultText(t, result)), &payload); err != nil {
		t.Fatalf("error result is not JSON: %v", err)
	}
	if payload.Code != string(icloud.CodePayloadTooLarge) {
		t.Fatalf("error code = %q, want %q", payload.Code, icloud.CodePayloadTooLarge)
	}
}
