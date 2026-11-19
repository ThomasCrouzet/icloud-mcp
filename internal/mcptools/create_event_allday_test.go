package mcptools

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ThomasCrouzet/icloud-mcp/internal/icloud"
)

// TestCreateEventHandlerAllDayAcceptsNamedTimezone covers NEW-1. An all-day
// create stores VALUE=DATE bounds as bare calendar dates, so no TZID wall time
// is written and the fold round-trip rule cannot apply. The handler must skip
// that check when all_day is set, in the same way the client skips it for
// ne.AllDay. Asia/Baku 2015-10-25 is one of the zones and dates where a
// midnight-UTC bound does not resolve back to itself under the zone.
func TestCreateEventHandlerAllDayAcceptsNamedTimezone(t *testing.T) {
	for _, test := range []struct {
		name     string
		start    string
		end      string
		timezone string
	}{
		{name: "Asia/Baku 2015 fold day", start: "2015-10-25", end: "2015-10-26", timezone: "Asia/Baku"},
		{name: "Europe/Chisinau 2016 fold day", start: "2016-10-29", end: "2016-10-30", timezone: "Europe/Chisinau"},
		{name: "Africa/Windhoek 2015 fold day", start: "2015-04-04", end: "2015-04-05", timezone: "Africa/Windhoek"},
		{name: "Australia/Lord_Howe half-hour fold day", start: "2026-04-05", end: "2026-04-06", timezone: "Australia/Lord_Howe"},
		{name: "Europe/Paris ordinary day", start: "2026-07-13", end: "2026-07-14", timezone: "Europe/Paris"},
	} {
		t.Run(test.name, func(t *testing.T) {
			svc := &icloud.MockService{CreatedUID: "all-day-uid"}
			handler := createEventHandler(testDeps(svc))
			res, err := handler(t.Context(), newReq(map[string]any{
				"title": "All day", "start": test.start, "end": test.end,
				"calendar": "/cal/home/", "all_day": true, "timezone": test.timezone,
			}))
			if err != nil {
				t.Fatalf("unexpected protocol error: %v", err)
			}
			if res.IsError {
				t.Fatalf("all-day create with timezone %s was rejected: %s", test.timezone, resultText(t, res))
			}
			if svc.CreateCallCount != 1 {
				t.Fatalf("CreateEvent calls = %d, want 1", svc.CreateCallCount)
			}
			created := svc.LastCreated
			if created == nil || !created.AllDay {
				t.Fatalf("LastCreated = %+v, want an all-day event", created)
			}
			// All-day bounds are midnight UTC on the requested civil dates.
			if got := created.StartTime.UTC().Format("2006-01-02T15:04:05Z07:00"); got != test.start+"T00:00:00Z" {
				t.Fatalf("start = %s, want %sT00:00:00Z", got, test.start)
			}
		})
	}
}

// TestCreateEventHandlerAllDayKeepsDateForm pins that the accepted all-day
// create still travels as all_day, not as a timed TZID event.
func TestCreateEventHandlerAllDayKeepsDateForm(t *testing.T) {
	svc := &icloud.MockService{CreatedUID: "all-day-uid"}
	handler := createEventHandler(testDeps(svc))
	res, err := handler(t.Context(), newReq(map[string]any{
		"title": "All day", "start": "2015-10-25", "end": "2015-10-26",
		"calendar": "/cal/home/", "all_day": true, "timezone": "Asia/Baku",
	}))
	if err != nil || res.IsError {
		t.Fatalf("all-day create: err=%v result=%s", err, resultText(t, res))
	}
	if svc.LastCreated == nil || !svc.LastCreated.AllDay {
		t.Fatalf("LastCreated = %+v, want AllDay true", svc.LastCreated)
	}
}

// TestCreateEventHandlerTimedStillRejectsFold keeps the timed rule intact after
// the all-day skip: a timed second-fold bound must still fail.
func TestCreateEventHandlerTimedStillRejectsFold(t *testing.T) {
	svc := &icloud.MockService{CreatedUID: "timed-uid"}
	handler := createEventHandler(testDeps(svc))
	res, err := handler(t.Context(), newReq(map[string]any{
		"title": "Timed", "start": "2026-10-25T02:30:00+01:00",
		"end": "2026-10-25T04:30:00+01:00", "calendar": "/cal/home/",
		"timezone": "Europe/Paris",
	}))
	if err != nil {
		t.Fatalf("unexpected protocol error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("timed second-fold create was accepted: %s", resultText(t, res))
	}
	var payload toolErrorPayload
	if err := json.Unmarshal([]byte(resultText(t, res)), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Code != string(icloud.CodeValidation) {
		t.Fatalf("code = %q, want validation", payload.Code)
	}
	if svc.CreateCallCount != 0 {
		t.Fatalf("rejected timed create reached CreateEvent %d time(s)", svc.CreateCallCount)
	}
}

// TestCreateEventHandlerValidationMessageHasSinglePrefix covers NEW-2. The
// guard already returns a typed validation error, whose Error() adds the code
// prefix, so the handler context must not add a second copy.
func TestCreateEventHandlerValidationMessageHasSinglePrefix(t *testing.T) {
	svc := &icloud.MockService{CreatedUID: "timed-uid"}
	handler := createEventHandler(testDeps(svc))
	res, err := handler(t.Context(), newReq(map[string]any{
		"title": "Timed", "start": "2026-10-25T02:30:00+01:00",
		"end": "2026-10-25T04:30:00+01:00", "calendar": "/cal/home/",
		"timezone": "Europe/Paris",
	}))
	if err != nil {
		t.Fatalf("unexpected protocol error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected a validation error: %s", resultText(t, res))
	}
	var payload toolErrorPayload
	if err := json.Unmarshal([]byte(resultText(t, res)), &payload); err != nil {
		t.Fatal(err)
	}
	if strings.Count(payload.Message, "validation:") != 1 {
		t.Fatalf("message repeats the validation prefix: %q", payload.Message)
	}
	if !strings.HasPrefix(payload.Message, "validation: ") {
		t.Fatalf("message lost the leading validation prefix: %q", payload.Message)
	}
}
