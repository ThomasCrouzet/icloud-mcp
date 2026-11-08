package mcptools

import (
	"strings"
	"testing"
	"time"

	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
)

// TestFloatingOutputKeepsWallClockAndMarker covers the MCP representation of a
// floating event: offset-free local values plus isFloating true.
func TestFloatingOutputKeepsWallClockAndMarker(t *testing.T) {
	paris, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Fatal(err)
	}
	event := icloud.Event{
		UID:        "floating-1",
		Title:      "Floating",
		StartTime:  time.Date(2026, time.July, 1, 9, 0, 0, 0, time.UTC),
		EndTime:    time.Date(2026, time.July, 1, 10, 0, 0, 0, time.UTC),
		IsFloating: true,
	}
	dto := eventsToDTO([]icloud.Event{event}, false, paris)
	if len(dto) != 1 {
		t.Fatalf("events = %d", len(dto))
	}
	if !dto[0].IsFloating {
		t.Fatal("isFloating is false")
	}
	if dto[0].StartTime != "2026-07-01T09:00:00" || dto[0].EndTime != "2026-07-01T10:00:00" {
		t.Fatalf("floating values gained an offset: %q..%q", dto[0].StartTime, dto[0].EndTime)
	}
	if strings.Contains(dto[0].StartTime, "+") || strings.Contains(dto[0].StartTime, "Z") {
		t.Fatalf("floating start carries an offset: %q", dto[0].StartTime)
	}

	detail := eventDetailToDTO(&icloud.EventDetail{Event: event}, paris)
	if !detail.IsFloating || detail.Start != "2026-07-01T09:00:00" {
		t.Fatalf("get_event DTO = %+v", detail)
	}
	found := false
	for _, warning := range detail.Warnings {
		if strings.Contains(warning, "floating") {
			found = true
		}
	}
	if !found {
		t.Fatalf("floating warning missing: %v", detail.Warnings)
	}

	if warning := floatingTimeWarning([]icloud.Event{event}); !strings.Contains(warning, "floating") {
		t.Fatalf("search warning = %q", warning)
	}
}

// TestAbsoluteOutputStillUsesOffset keeps the previous contract for genuine
// UTC and TZID values.
func TestAbsoluteOutputStillUsesOffset(t *testing.T) {
	paris, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Fatal(err)
	}
	event := icloud.Event{
		UID:       "utc-1",
		StartTime: time.Date(2026, time.July, 1, 9, 0, 0, 0, time.UTC),
		EndTime:   time.Date(2026, time.July, 1, 10, 0, 0, 0, time.UTC),
	}
	dto := eventsToDTO([]icloud.Event{event}, false, paris)
	if dto[0].StartTime != "2026-07-01T11:00:00+02:00" {
		t.Fatalf("UTC output = %q", dto[0].StartTime)
	}
}
