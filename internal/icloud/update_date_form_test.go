package icloud

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-ical"
)

// allDaySeriesWithOverride is a weekly all-day series with one detached DATE
// override. Both components use the DATE form.
const allDaySeriesWithOverride = "BEGIN:VCALENDAR\r\n" +
	"VERSION:2.0\r\n" +
	"PRODID:-//test//EN\r\n" +
	"BEGIN:VEVENT\r\n" +
	"UID:uid-allday-recur-1\r\n" +
	"SUMMARY:All day series\r\n" +
	"DTSTART;VALUE=DATE:20260706\r\n" +
	"DTEND;VALUE=DATE:20260707\r\n" +
	"RRULE:FREQ=WEEKLY;COUNT=4\r\n" +
	"DTSTAMP:20260701T000000Z\r\n" +
	"END:VEVENT\r\n" +
	"BEGIN:VEVENT\r\n" +
	"UID:uid-allday-recur-1\r\n" +
	"RECURRENCE-ID;VALUE=DATE:20260713\r\n" +
	"SUMMARY:All day series (moved)\r\n" +
	"DTSTART;VALUE=DATE:20260714\r\n" +
	"DTEND;VALUE=DATE:20260715\r\n" +
	"DTSTAMP:20260701T000000Z\r\n" +
	"END:VEVENT\r\n" +
	"END:VCALENDAR\r\n"

// putBodies returns the captured PUT bodies under the mock lock.
func putBodies(m *mockCalDAV) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	bodies := make([]string, 0, len(m.puts))
	for _, put := range m.puts {
		bodies = append(bodies, put.body)
	}
	return bodies
}

// eventFromFixture decodes a fixture and returns its master VEVENT.
func eventFromFixture(t *testing.T, fixture string) *ical.Event {
	t.Helper()
	cal, err := decodeRemoteCalendar(strings.NewReader(fixture))
	if err != nil {
		t.Fatalf("decodeRemoteCalendar: %v", err)
	}
	vevent, err := findMasterVEvent(cal)
	if err != nil {
		t.Fatalf("findMasterVEvent: %v", err)
	}
	return vevent
}

func requireValidationError(t *testing.T, err error, wantFragment string) {
	t.Helper()
	if err == nil {
		t.Fatalf("update was accepted, want a typed validation error")
	}
	typed := AsICloudError(err)
	if typed == nil || typed.Code != CodeValidation {
		t.Fatalf("error = %v, want code %q", err, CodeValidation)
	}
	if wantFragment != "" && !strings.Contains(typed.Message, wantFragment) {
		t.Fatalf("error message = %q, want it to mention %q", typed.Message, wantFragment)
	}
}

// TestUpdateEventRejectsTimedInputForAllDayEvent covers the reverse type
// mismatch of #183: a DATE-TIME value for a VALUE=DATE start or end must fail
// before any conditional PUT instead of silently dropping its clock.
func TestUpdateEventRejectsTimedInputForAllDayEvent(t *testing.T) {
	paris, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Fatal(err)
	}
	timedStart := time.Date(2026, 7, 3, 23, 30, 0, 0, paris)
	timedEnd := time.Date(2026, 7, 4, 0, 30, 0, 0, paris)
	lateTimedEnd := time.Date(2026, 7, 11, 0, 30, 0, 0, paris)
	utcStart := time.Date(2026, 7, 3, 9, 0, 0, 0, time.UTC)
	dateStart := time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC)
	dateEnd := time.Date(2026, 7, 4, 0, 0, 0, 0, time.UTC)
	laterDateEnd := time.Date(2026, 7, 12, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		name    string
		ics     string
		uid     string
		update  *EventUpdate
		frag    string
		wantPUT bool
	}{
		{
			name: "timed start and end with offset", ics: icsAllDay, uid: "uid-allday-1",
			update: &EventUpdate{StartTime: &timedStart, EndTime: &timedEnd}, frag: "DATE-TIME",
		},
		{
			name: "timed start only", ics: icsAllDay, uid: "uid-allday-1",
			update: &EventUpdate{StartTime: &timedStart}, frag: "DATE-TIME",
		},
		{
			name: "timed end only", ics: icsAllDay, uid: "uid-allday-1",
			update: &EventUpdate{EndTime: &lateTimedEnd}, frag: "DATE-TIME",
		},
		{
			name: "timed start in UTC", ics: icsAllDay, uid: "uid-allday-1",
			update: &EventUpdate{StartTime: &utcStart}, frag: "DATE-TIME",
		},
		{
			name: "mixed forms start timed end date", ics: icsAllDay, uid: "uid-allday-1",
			update: &EventUpdate{StartTime: &timedStart, EndTime: &dateEnd, EndDateOnly: true}, frag: "DATE-TIME",
		},
		{
			name: "mixed forms start date end timed", ics: icsAllDay, uid: "uid-allday-1",
			update: &EventUpdate{StartTime: &dateStart, StartDateOnly: true, EndTime: &timedEnd}, frag: "DATE-TIME",
		},
		{
			name: "all day event without DTEND rejects a timed end", ics: icsAllDayNoDtend, uid: "uid-allday-nodtend-1",
			update: &EventUpdate{EndTime: &lateTimedEnd}, frag: "DATE-TIME",
		},
		{
			// The DATE update is the positive control for the two cases above.
			name: "date start keeps DATE form", ics: icsAllDay, uid: "uid-allday-1",
			update: &EventUpdate{StartTime: &dateStart, StartDateOnly: true}, wantPUT: true,
		},
		{
			name: "date start and exclusive date end keep DATE form", ics: icsAllDay, uid: "uid-allday-1",
			update: &EventUpdate{StartTime: &dateStart, StartDateOnly: true, EndTime: &dateEnd, EndDateOnly: true}, wantPUT: true,
		},
		{
			// A DATE end on an event without DTEND is inherited from DTSTART.
			name: "date end on all day event without DTEND", ics: icsAllDayNoDtend, uid: "uid-allday-nodtend-1",
			update: &EventUpdate{EndTime: &laterDateEnd, EndDateOnly: true}, wantPUT: true,
		},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			m := newMockCalDAV(t)
			path := testHomeCalendar + test.uid + ".ics"
			m.objects[test.uid] = mockObject{path: path, ics: test.ics}
			m.etags[path] = `"v1"`
			client := m.client()

			err := client.UpdateEvent(context.Background(), testHomeCalendar, test.uid, test.update)
			if test.wantPUT {
				if err != nil {
					t.Fatalf("DATE update = %v", err)
				}
				bodies := putBodies(m)
				if len(bodies) != 1 {
					t.Fatalf("puts = %d, want 1", len(bodies))
				}
				if !strings.Contains(bodies[0], "VALUE=DATE:") {
					t.Fatalf("PUT lost the DATE form:\n%s", bodies[0])
				}
				if strings.Contains(bodies[0], "T000000") || strings.Contains(bodies[0], "T233000") {
					t.Fatalf("PUT stored a clock component on an all-day event:\n%s", bodies[0])
				}
				return
			}
			requireValidationError(t, err, test.frag)
			if bodies := putBodies(m); len(bodies) != 0 {
				t.Fatalf("rejected input issued %d PUT(s):\n%s", len(bodies), bodies[0])
			}
		})
	}
}

// TestUpdateEventAllDayOccurrenceRejectsTimedInput covers scope=occurrence on
// an all-day series, including an existing detached DATE override.
func TestUpdateEventAllDayOccurrenceRejectsTimedInput(t *testing.T) {
	paris, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Fatal(err)
	}
	timedStart := time.Date(2026, 7, 20, 23, 30, 0, 0, paris)
	dateStart := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)

	for _, test := range []struct {
		name    string
		recID   time.Time
		update  *EventUpdate
		wantPUT bool
	}{
		{
			name: "generated occurrence rejects a timed start", recID: time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC),
			update: &EventUpdate{StartTime: &timedStart},
		},
		{
			name: "existing override rejects a timed start", recID: time.Date(2026, 7, 13, 0, 0, 0, 0, time.UTC),
			update: &EventUpdate{StartTime: &timedStart},
		},
		{
			name: "existing override rejects a timed end", recID: time.Date(2026, 7, 13, 0, 0, 0, 0, time.UTC),
			update: &EventUpdate{EndTime: &timedStart},
		},
		{
			name: "generated occurrence accepts a DATE start", recID: time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC),
			update: &EventUpdate{StartTime: &dateStart, StartDateOnly: true}, wantPUT: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			m := newMockCalDAV(t)
			path := testHomeCalendar + "uid-allday-recur-1.ics"
			m.objects["uid-allday-recur-1"] = mockObject{path: path, ics: allDaySeriesWithOverride}
			m.etags[path] = `"v1"`
			client := m.client()

			recID := test.recID
			up := test.update
			up.Scope = ScopeOccurrence
			up.RecurrenceID = &recID
			err := client.UpdateEvent(context.Background(), testHomeCalendar, "uid-allday-recur-1", up)
			if test.wantPUT {
				if err != nil {
					t.Fatalf("DATE occurrence update = %v", err)
				}
				bodies := putBodies(m)
				if len(bodies) != 1 {
					t.Fatalf("puts = %d, want 1", len(bodies))
				}
				if !strings.Contains(bodies[0], "VALUE=DATE:") {
					t.Fatalf("occurrence PUT lost the DATE form:\n%s", bodies[0])
				}
				return
			}
			requireValidationError(t, err, "DATE-TIME")
			if bodies := putBodies(m); len(bodies) != 0 {
				t.Fatalf("rejected input issued %d PUT(s):\n%s", len(bodies), bodies[0])
			}
		})
	}
}

// TestUpdateEventTimedEventKeepsDateOnlyRejection keeps the #183 behaviour in
// place for both bounds and does not over-reject a timed update.
func TestUpdateEventTimedEventKeepsDateOnlyRejection(t *testing.T) {
	dateOnly := time.Date(2026, 7, 9, 0, 0, 0, 0, time.UTC)
	timedStart := time.Date(2026, 7, 6, 11, 0, 0, 0, time.UTC)
	timedEnd := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)

	m := newMockCalDAV(t)
	path := testHomeCalendar + "uid-simple-1.ics"
	m.objects["uid-simple-1"] = mockObject{path: path, ics: icsSimpleEvent}
	m.etags[path] = `"v1"`
	client := m.client()

	requireValidationError(t, client.UpdateEvent(context.Background(), testHomeCalendar, "uid-simple-1",
		&EventUpdate{StartTime: &dateOnly, StartDateOnly: true}), "DATE-only")
	requireValidationError(t, client.UpdateEvent(context.Background(), testHomeCalendar, "uid-simple-1",
		&EventUpdate{EndTime: &dateOnly, EndDateOnly: true}), "DATE-only")
	if bodies := putBodies(m); len(bodies) != 0 {
		t.Fatalf("rejected DATE-only input issued %d PUT(s)", len(bodies))
	}

	if err := client.UpdateEvent(context.Background(), testHomeCalendar, "uid-simple-1",
		&EventUpdate{StartTime: &timedStart, EndTime: &timedEnd}); err != nil {
		t.Fatalf("timed update on a timed event = %v", err)
	}
	if bodies := putBodies(m); len(bodies) != 1 {
		t.Fatalf("puts = %d, want 1", len(bodies))
	}
}

// TestValidateUpdateDateFormsCarriesGuidance pins the typed guidance for the
// rejected reverse mismatch, so an agent can act without parsing prose.
func TestValidateUpdateDateFormsCarriesGuidance(t *testing.T) {
	allDay := time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)
	timed := time.Date(2026, 7, 10, 23, 30, 0, 0, time.UTC)

	vevent := eventFromFixture(t, icsAllDay)
	err := validateUpdateDateForms(vevent, &EventUpdate{StartTime: &timed})
	typed := AsICloudError(err)
	if typed == nil || typed.Code != CodeValidation {
		t.Fatalf("error = %v, want validation", err)
	}
	if typed.Details["reconciliation"] == "" {
		t.Fatalf("validation error lost its guidance: %+v", typed)
	}
	if err := validateUpdateDateForms(vevent, &EventUpdate{StartTime: &allDay, StartDateOnly: true}); err != nil {
		t.Fatalf("DATE input rejected: %v", err)
	}
}
