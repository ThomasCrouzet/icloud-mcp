package mcptools

import (
	"encoding/json"
	"strings"
	"testing"
)

// Exercise MCP dispatch, Calendar parsing, and availability against synthetic
// TLS DAV data. Reads must keep historical events and their busy intervals.
// Invalid scheduling values must not reach PUT or DELETE after being read.
func TestProtocolHistoricalSchedulingReads(t *testing.T) {
	for _, test := range []struct {
		name       string
		properties string
	}{
		{"empty organizer", "ORGANIZER:\n"},
		{"plain addresses", "ORGANIZER:legacy@example.invalid\nATTENDEE:guest@example.invalid\n"},
		{"non URI attendee", "ATTENDEE:not-a-uri\n"},
		{"invalid URI", "ORGANIZER:http://[broken\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newCalendarTextProtocol(t, calendarTextObject("SUMMARY:Historical event\n"+test.properties, ""))
			var detail getEventDTO
			f.call(t, "get_event", nil, false, &detail)
			if detail.Title != "Historical event" || !detail.IsGroupScheduled || detail.ETag != calendarTextETag {
				t.Fatalf("read lost event data or the conservative scheduling marker: %+v", detail)
			}
			var page searchEventsResponse
			f.call(t, "search_events", nil, false, &page)
			if page.Count != 1 || page.Events[0].Title != detail.Title {
				t.Fatalf("search lost the historical event: %+v", page)
			}
			var free freeSlotsResponse
			f.call(t, "find_free_slots", map[string]any{
				"start": "2026-11-07T09:00:00Z", "end": "2026-11-07T12:00:00Z",
				"timezone": "UTC", "duration_minutes": 60,
			}, false, &free)
			if free.Count != 2 || free.Slots[0].Start != "2026-11-07T09:00:00+00:00" ||
				free.Slots[0].End != "2026-11-07T10:00:00+00:00" ||
				free.Slots[1].Start != "2026-11-07T11:00:00+00:00" || free.Slots[1].End != "2026-11-07T12:00:00+00:00" {
				t.Fatalf("availability lost the 10:00-11:00 busy interval: %+v", free)
			}
			encoded, err := json.Marshal([]any{detail, page, free})
			if err != nil || strings.Contains(string(encoded), "example.invalid") ||
				strings.Contains(string(encoded), "not-a-uri") || strings.Contains(string(encoded), "[broken") {
				t.Fatal("read results exposed scheduling identities")
			}
			for _, tool := range []string{"update_event", "delete_event"} {
				var denied struct {
					Code string `json:"code"`
				}
				f.call(t, tool, map[string]any{
					"title": "Changed", "etag": calendarTextETag, "acknowledge_scheduling": true,
				}, true, &denied)
				if denied.Code != "protocol_error" {
					t.Fatalf("%s lost strict scheduling validation: %+v", tool, denied)
				}
			}
			if f.deletes.Load() != 0 {
				t.Fatal("reading or rejecting a mutation dispatched DELETE")
			}
		})
	}
}
