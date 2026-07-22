package mcptools

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
)

func TestSearchEventsHandler_CompactAndFilters(t *testing.T) {
	start := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	svc := &icloud.MockService{
		Events: []icloud.Event{
			{UID: "1", Title: "A", Notes: "secret-notes", StartTime: start, EndTime: start.Add(time.Hour), Status: "CONFIRMED"},
			{UID: "2", Title: "B", StartTime: start.Add(2 * time.Hour), EndTime: start.Add(3 * time.Hour), Status: "CANCELLED"},
			{UID: "3", Title: "C", StartTime: start.Add(4 * time.Hour), EndTime: start.Add(5 * time.Hour), Transp: "TRANSPARENT"},
		},
	}
	h := searchEventsHandler(testDeps(svc))
	res, err := h(context.Background(), mcp.CallToolRequest{
		Params: mcp.CallToolParams{Arguments: map[string]any{
			"start":             start.Format(time.RFC3339),
			"end":               start.Add(24 * time.Hour).Format(time.RFC3339),
			"calendar":          "/cal/home/",
			"compact":           true,
			"include_cancelled": false,
			"busy_only":         true,
		}},
	})
	if err != nil || res.IsError {
		t.Fatalf("%v %+v", err, res)
	}
	text := resultText(t, res)
	if strings.Contains(text, "secret-notes") {
		t.Error("compact must omit notes")
	}
	if strings.Contains(text, `"uid": "2"`) || strings.Contains(text, `"uid":"2"`) {
		t.Error("cancelled should be excluded")
	}
}

func TestUpdateEventHandler_OccurrenceRequiresRecurrenceID(t *testing.T) {
	svc := &icloud.MockService{}
	h := updateEventHandler(testDeps(svc))
	res, err := h(context.Background(), mcp.CallToolRequest{
		Params: mcp.CallToolParams{Arguments: map[string]any{
			"uid": "u", "calendar": "/cal/home/", "title": "x", "scope": "occurrence",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("expected validation error")
	}
	if svc.UpdateCallCount != 0 {
		t.Error("must not call service")
	}
}

func TestDeleteEventHandler_OccurrenceRequiresRecurrenceID(t *testing.T) {
	svc := &icloud.MockService{}
	h := deleteEventHandler(testDeps(svc))
	res, err := h(context.Background(), mcp.CallToolRequest{
		Params: mcp.CallToolParams{Arguments: map[string]any{
			"uid": "u", "calendar": "/cal/home/", "scope": "occurrence",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("expected validation error")
	}
}

func TestGetEventHandler_Validation(t *testing.T) {
	svc := &icloud.MockService{}
	h := getEventHandler(testDeps(svc))
	res, err := h(context.Background(), mcp.CallToolRequest{
		Params: mcp.CallToolParams{Arguments: map[string]any{
			"calendar": "../evil", "uid": "u",
		}},
	})
	if err != nil || !res.IsError {
		t.Fatalf("want validation error: %v %+v", err, res)
	}
}

func TestFindFreeSlotsHandler_Validation(t *testing.T) {
	h := findFreeSlotsHandler(testDeps(&icloud.MockService{}))
	res, err := h(context.Background(), mcp.CallToolRequest{
		Params: mcp.CallToolParams{Arguments: map[string]any{
			"start": "2026-07-01T10:00:00Z", "end": "2026-07-01T11:00:00Z",
			"duration_minutes": 0,
		}},
	})
	if err != nil || !res.IsError {
		t.Fatalf("want validation: %v %+v", err, res)
	}
}

func TestParseDaysOfWeek(t *testing.T) {
	days, err := parseDaysOfWeek("mon,wed,5")
	if err != nil {
		t.Fatal(err)
	}
	if len(days) != 3 {
		t.Fatalf("%v", days)
	}
	if _, err := parseDaysOfWeek("funday"); err == nil {
		t.Fatal("expected error")
	}
}

func TestErrResult_ValidationCode(t *testing.T) {
	red := testDeps(&icloud.MockService{}).Redactor
	res := errResult(red, "validation", icloud.NewValidationError("bad"))
	text := resultText(t, res)
	if !strings.Contains(text, "validation") {
		t.Errorf("%s", text)
	}
}

func TestCreateEventHandler_EnrichedOptionalFields(t *testing.T) {
	svc := &icloud.MockService{CreatedUID: "new-1"}
	h := createEventHandler(testDeps(svc))
	res, err := h(context.Background(), mcp.CallToolRequest{
		Params: mcp.CallToolParams{Arguments: map[string]any{
			"title": "T", "calendar": "/cal/home/",
			"start": "2026-07-01T10:00:00Z", "end": "2026-07-01T11:00:00Z",
			"status": "CONFIRMED", "transparency": "OPAQUE",
			"url": "https://example.com", "client_uid": "my-client-uid",
		}},
	})
	if err != nil || res.IsError {
		t.Fatalf("%v %+v", err, res)
	}
	if svc.LastCreated == nil || svc.LastCreated.ClientUID != "my-client-uid" {
		t.Fatalf("%+v", svc.LastCreated)
	}
	if svc.LastCreated.Status != "CONFIRMED" {
		t.Errorf("status=%q", svc.LastCreated.Status)
	}
}
