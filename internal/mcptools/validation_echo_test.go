package mcptools

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/ThomasCrouzet/icloud-mcp/internal/contacts"
)

// echoMarker is caller text that no error message may repeat.
const echoMarker = "ZQXMARKER"

// echoCase sends one rejected argument value. marker is the caller text, or a
// value that the server derives from it, that the rejection must not repeat.
type echoCase struct {
	tool, name, marker string
	args               map[string]any
}

func calendarEchoCases() []echoCase {
	event := func(tool string, values map[string]any) map[string]any {
		return contractMerge(contractBusinessFixtures()[tool].base, values)
	}
	var cases []echoCase
	for _, tool := range []string{"create_event", "validate_event"} {
		weekly := map[string]any{"recurrence_frequency": "weekly"}
		cases = append(cases,
			echoCase{tool, "by_day", echoMarker, event(tool, contractMerge(weekly, map[string]any{"recurrence_by_day": echoMarker}))},
			echoCase{tool, "by_day after valid day", echoMarker, event(tool, contractMerge(weekly, map[string]any{"recurrence_by_day": "MO," + echoMarker}))},
			echoCase{tool, "until", echoMarker, event(tool, contractMerge(weekly, map[string]any{"recurrence_until": echoMarker}))},
			echoCase{tool, "exceptions", echoMarker, event(tool, contractMerge(weekly, map[string]any{"recurrence_exceptions": echoMarker}))},
			echoCase{tool, "rrule frequency", echoMarker, event(tool, map[string]any{"rrule": "FREQ=" + echoMarker})},
			echoCase{tool, "rrule unknown part", echoMarker, event(tool, map[string]any{"rrule": "FREQ=DAILY;" + echoMarker + "=1"})},
			echoCase{tool, "rrule repeated part", echoMarker, event(tool, map[string]any{"rrule": "FREQ=DAILY;X" + echoMarker + "=1;X" + echoMarker + "=2"})},
			echoCase{tool, "rrule weekday", echoMarker, event(tool, map[string]any{"rrule": "FREQ=WEEKLY;BYDAY=" + echoMarker})},
			echoCase{tool, "rrule interval", echoMarker, event(tool, map[string]any{"rrule": "FREQ=DAILY;INTERVAL=" + echoMarker})},
			echoCase{tool, "rrule until", echoMarker, event(tool, map[string]any{"rrule": "FREQ=DAILY;UNTIL=" + echoMarker})},
			echoCase{tool, "timezone", echoMarker, event(tool, map[string]any{"timezone": echoMarker + "/Nowhere"})},
			echoCase{tool, "url", echoMarker, event(tool, map[string]any{"url": echoMarker})},
			echoCase{tool, "client uid", echoMarker, event(tool, map[string]any{"client_uid": echoMarker + "/x"})},
			echoCase{tool, "alarms list", "987654321", event(tool, map[string]any{"alarms_minutes": "987654321"})},
			echoCase{tool, "reversed range", "2031-05-07", event(tool, map[string]any{"start": "2031-05-07T08:09:10Z", "end": "2031-05-07T07:00:00Z"})},
		)
	}
	cases = append(cases,
		echoCase{"create_event", "second fold", "01:30", event("create_event", map[string]any{
			"timezone": "America/New_York", "start": "2026-11-01T06:30:00Z", "end": "2026-11-01T07:30:00Z",
		})},
		echoCase{"update_event", "status", echoMarker, event("update_event", map[string]any{"status": echoMarker})},
		echoCase{"update_event", "transparency", echoMarker, event("update_event", map[string]any{"transparency": echoMarker})},
		echoCase{"update_event", "url", echoMarker, event("update_event", map[string]any{"url": echoMarker})},
		echoCase{"update_event", "start", echoMarker, event("update_event", map[string]any{"start": echoMarker})},
		echoCase{"update_event", "recurrence id", echoMarker, event("update_event", map[string]any{"scope": "occurrence", "recurrence_id": echoMarker})},
		echoCase{"update_event", "uid", echoMarker, event("update_event", map[string]any{"uid": echoMarker + "/x"})},
		echoCase{"update_event", "etag", echoMarker, event("update_event", map[string]any{"etag": echoMarker})},
		echoCase{"update_event", "reversed range", "2031-05-07", event("update_event", map[string]any{"start": "2031-05-07T08:09:10Z", "end": "2031-05-07T07:00:00Z"})},
		echoCase{"find_free_slots", "working hours form", echoMarker, event("find_free_slots", map[string]any{"calendar": contractCalendar, "working_hours_start": echoMarker})},
		echoCase{"find_free_slots", "working hours range", "25:00", event("find_free_slots", map[string]any{"calendar": contractCalendar, "working_hours_end": "25:00"})},
		echoCase{"find_free_slots", "daylight-saving gap", "02:30", event("find_free_slots", map[string]any{
			"calendar": contractCalendar, "timezone": "Europe/Paris", "start": "2026-03-29T02:30:00", "end": "2026-03-30T00:00:00",
		})},
		echoCase{"find_free_slots", "days of week", echoMarker, event("find_free_slots", map[string]any{"calendar": contractCalendar, "days_of_week": echoMarker})},
		echoCase{"search_events", "calendar", echoMarker, event("search_events", map[string]any{"calendar": echoMarker})},
		echoCase{"search_events", "cursor", echoMarker, event("search_events", map[string]any{"calendar": contractCalendar, "cursor": echoMarker})},
		echoCase{"get_event", "uid", echoMarker, event("get_event", map[string]any{"uid": echoMarker + "/x"})},
		echoCase{"delete_event", "recurrence id", echoMarker, event("delete_event", map[string]any{"scope": "occurrence", "recurrence_id": echoMarker})},
	)
	return cases
}

// TestCalendarValidationErrorsDoNotRepeatInput sends each rejected value
// through the initialized MCP server. The rejection must reach the handler,
// send no request, and repeat neither the value nor a value derived from it.
func TestCalendarValidationErrorsDoNotRepeatInput(t *testing.T) {
	for _, test := range calendarEchoCases() {
		t.Run(test.tool+"/"+test.name, func(t *testing.T) {
			f := newContractRuntime(t, contractProfile{plan: NewCapabilityPlan(false, true, true, true, true)})
			before := f.counts()
			result := f.call(t, test.tool, test.args)
			text := resultText(t, result)
			if strings.Contains(text, "input schema validation failed") {
				t.Fatalf("case must pass the input schema and reach the handler: %s", text)
			}
			rejection := text
			if !result.IsError {
				payload := contractPayload(t, result)
				errs, _ := payload["errors"].([]any)
				if test.tool != "validate_event" || payload["ok"] != false || len(errs) == 0 {
					t.Fatalf("value was accepted: %s", text)
				}
			}
			if strings.Contains(strings.ToUpper(rejection), strings.ToUpper(test.marker)) {
				t.Fatalf("rejection repeats %q: %s", test.marker, rejection)
			}
			if !reflect.DeepEqual(before, f.counts()) {
				t.Fatalf("rejected value reached a backend: before=%v after=%v", before, f.counts())
			}
		})
	}
}

// TestContactsUnknownParameterIsNotRepeated covers the Contacts handler check
// that runs when no input schema refused an unknown key first.
func TestContactsUnknownParameterIsNotRepeated(t *testing.T) {
	fake := &fakeContactsService{}
	deps := contactsTestDeps(fake, nil)
	for name, args := range map[string]map[string]any{
		"root":   {echoMarker: true},
		"nested": {"address_book": contactsTestBook, "name": map[string]any{echoMarker: "x"}},
	} {
		t.Run(name, func(t *testing.T) {
			handler := searchContactsHandler(deps)
			if name == "nested" {
				handler = createContactHandler(deps)
			}
			result, err := handler(context.Background(), contactsRequest(args))
			if err != nil {
				t.Fatal(err)
			}
			payload := contactsErrorPayloadFromResult(t, result)
			if payload.Code != string(contacts.CodeValidation) {
				t.Fatalf("code = %q: %s", payload.Code, resultText(t, result))
			}
			if strings.Contains(payload.Message, echoMarker) {
				t.Fatalf("message repeats the unknown key: %q", payload.Message)
			}
			if !strings.Contains(payload.Message, "unknown parameter") {
				t.Fatalf("message does not name the rule: %q", payload.Message)
			}
		})
	}
	snapshot := fake.snapshot()
	if snapshot.searchCalls != 0 || snapshot.createCalls != 0 {
		t.Fatalf("unknown parameter reached the service: search=%d create=%d", snapshot.searchCalls, snapshot.createCalls)
	}
}
