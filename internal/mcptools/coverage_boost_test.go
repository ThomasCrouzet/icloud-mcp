package mcptools

import (
	"context"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
)

func TestParseAllDayDate(t *testing.T) {
	tt, err := parseAllDayDate("start", "2026-07-01", time.UTC)
	if err != nil || tt.Day() != 1 {
		t.Fatal(err, tt)
	}
	tt, err = parseAllDayDate("start", "2026-07-01T15:00:00Z", time.UTC)
	if err != nil || tt.Hour() != 0 {
		t.Fatal(err, tt)
	}
}

func TestJoinErrors(t *testing.T) {
	if joinErrors(nil) != "validation failed" {
		t.Fatal()
	}
	if joinErrors([]string{"a", "b"}) != "a; b" {
		t.Fatal()
	}
}

func TestResolveCalendarPaths_Multi(t *testing.T) {
	svc := &icloud.MockService{Calendars: []icloud.Calendar{{Path: "/cal/a/"}, {Path: "/cal/b/"}}}
	deps := testDeps(svc)
	paths, err := resolveCalendarPaths(context.Background(), deps, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Arguments: map[string]any{
			"calendars": "/cal/a/,/cal/b/",
		}},
	})
	if err != nil || len(paths) != 2 {
		t.Fatalf("%v %v", paths, err)
	}
	// all calendars when omitted
	paths, err = resolveCalendarPaths(context.Background(), deps, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Arguments: map[string]any{}},
	})
	if err != nil || len(paths) != 2 {
		t.Fatalf("%v %v", paths, err)
	}
}

func TestFindFreeSlots_WithWorkingHoursAndDays(t *testing.T) {
	start := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC) // Wednesday
	svc := &icloud.MockService{
		Calendars: []icloud.Calendar{{Path: "/cal/home/"}},
		Events:    nil,
	}
	h := findFreeSlotsHandler(testDeps(svc))
	res, err := h(context.Background(), mcp.CallToolRequest{
		Params: mcp.CallToolParams{Arguments: map[string]any{
			"start":                 start.Format(time.RFC3339),
			"end":                   start.Add(24 * time.Hour).Format(time.RFC3339),
			"duration_minutes":      60,
			"calendar":              "/cal/home/",
			"working_hours_start":   "09:00",
			"working_hours_end":     "12:00",
			"days_of_week":          "wed",
			"buffer_before_minutes": 0,
			"limit":                 5,
		}},
	})
	if err != nil || res.IsError {
		t.Fatalf("%v %+v", err, res)
	}
}

func TestValidateEventHandler_AllDay(t *testing.T) {
	h := validateEventHandler(testDeps(&icloud.MockService{}))
	res, err := h(context.Background(), mcp.CallToolRequest{
		Params: mcp.CallToolParams{Arguments: map[string]any{
			"title": "H", "start": "2026-07-01", "end": "2026-07-02", "all_day": true,
		}},
	})
	if err != nil || res.IsError {
		t.Fatalf("%v %+v", err, res)
	}
}

func TestSearchEventsHandler_UIDFilter(t *testing.T) {
	start := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	svc := &icloud.MockService{
		Events: []icloud.Event{
			{UID: "keep", Title: "A", StartTime: start, EndTime: start.Add(time.Hour)},
			{UID: "drop", Title: "B", StartTime: start.Add(time.Hour), EndTime: start.Add(2 * time.Hour)},
		},
	}
	h := searchEventsHandler(testDeps(svc))
	res, err := h(context.Background(), mcp.CallToolRequest{
		Params: mcp.CallToolParams{Arguments: map[string]any{
			"start": start.Format(time.RFC3339), "end": start.Add(24 * time.Hour).Format(time.RFC3339),
			"calendar": "/cal/home/", "uid": "keep",
		}},
	})
	if err != nil || res.IsError {
		t.Fatalf("%v %+v", err, res)
	}
	var payload searchEventsResponse
	decodeResult(t, res, &payload)
	if payload.Total != 1 || payload.Events[0].UID != "keep" {
		t.Fatalf("%+v", payload)
	}
}

func TestUpdateEventHandler_WithEtagAndStatus(t *testing.T) {
	svc := &icloud.MockService{}
	h := updateEventHandler(testDeps(svc))
	res, err := h(context.Background(), mcp.CallToolRequest{
		Params: mcp.CallToolParams{Arguments: map[string]any{
			"uid": "u1", "calendar": "/cal/home/", "status": "CANCELLED", "etag": "abc",
		}},
	})
	if err != nil || res.IsError {
		t.Fatalf("%v %+v", err, res)
	}
	if svc.LastUpdate == nil || svc.LastUpdate.IfMatchETag != "abc" {
		t.Fatalf("%+v", svc.LastUpdate)
	}
}

func TestDeleteEventHandler_Success(t *testing.T) {
	svc := &icloud.MockService{DeletedTitle: "Bye"}
	h := deleteEventHandler(testDeps(svc))
	res, err := h(context.Background(), mcp.CallToolRequest{
		Params: mcp.CallToolParams{Arguments: map[string]any{
			"uid": "u1", "calendar": "/cal/home/",
		}},
	})
	if err != nil || res.IsError {
		t.Fatalf("%v %+v", err, res)
	}
	var payload deleteEventResponse
	decodeResult(t, res, &payload)
	if payload.DeletedTitle != "Bye" {
		t.Fatalf("%+v", payload)
	}
}

func TestCreateEventHandler_AllDayDateOnly(t *testing.T) {
	svc := &icloud.MockService{CreatedUID: "ad"}
	h := createEventHandler(testDeps(svc))
	res, err := h(context.Background(), mcp.CallToolRequest{
		Params: mcp.CallToolParams{Arguments: map[string]any{
			"title": "Holiday", "calendar": "/cal/home/",
			"start": "2026-07-01", "end": "2026-07-01", "all_day": true,
		}},
	})
	if err != nil || res.IsError {
		t.Fatalf("%v %+v", err, res)
	}
	if svc.LastCreated == nil || !svc.LastCreated.AllDay {
		t.Fatalf("%+v", svc.LastCreated)
	}
}
