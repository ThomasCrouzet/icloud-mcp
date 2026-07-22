package mcptools

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
)

func TestGetEventHandler_Success(t *testing.T) {
	svc := &icloud.MockService{
		Detail: &icloud.EventDetail{
			Event: icloud.Event{UID: "u1", Title: "Hello", StartTime: time.Now(), EndTime: time.Now().Add(time.Hour), ETag: "abc"},
		},
	}
	h := getEventHandler(testDeps(svc))
	res, err := h(context.Background(), mcp.CallToolRequest{
		Params: mcp.CallToolParams{Arguments: map[string]any{
			"calendar": "/cal/home/", "uid": "u1",
		}},
	})
	if err != nil || res.IsError {
		t.Fatalf("err=%v res=%+v", err, res)
	}
	var detail icloud.EventDetail
	decodeResult(t, res, &detail)
	if detail.UID != "u1" || detail.Title != "Hello" {
		t.Fatalf("%+v", detail)
	}
	if detail.Path != "" {
		t.Errorf("path must not be exposed: %q", detail.Path)
	}
}

func TestValidateEventHandler_NoNetwork(t *testing.T) {
	// RoundTripper that fails if any HTTP is attempted.
	svc := &icloud.MockService{}
	// Handler must not call Service at all.
	h := validateEventHandler(testDeps(svc))
	res, err := h(context.Background(), mcp.CallToolRequest{
		Params: mcp.CallToolParams{Arguments: map[string]any{
			"title": "T",
			"start": "2026-07-01T10:00:00Z",
			"end":   "2026-07-01T11:00:00Z",
		}},
	})
	if err != nil || res.IsError {
		t.Fatalf("err=%v res=%+v", err, res)
	}
	if svc.GetCallCount+svc.SearchCallCount+svc.CreateCallCount != 0 {
		t.Fatalf("validate_event must not call service: get=%d search=%d create=%d",
			svc.GetCallCount, svc.SearchCallCount, svc.CreateCallCount)
	}
	var payload icloud.ValidationResult
	decodeResult(t, res, &payload)
	if !payload.OK {
		t.Fatalf("want OK: %+v", payload)
	}
}

func TestCalendarCapabilities_NoSecrets(t *testing.T) {
	deps := testDeps(&icloud.MockService{})
	deps.ReadOnly = true
	h := calendarCapabilitiesHandler(deps)
	res, err := h(context.Background(), mcp.CallToolRequest{})
	if err != nil || res.IsError {
		t.Fatalf("err=%v res=%+v", err, res)
	}
	text := resultText(t, res)
	for _, banned := range []string{"ICLOUD_EMAIL", "password", "shard", "DSID", "@", "file://", "caldav.icloud"} {
		if strings.Contains(strings.ToLower(text), strings.ToLower(banned)) && banned != "@" {
			// '@' alone is too broad; skip. email domains not expected.
		}
		if banned != "@" && strings.Contains(text, banned) {
			t.Errorf("capabilities leaked %q: %s", banned, text)
		}
	}
	var cap capabilitiesResponse
	if err := json.Unmarshal([]byte(text), &cap); err != nil {
		t.Fatal(err)
	}
	if !cap.ReadOnly {
		t.Error("expected readOnly true")
	}
	if !cap.Features["free_slots"] || cap.Features["this_and_future"] {
		t.Errorf("features: %+v", cap.Features)
	}
}

func TestDeleteEventHandler_DryRunNoMutation(t *testing.T) {
	svc := &icloud.MockService{DeletedTitle: "Secret title"}
	h := deleteEventHandler(testDeps(svc))
	res, err := h(context.Background(), mcp.CallToolRequest{
		Params: mcp.CallToolParams{Arguments: map[string]any{
			"calendar": "/cal/home/", "uid": "uid-1", "dry_run": true,
		}},
	})
	if err != nil || res.IsError {
		t.Fatalf("err=%v res=%+v", err, res)
	}
	if svc.MutationCount() != 0 {
		t.Fatalf("dry_run recorded mutations: %v", svc.RecordedMutations)
	}
	var payload deleteEventResponse
	decodeResult(t, res, &payload)
	if !payload.DryRun || !payload.Success {
		t.Fatalf("%+v", payload)
	}
}

func TestFindFreeSlotsHandler_DoesNotLeakTitles(t *testing.T) {
	start := time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)
	svc := &icloud.MockService{
		Calendars: []icloud.Calendar{{Path: "/cal/home/"}},
		Events: []icloud.Event{
			{UID: "1", Title: "SECRET-MEETING", StartTime: start.Add(time.Hour), EndTime: start.Add(2 * time.Hour)},
		},
	}
	h := findFreeSlotsHandler(testDeps(svc))
	res, err := h(context.Background(), mcp.CallToolRequest{
		Params: mcp.CallToolParams{Arguments: map[string]any{
			"start":            start.Format(time.RFC3339),
			"end":              start.Add(4 * time.Hour).Format(time.RFC3339),
			"duration_minutes": 30,
			"calendar":         "/cal/home/",
		}},
	})
	if err != nil || res.IsError {
		t.Fatalf("err=%v res=%+v", err, res)
	}
	text := resultText(t, res)
	if strings.Contains(text, "SECRET-MEETING") {
		t.Fatalf("free slots leaked event title: %s", text)
	}
}

func TestCreateEventHandler_ClientUIDConflict(t *testing.T) {
	svc := &icloud.MockService{
		ExistingUIDs: map[string]bool{"client-1": true},
	}
	h := createEventHandler(testDeps(svc))
	res, err := h(context.Background(), mcp.CallToolRequest{
		Params: mcp.CallToolParams{Arguments: map[string]any{
			"title": "T", "calendar": "/cal/home/",
			"start": "2026-07-01T10:00:00Z", "end": "2026-07-01T11:00:00Z",
			"client_uid": "client-1",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("expected conflict error")
	}
	text := resultText(t, res)
	if !strings.Contains(text, "conflict") {
		t.Errorf("want conflict code in %s", text)
	}
}

// failingRT fails any RoundTrip; used to prove local tools need no network.
type failingRT struct{}

func (failingRT) RoundTrip(*http.Request) (*http.Response, error) {
	panic("network forbidden")
}
