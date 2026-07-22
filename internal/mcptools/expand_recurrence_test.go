package mcptools

import (
	"context"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
)

func TestSearchEventsHandler_ExpandRecurrenceFalse(t *testing.T) {
	start := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(7 * 24 * time.Hour)
	master := icloud.Event{
		UID: "series-1", Title: "Weekly",
		StartTime: start.Add(10 * time.Hour), EndTime: start.Add(11 * time.Hour),
		Recurrence: "FREQ=WEEKLY;COUNT=5",
	}
	// Expanded path returns many occurrences; unexpanded returns the master only.
	expanded := make([]icloud.Event, 0, 5)
	for i := 0; i < 5; i++ {
		e := master
		e.StartTime = master.StartTime.Add(time.Duration(i) * 7 * 24 * time.Hour)
		e.EndTime = master.EndTime.Add(time.Duration(i) * 7 * 24 * time.Hour)
		e.Recurrence = ""
		expanded = append(expanded, e)
	}
	svc := &icloud.MockService{
		Events:           expanded,
		EventsUnexpanded: []icloud.Event{master},
	}
	h := searchEventsHandler(testDeps(svc))

	// expand true (default): uses Events (5 occs)
	res, err := h(context.Background(), mcp.CallToolRequest{
		Params: mcp.CallToolParams{Arguments: map[string]any{
			"start": start.Format(time.RFC3339), "end": end.Format(time.RFC3339),
			"calendar": "/cal/home/",
		}},
	})
	if err != nil || res.IsError {
		t.Fatalf("%v %+v", err, res)
	}
	var payload searchEventsResponse
	decodeResult(t, res, &payload)
	if payload.Total != 5 {
		t.Fatalf("expand default total=%d want 5", payload.Total)
	}
	if svc.LastSearchOpts == nil || !svc.LastSearchOpts.ExpandRecurrence {
		t.Fatalf("expected ExpandRecurrence true, got %+v", svc.LastSearchOpts)
	}

	// expand false: uses EventsUnexpanded (1 master)
	res, err = h(context.Background(), mcp.CallToolRequest{
		Params: mcp.CallToolParams{Arguments: map[string]any{
			"start": start.Format(time.RFC3339), "end": end.Format(time.RFC3339),
			"calendar": "/cal/home/", "expand_recurrence": false,
		}},
	})
	if err != nil || res.IsError {
		t.Fatalf("%v %+v", err, res)
	}
	decodeResult(t, res, &payload)
	if payload.Total != 1 {
		t.Fatalf("expand false total=%d want 1 master", payload.Total)
	}
	if payload.Events[0].Recurrence == "" {
		t.Error("master should retain recurrence string")
	}
	if svc.LastSearchOpts == nil || svc.LastSearchOpts.ExpandRecurrence {
		t.Fatalf("expected ExpandRecurrence false, got %+v", svc.LastSearchOpts)
	}
}

func TestCreateEventHandler_MultiAlarmAndStructuredRecurrence(t *testing.T) {
	svc := &icloud.MockService{CreatedUID: "s1"}
	h := createEventHandler(testDeps(svc))
	res, err := h(context.Background(), mcp.CallToolRequest{
		Params: mcp.CallToolParams{Arguments: map[string]any{
			"title": "Standup", "calendar": "/cal/home/",
			"start": "2026-07-01T10:00:00Z", "end": "2026-07-01T10:30:00Z",
			"alarms_minutes":       "15,60",
			"recurrence_frequency": "weekly",
			"recurrence_interval":  1,
			"recurrence_count":     4,
			"recurrence_by_day":    "MO,WE",
		}},
	})
	if err != nil || res.IsError {
		t.Fatalf("%v %+v text=%s", err, res, resultText(t, res))
	}
	if svc.LastCreated == nil {
		t.Fatal("no create")
	}
	if len(svc.LastCreated.Alarms) != 2 {
		t.Fatalf("alarms=%+v", svc.LastCreated.Alarms)
	}
	if svc.LastCreated.Recurrence == "" || svc.LastCreated.Recurrence[:4] != "FREQ" {
		t.Fatalf("rrule=%q", svc.LastCreated.Recurrence)
	}
}

func TestValidateEventHandler_StructuredRecurrence(t *testing.T) {
	svc := &icloud.MockService{}
	h := validateEventHandler(testDeps(svc))
	res, err := h(context.Background(), mcp.CallToolRequest{
		Params: mcp.CallToolParams{Arguments: map[string]any{
			"title": "T", "start": "2026-07-01T10:00:00Z", "end": "2026-07-01T11:00:00Z",
			"recurrence_frequency": "daily", "recurrence_count": 3,
			"alarms_minutes": "10,20",
		}},
	})
	if err != nil || res.IsError {
		t.Fatalf("%v %+v", err, res)
	}
	if svc.SearchCallCount+svc.CreateCallCount+svc.GetCallCount != 0 {
		t.Fatal("validate must not call service")
	}
	var payload icloud.ValidationResult
	decodeResult(t, res, &payload)
	if !payload.OK {
		t.Fatalf("%+v", payload)
	}
	if payload.Normalized == nil || payload.Normalized.Recurrence == "" {
		t.Fatal("expected normalized rrule")
	}
	if len(payload.Normalized.AlarmMinutes) < 2 {
		t.Fatalf("alarms=%v", payload.Normalized.AlarmMinutes)
	}
}
