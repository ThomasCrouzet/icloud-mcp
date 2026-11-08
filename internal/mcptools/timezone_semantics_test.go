package mcptools

import (
	"context"
	"testing"
	"time"

	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
)

// A floating event is interpreted in the free-slot timezone before the busy
// projection, so a UTC query range cannot hide a Paris wall-clock event.
func TestFindFreeSlotsUsesFloatingEventTimezone(t *testing.T) {
	paris, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Fatal(err)
	}
	service := &icloud.MockService{
		Calendars: []icloud.Calendar{{Path: "/cal/home/"}},
		Events: []icloud.Event{{
			UID: "floating", IsFloating: true,
			StartTime: time.Date(2026, 7, 1, 12, 30, 0, 0, time.UTC),
			EndTime:   time.Date(2026, 7, 1, 12, 45, 0, 0, time.UTC),
		}},
	}
	deps := freeSlotsTestDeps(service)
	deps.DefaultLocation = paris
	result, callErr := findFreeSlotsHandler(deps)(
		context.Background(),
		freeSlotsTestRequest(
			time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC),
			time.Date(2026, 7, 1, 11, 0, 0, 0, time.UTC),
			map[string]any{
				"calendar":             "/cal/home/",
				"timezone":             "Europe/Paris",
				"duration_minutes":     15,
				"working_hours_start":  "00:00",
				"working_hours_end":    "24:00",
				"include_all_day_busy": true,
			}),
	)
	if callErr != nil || result.IsError {
		t.Fatalf("err=%v result=%s", callErr, resultText(t, result))
	}
	if service.LastSearchFrom.Location().String() != "Europe/Paris" || !service.LastSearchFrom.Equal(time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("search start = %v, want the same instant in Europe/Paris", service.LastSearchFrom)
	}
	var payload freeSlotsResponse
	decodeResult(t, result, &payload)
	busyStart := time.Date(2026, 7, 1, 10, 30, 0, 0, time.UTC)
	busyEnd := time.Date(2026, 7, 1, 10, 45, 0, 0, time.UTC)
	for _, slot := range payload.Slots {
		start, startErr := time.Parse(time.RFC3339, slot.Start)
		end, endErr := time.Parse(time.RFC3339, slot.End)
		if startErr != nil || endErr != nil {
			t.Fatalf("slot parse: %v / %v", startErr, endErr)
		}
		if start.Before(busyEnd) && end.After(busyStart) {
			t.Fatalf("free slot %s-%s overlaps floating busy time %s-%s", slot.Start, slot.End, busyStart, busyEnd)
		}
	}
}

// A naive start with an explicit timezone is interpreted in that timezone, not
// in the default location.
func TestFindFreeSlotsParsesNaiveInputInRequestedTimezone(t *testing.T) {
	newYork, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	paris, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Fatal(err)
	}
	service := &icloud.MockService{Calendars: []icloud.Calendar{{Path: "/cal/home/"}}}
	deps := freeSlotsTestDeps(service)
	deps.DefaultLocation = paris
	request := freeSlotsTestRequest(time.Time{}, time.Time{}, map[string]any{
		"calendar":         "/cal/home/",
		"timezone":         "America/New_York",
		"start":            "2026-07-01T09:00:00",
		"end":              "2026-07-01T10:00:00",
		"duration_minutes": 15,
	})
	result, callErr := findFreeSlotsHandler(deps)(context.Background(), request)
	if callErr != nil || result.IsError {
		t.Fatalf("err=%v result=%s", callErr, resultText(t, result))
	}
	want := time.Date(2026, 7, 1, 9, 0, 0, 0, newYork)
	if !service.LastSearchFrom.Equal(want) || service.LastSearchFrom.Location().String() != "America/New_York" {
		t.Fatalf("naive start = %v, want %v", service.LastSearchFrom, want)
	}
}
