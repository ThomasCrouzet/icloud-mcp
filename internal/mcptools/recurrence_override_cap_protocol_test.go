package mcptools

import (
	"encoding/json"
	"fmt"
	"testing"
)

// TestProtocolRecurrenceOverrideResolvedBeforeCap covers issue 221 end to end
// with the real Calendar client. The July 1 occurrence is moved to July 20,
// and another override lands inside the window on July 3 at 13:00. All valid
// in-range occurrences must survive.
func TestProtocolRecurrenceOverrideResolvedBeforeCap(t *testing.T) {
	path := recurrenceProtocolHome + "override/"
	protocol := newRecurrenceProtocol(t, "UTC", map[string][]string{
		path: {"override-move.ics"},
	})
	args := map[string]any{
		"calendar": path, "start": "2026-07-01T00:00:00Z", "end": "2026-07-05T00:00:00Z", "limit": 400,
	}
	result := protocol.call(t, "search_events", args)
	assertRecurrenceProtocolResult(t, result, false, json.RawMessage(fmt.Sprintf(`{
		"count": 4,
		"total": 4,
		"offset": 0,
		"limit": 400,
		"truncated": false,
		"events": [
			{"calendar": %q, "uid": "override-move", "title": "Daily", "start": "2026-07-02T09:00:00+00:00", "end": "2026-07-02T10:00:00+00:00", "recurrenceId": "2026-07-02T09:00:00+00:00", "etag": %q},
			{"calendar": %q, "uid": "override-move", "title": "Daily", "start": "2026-07-03T09:00:00+00:00", "end": "2026-07-03T10:00:00+00:00", "recurrenceId": "2026-07-03T09:00:00+00:00", "etag": %q},
			{"calendar": %q, "uid": "override-move", "title": "Moved in", "start": "2026-07-03T13:00:00+00:00", "end": "2026-07-03T14:00:00+00:00", "recurrenceId": "2026-06-10T09:00:00+00:00", "isOverride": true, "etag": %q},
			{"calendar": %q, "uid": "override-move", "title": "Daily", "start": "2026-07-04T09:00:00+00:00", "end": "2026-07-04T10:00:00+00:00", "recurrenceId": "2026-07-04T09:00:00+00:00", "etag": %q}
		]
	}`, path, recurrenceProtocolETag, path, recurrenceProtocolETag, path, recurrenceProtocolETag, path, recurrenceProtocolETag)))
}

// TestProtocolRecurrenceMovedOutOccurrenceDoesNotConsumeCap narrows the window
// so only July 2 and July 3 remain: neither may be lost to the suppressed July
// 1 occurrence, and find_free_slots must still see the real busy rows.
func TestProtocolRecurrenceMovedOutOccurrenceDoesNotConsumeCap(t *testing.T) {
	path := recurrenceProtocolHome + "override/"
	protocol := newRecurrenceProtocol(t, "UTC", map[string][]string{
		path: {"override-move.ics"},
	})
	args := map[string]any{
		"calendar": path, "start": "2026-07-02T00:00:00Z", "end": "2026-07-04T00:00:00Z", "limit": 400,
	}
	result := protocol.call(t, "search_events", args)
	assertRecurrenceProtocolResult(t, result, false, json.RawMessage(fmt.Sprintf(`{
		"count": 3,
		"total": 3,
		"offset": 0,
		"limit": 400,
		"truncated": false,
		"events": [
			{"calendar": %q, "uid": "override-move", "title": "Daily", "start": "2026-07-02T09:00:00+00:00", "end": "2026-07-02T10:00:00+00:00", "recurrenceId": "2026-07-02T09:00:00+00:00", "etag": %q},
			{"calendar": %q, "uid": "override-move", "title": "Daily", "start": "2026-07-03T09:00:00+00:00", "end": "2026-07-03T10:00:00+00:00", "recurrenceId": "2026-07-03T09:00:00+00:00", "etag": %q},
			{"calendar": %q, "uid": "override-move", "title": "Moved in", "start": "2026-07-03T13:00:00+00:00", "end": "2026-07-03T14:00:00+00:00", "recurrenceId": "2026-06-10T09:00:00+00:00", "isOverride": true, "etag": %q}
		]
	}`, path, recurrenceProtocolETag, path, recurrenceProtocolETag, path, recurrenceProtocolETag)))
}
