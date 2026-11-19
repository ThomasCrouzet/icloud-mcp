package mcptools

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
)

// floatingScanEvents returns three events in one calendar. Only the second and
// third events use a floating local time or a DST gap resolution, so they
// become visible on later cursor pages.
func floatingScanEvents(calendar string) []icloud.Event {
	start := time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)
	return []icloud.Event{
		{
			UID:       "page-1-utc",
			Title:     "Absolute",
			Calendar:  calendar,
			StartTime: start,
			EndTime:   start.Add(time.Hour),
		},
		{
			UID:        "page-2-floating",
			Title:      "Floating",
			Calendar:   calendar,
			StartTime:  start.Add(2 * time.Hour),
			EndTime:    start.Add(3 * time.Hour),
			IsFloating: true,
		},
		{
			UID:         "page-3-dst",
			Title:       "DST gap",
			Calendar:    calendar,
			StartTime:   start.Add(4 * time.Hour),
			EndTime:     start.Add(5 * time.Hour),
			DSTAdjusted: true,
		},
	}
}

func countWarningsContaining(warnings []string, needle string) int {
	count := 0
	for _, warning := range warnings {
		if strings.Contains(warning, needle) {
			count++
		}
	}
	return count
}

// TestSearchEventsCursorRepeatsScanLevelFloatingWarning covers issue 217: the
// floating/DST warning describes the completed scan, so every cursor page of
// that scan must report it, including pages whose own rows are unaffected.
func TestSearchEventsCursorRepeatsScanLevelFloatingWarning(t *testing.T) {
	svc := &icloud.MockService{
		Calendars: []icloud.Calendar{{Path: "/cal/a/"}},
		Events:    floatingScanEvents("/cal/a/"),
	}
	c := newCursorProtocolMCP(t, svc, "calendar.read=/cal/a/")

	first := cursorCallTool(t, c, cursorBaseArgs("/cal/a/", 1, 0, "start"))
	if first.payload.Count != 1 || first.token == "" {
		t.Fatalf("first page = %+v", first.payload)
	}
	if first.payload.Events[0].UID != "page-1-utc" {
		t.Fatalf("first page rows = %+v", first.payload.Events)
	}
	if got := countWarningsContaining(first.payload.Warnings, "floating local times"); got != 1 {
		t.Fatalf("first page floating warnings = %d in %v", got, first.payload.Warnings)
	}
	if got := countWarningsContaining(first.payload.Warnings, "daylight-saving gap"); got != 1 {
		t.Fatalf("first page DST warnings = %d in %v", got, first.payload.Warnings)
	}

	// Page 2 returns a row that is itself floating. The scan-level warnings
	// must still appear exactly once each, with no duplicate for this page.
	second := cursorCallTool(t, c, cursorBaseArgs("/cal/a/", 1, 0, first.token))
	if second.payload.Count != 1 || second.payload.Events[0].UID != "page-2-floating" {
		t.Fatalf("second page = %+v", second.payload)
	}
	if got := countWarningsContaining(second.payload.Warnings, "floating local times"); got != 1 {
		t.Fatalf("second page floating warnings = %d in %v", got, second.payload.Warnings)
	}
	if got := countWarningsContaining(second.payload.Warnings, "daylight-saving gap"); got != 1 {
		t.Fatalf("second page DST warnings = %d in %v", got, second.payload.Warnings)
	}
	if second.token == "" {
		t.Fatalf("second page did not continue: %+v", second.payload)
	}

	// Page 3 is the final page and carries the DST-adjusted row only.
	third := cursorCallTool(t, c, cursorBaseArgs("/cal/a/", 1, 0, second.token))
	if third.payload.Count != 1 || third.payload.Events[0].UID != "page-3-dst" {
		t.Fatalf("third page = %+v", third.payload)
	}
	if got := countWarningsContaining(third.payload.Warnings, "floating local times"); got != 1 {
		t.Fatalf("third page floating warnings = %d in %v", got, third.payload.Warnings)
	}
	if got := countWarningsContaining(third.payload.Warnings, "daylight-saving gap"); got != 1 {
		t.Fatalf("third page DST warnings = %d in %v", got, third.payload.Warnings)
	}

	// Snapshot re-use: the same token returns the same page and warnings.
	repeated := cursorCallTool(t, c, cursorBaseArgs("/cal/a/", 1, 0, second.token))
	if repeated.payload.Count != 1 || repeated.payload.Events[0].UID != "page-3-dst" {
		t.Fatalf("repeated page = %+v", repeated.payload)
	}
	if len(repeated.payload.Warnings) != len(third.payload.Warnings) {
		t.Fatalf("repeated page warnings = %v, want %v", repeated.payload.Warnings, third.payload.Warnings)
	}
	if svc.SearchCallCount != 1 {
		t.Fatalf("backend searches = %d, want 1 shared snapshot", svc.SearchCallCount)
	}
}

// TestSearchEventsLegacyFloatingWarningScopeUnchanged keeps the documented
// page-local behavior of direct (non-cursor) queries.
func TestSearchEventsLegacyFloatingWarningScopeUnchanged(t *testing.T) {
	svc := &icloud.MockService{
		Calendars: []icloud.Calendar{{Path: "/cal/a/"}},
		Events:    floatingScanEvents("/cal/a/"),
	}
	deps := testDeps(svc)
	deps.ResourceScopes = mustScopes(t, "calendar.read=/cal/a/")
	handler := searchEventsHandlerWithCodec(deps, newTestCursorCodec(time.Now()))

	res, err := handler(context.Background(), newReq(map[string]any{
		"start": "2026-07-01T00:00:00Z", "end": "2026-07-02T00:00:00Z",
		"calendar": "/cal/a/", "limit": float64(1),
	}))
	if err != nil || res.IsError {
		t.Fatalf("legacy page failed: err=%v result=%v", err, res)
	}
	var payload searchEventsResponse
	decodeResult(t, res, &payload)
	if payload.Count != 1 || payload.Events[0].UID != "page-1-utc" {
		t.Fatalf("legacy page = %+v", payload)
	}
	// A direct query describes the returned page only.
	if got := countWarningsContaining(payload.Warnings, "floating local times"); got != 0 {
		t.Fatalf("legacy page floating warnings = %d in %v", got, payload.Warnings)
	}
}

// TestSearchEventsCursorWarningFollowsFilteredScan proves the stored warning
// matches the snapshot contents: rows dropped by a filter never contribute.
func TestSearchEventsCursorWarningFollowsFilteredScan(t *testing.T) {
	events := floatingScanEvents("/cal/a/")
	events[1].Transp = "TRANSPARENT"
	svc := &icloud.MockService{
		Calendars: []icloud.Calendar{{Path: "/cal/a/"}},
		Events:    events,
	}
	codec := newTestCursorCodec(time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC))
	deps := testDeps(svc)
	deps.ResourceScopes = mustScopes(t, "calendar.read=/cal/a/")
	handler := searchEventsHandlerWithCodec(deps, codec)

	args := cursorBaseArgs("/cal/a/", 5, 0, "start")
	args["busy_only"] = true
	res, err := handler(context.Background(), newReq(args))
	if err != nil || res.IsError {
		t.Fatalf("filtered scan failed: err=%v result=%v", err, res)
	}
	var payload searchEventsResponse
	decodeResult(t, res, &payload)
	if payload.Total != 2 {
		t.Fatalf("filtered scan total = %d", payload.Total)
	}
	if got := countWarningsContaining(payload.Warnings, "floating local times"); got != 0 {
		t.Fatalf("filtered scan floating warnings = %d in %v", got, payload.Warnings)
	}
	if got := countWarningsContaining(payload.Warnings, "daylight-saving gap"); got != 1 {
		t.Fatalf("filtered scan DST warnings = %d in %v", got, payload.Warnings)
	}
}
