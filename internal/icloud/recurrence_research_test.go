package icloud

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-ical"
	extcaldav "github.com/emersion/go-webdav/caldav"
)

// This file characterizes the recurrence and invitation behavior that
// docs/decisions/0001-recurrence-and-invitations.md records. The tests use
// synthetic iCalendar fixtures. They do not claim live iCloud behavior.

const researchCalendarHead = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//icloud-mcp research//EN\r\n"

func researchParse(t *testing.T, body string) (*Event, []Event) {
	t.Helper()
	cal, err := decodeRemoteCalendar(strings.NewReader(body))
	if err != nil {
		t.Fatalf("decodeRemoteCalendar: %v", err)
	}
	master, overrides, err := parseCalendarObject(&extcaldav.CalendarObject{Data: cal})
	if err != nil {
		t.Fatalf("parseCalendarObject: %v", err)
	}
	return master, overrides
}

func researchParseError(t *testing.T, body string) error {
	t.Helper()
	cal, err := decodeRemoteCalendar(strings.NewReader(body))
	if err != nil {
		return err
	}
	_, _, err = parseCalendarObject(&extcaldav.CalendarObject{Data: cal})
	return err
}

func researchExpand(t *testing.T, body string, from, to time.Time) []Event {
	t.Helper()
	master, overrides := researchParse(t, body)
	events, truncated, err := ExpandOccurrences(*master, overrides, from, to, 0)
	if err != nil {
		t.Fatalf("ExpandOccurrences: %v", err)
	}
	if truncated {
		t.Fatal("unexpected recurrence truncation")
	}
	return events
}

// TestResearchRecurrenceDSTSpringForwardKeepsWallClock pins the documented
// wall-clock RRULE behavior over the Europe/Paris spring transition. The
// EXDATE uses the same TZID as DTSTART and must remove only its own slot.
func TestResearchRecurrenceDSTSpringForwardKeepsWallClock(t *testing.T) {
	body := researchCalendarHead +
		"BEGIN:VEVENT\r\nUID:research-dst-spring\r\n" +
		"DTSTART;TZID=Europe/Paris:20260321T100000\r\n" +
		"DTEND;TZID=Europe/Paris:20260321T110000\r\n" +
		"RRULE:FREQ=WEEKLY;COUNT=4\r\n" +
		"EXDATE;TZID=Europe/Paris:20260328T100000\r\n" +
		"END:VEVENT\r\nEND:VCALENDAR\r\n"

	paris, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Fatal(err)
	}
	events := researchExpand(t, body,
		time.Date(2026, time.March, 20, 0, 0, 0, 0, time.UTC),
		time.Date(2026, time.April, 20, 0, 0, 0, 0, time.UTC))

	wantStarts := []time.Time{
		time.Date(2026, time.March, 21, 10, 0, 0, 0, paris),
		time.Date(2026, time.April, 4, 10, 0, 0, 0, paris),
		time.Date(2026, time.April, 11, 10, 0, 0, 0, paris),
	}
	if len(events) != len(wantStarts) {
		t.Fatalf("events = %d, want %d: %+v", len(events), len(wantStarts), events)
	}
	for i, want := range wantStarts {
		if !events[i].StartTime.Equal(want) {
			t.Errorf("event %d start = %v, want %v", i, events[i].StartTime, want)
		}
		if got := events[i].StartTime.In(paris).Hour(); got != 10 {
			t.Errorf("event %d Paris wall hour = %d, want 10", i, got)
		}
		if events[i].StartTime.Equal(time.Date(2026, time.March, 28, 10, 0, 0, 0, paris)) {
			t.Errorf("EXDATE slot 2026-03-28 10:00 Paris is present")
		}
	}
	if _, offset := events[0].StartTime.In(paris).Zone(); offset != 1*60*60 {
		t.Errorf("first offset = %d, want +01:00 before DST", offset)
	}
	for i := 1; i < len(events); i++ {
		if _, offset := events[i].StartTime.In(paris).Zone(); offset != 2*60*60 {
			t.Errorf("event %d offset = %d, want +02:00 after DST", i, offset)
		}
	}
}

// TestResearchRecurrenceDSTFallBackKeepsWallClock covers the autumn transition
// in America/New_York. A fixed UTC offset would move the series by one hour.
func TestResearchRecurrenceDSTFallBackKeepsWallClock(t *testing.T) {
	body := researchCalendarHead +
		"BEGIN:VEVENT\r\nUID:research-dst-fall\r\n" +
		"DTSTART;TZID=America/New_York:20261026T100000\r\n" +
		"DTEND;TZID=America/New_York:20261026T110000\r\n" +
		"RRULE:FREQ=WEEKLY;COUNT=3\r\n" +
		"END:VEVENT\r\nEND:VCALENDAR\r\n"

	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	events := researchExpand(t, body,
		time.Date(2026, time.October, 25, 0, 0, 0, 0, time.UTC),
		time.Date(2026, time.November, 15, 0, 0, 0, 0, time.UTC))
	if len(events) != 3 {
		t.Fatalf("events = %d, want 3: %+v", len(events), events)
	}
	wantOffsets := []int{-4 * 60 * 60, -5 * 60 * 60, -5 * 60 * 60}
	for i, event := range events {
		if got := event.StartTime.In(ny).Hour(); got != 10 {
			t.Errorf("event %d New York wall hour = %d, want 10", i, got)
		}
		if _, offset := event.StartTime.In(ny).Zone(); offset != wantOffsets[i] {
			t.Errorf("event %d offset = %d, want %d", i, offset, wantOffsets[i])
		}
	}
}

// TestResearchRecurrenceAllDayExpansionUsesDateBounds covers DATE values with
// an EXDATE. Each occurrence keeps the one civil day convention.
func TestResearchRecurrenceAllDayExpansionUsesDateBounds(t *testing.T) {
	body := researchCalendarHead +
		"BEGIN:VEVENT\r\nUID:research-all-day\r\n" +
		"DTSTART;VALUE=DATE:20260328\r\n" +
		"DTEND;VALUE=DATE:20260329\r\n" +
		"RRULE:FREQ=DAILY;COUNT=4\r\n" +
		"EXDATE;VALUE=DATE:20260329\r\n" +
		"END:VEVENT\r\nEND:VCALENDAR\r\n"

	events := researchExpand(t, body,
		time.Date(2026, time.March, 27, 0, 0, 0, 0, time.UTC),
		time.Date(2026, time.April, 2, 0, 0, 0, 0, time.UTC))
	wantDays := []int{28, 30, 31}
	if len(events) != len(wantDays) {
		t.Fatalf("events = %d, want %d: %+v", len(events), len(wantDays), events)
	}
	for i, day := range wantDays {
		if !events[i].AllDay {
			t.Errorf("event %d is not all-day", i)
		}
		if events[i].StartTime.Day() != day || events[i].StartTime.Hour() != 0 {
			t.Errorf("event %d start = %v, want 2026-03-%02d 00:00", i, events[i].StartTime, day)
		}
		if got := events[i].EndTime.Sub(events[i].StartTime); got != 24*time.Hour {
			t.Errorf("event %d duration = %v, want 24h", i, got)
		}
	}
}

// TestResearchRecurrenceConflictingOverridesFailClosed covers identity shapes
// that cannot be expanded unambiguously.
func TestResearchRecurrenceConflictingOverridesFailClosed(t *testing.T) {
	master := "BEGIN:VEVENT\r\nUID:research-conflict\r\n" +
		"DTSTART:20260701T100000Z\r\nDTEND:20260701T110000Z\r\n" +
		"RRULE:FREQ=DAILY;COUNT=3\r\nEND:VEVENT\r\n"
	override := func(recurrenceID, start string) string {
		return "BEGIN:VEVENT\r\nUID:research-conflict\r\nRECURRENCE-ID" + recurrenceID + "\r\n" +
			"DTSTART:" + start + "\r\nDTEND:20260702T130000Z\r\nEND:VEVENT\r\n"
	}
	tests := []struct {
		name string
		body string
	}{
		{
			name: "duplicate recurrence identifier",
			body: master +
				override(":20260702T100000Z", "20260702T120000Z") +
				override(":20260702T100000Z", "20260702T140000Z"),
		},
		{
			name: "override carries its own recurring rule",
			body: master +
				"BEGIN:VEVENT\r\nUID:research-conflict\r\nRECURRENCE-ID:20260702T100000Z\r\n" +
				"DTSTART:20260702T120000Z\r\nDTEND:20260702T130000Z\r\n" +
				"RRULE:FREQ=DAILY;COUNT=2\r\nEND:VEVENT\r\n",
		},
		{
			name: "override without recurring master",
			body: strings.Replace(master, "RRULE:FREQ=DAILY;COUNT=3\r\n", "", 1) +
				override(":20260702T100000Z", "20260702T120000Z"),
		},
		{
			name: "different UID in the same object",
			body: master +
				"BEGIN:VEVENT\r\nUID:research-other\r\nRECURRENCE-ID:20260702T100000Z\r\n" +
				"DTSTART:20260702T120000Z\r\nDTEND:20260702T130000Z\r\nEND:VEVENT\r\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := researchParseError(t, researchCalendarHead+test.body+"END:VCALENDAR\r\n")
			requireICloudCode(t, err, CodeProtocolError)
		})
	}
}

// TestResearchRecurrenceMixedRDATEAndEXDATEFailsClosed proves that EXDATE alone
// is applied, and that adding RDATE in any value form rejects the whole object
// before expansion. No partial result is presented.
func TestResearchRecurrenceMixedRDATEAndEXDATEFailsClosed(t *testing.T) {
	base := "BEGIN:VEVENT\r\nUID:research-rdate\r\n" +
		"DTSTART:20260701T100000Z\r\nDTEND:20260701T110000Z\r\n" +
		"RRULE:FREQ=DAILY;COUNT=4\r\n" +
		"EXDATE:20260702T100000Z\r\n"

	baseline := researchCalendarHead + base + "END:VEVENT\r\nEND:VCALENDAR\r\n"
	events := researchExpand(t, baseline,
		time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, time.July, 10, 0, 0, 0, 0, time.UTC))
	if len(events) != 3 {
		t.Fatalf("EXDATE baseline events = %d, want 3", len(events))
	}
	for _, event := range events {
		if event.StartTime.Equal(time.Date(2026, time.July, 2, 10, 0, 0, 0, time.UTC)) {
			t.Error("EXDATE slot 2026-07-02 10:00Z is present")
		}
	}

	for _, rdate := range []string{
		"RDATE:20260705T100000Z\r\n",
		"RDATE;VALUE=DATE:20260705\r\n",
		"RDATE;VALUE=PERIOD:20260705T100000Z/20260705T120000Z\r\n",
		"RDATE;TZID=Europe/Paris:20260705T120000\r\n",
	} {
		t.Run(rdate, func(t *testing.T) {
			err := researchParseError(t, researchCalendarHead+base+rdate+"END:VEVENT\r\nEND:VCALENDAR\r\n")
			requireICloudCode(t, err, CodeProtocolError)
			if err == nil || !strings.Contains(err.Error(), "RDATE") {
				t.Fatalf("error = %v, want an RDATE rejection", err)
			}
		})
	}
}

// TestResearchRecurrenceThisAndFutureFailsClosed covers ranged overrides in
// UTC, TZID, DATE, and lower-case parameter spelling. The client never applies
// a range to later occurrences.
func TestResearchRecurrenceThisAndFutureFailsClosed(t *testing.T) {
	tests := []struct {
		name   string
		master string
		rangee string
	}{
		{
			name: "UTC",
			master: "DTSTART:20260701T100000Z\r\nDTEND:20260701T110000Z\r\n" +
				"RRULE:FREQ=DAILY;COUNT=4\r\n",
			rangee: "RECURRENCE-ID;RANGE=THISANDFUTURE:20260702T100000Z\r\n" +
				"DTSTART:20260702T120000Z\r\nDTEND:20260702T130000Z\r\n",
		},
		{
			name: "TZID",
			master: "DTSTART;TZID=America/New_York:20261025T100000\r\n" +
				"DTEND;TZID=America/New_York:20261025T110000\r\n" +
				"RRULE:FREQ=WEEKLY;COUNT=4\r\n",
			rangee: "RECURRENCE-ID;TZID=America/New_York;RANGE=THISANDFUTURE:20261101T100000\r\n" +
				"DTSTART;TZID=America/New_York:20261101T120000\r\n" +
				"DTEND;TZID=America/New_York:20261101T130000\r\n",
		},
		{
			name: "all day",
			master: "DTSTART;VALUE=DATE:20260328\r\nDTEND;VALUE=DATE:20260329\r\n" +
				"RRULE:FREQ=DAILY;COUNT=4\r\n",
			rangee: "RECURRENCE-ID;VALUE=DATE;RANGE=THISANDFUTURE:20260330\r\n" +
				"DTSTART;VALUE=DATE:20260331\r\nDTEND;VALUE=DATE:20260401\r\n",
		},
		{
			name: "lower-case parameter",
			master: "DTSTART:20260701T100000Z\r\nDTEND:20260701T110000Z\r\n" +
				"RRULE:FREQ=DAILY;COUNT=4\r\n",
			rangee: "recurrence-id;range=thisandfuture:20260702T100000Z\r\n" +
				"DTSTART:20260702T120000Z\r\nDTEND:20260702T130000Z\r\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := researchCalendarHead + "BEGIN:VEVENT\r\nUID:research-range\r\n" +
				test.master + "END:VEVENT\r\n" +
				"BEGIN:VEVENT\r\nUID:research-range\r\n" + test.rangee + "END:VEVENT\r\n" +
				"END:VCALENDAR\r\n"
			err := researchParseError(t, body)
			requireICloudCode(t, err, CodeProtocolError)
			if err == nil || !strings.Contains(err.Error(), "ranged recurrence") {
				t.Fatalf("error = %v, want a ranged-recurrence rejection", err)
			}
		})
	}
}

func researchInvitedFixture(uid string) string {
	return researchCalendarHead +
		"BEGIN:VEVENT\r\nUID:" + uid + "\r\n" +
		"SUMMARY:Research invited series\r\n" +
		"DTSTART:20260701T100000Z\r\nDTEND:20260701T110000Z\r\n" +
		"RRULE:FREQ=DAILY;COUNT=3\r\n" +
		"SEQUENCE:4\r\n" +
		"DTSTAMP:20260601T000000Z\r\n" +
		"ORGANIZER;CN=Owner:mailto:owner@example.com\r\n" +
		"ATTENDEE;CN=Guest;ROLE=REQ-PARTICIPANT;PARTSTAT=ACCEPTED:mailto:guest@example.com\r\n" +
		"X-RESEARCH-KEEP:opaque\r\n" +
		"END:VEVENT\r\nEND:VCALENDAR\r\n"
}

func researchPut(t *testing.T, m *mockCalDAV) mockPut {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.puts) != 1 {
		t.Fatalf("puts = %d, want 1", len(m.puts))
	}
	if len(m.deletes) != 0 {
		t.Fatalf("deletes = %d, want 0", len(m.deletes))
	}
	return m.puts[0]
}

func researchDecodedPut(t *testing.T, put mockPut) *ical.Calendar {
	t.Helper()
	cal, err := ical.NewDecoder(strings.NewReader(put.body)).Decode()
	if err != nil {
		t.Fatalf("decode PUT body: %v", err)
	}
	return cal
}

// TestResearchRecurrenceOccurrenceUpdatePreservesMasterAndDropsSchedulingOnOverride
// pins the partial-series edit boundary: the master keeps every unknown and
// scheduling property, and a new override copies only the allowlist. The
// dropped ATTENDEE/ORGANIZER on the new override is deliberate (see the
// decision record) and avoids invitation churn.
func TestResearchRecurrenceOccurrenceUpdatePreservesMasterAndDropsSchedulingOnOverride(t *testing.T) {
	uid := "research-occurrence-update"
	path := testHomeCalendar + uid + ".ics"
	m := newMockCalDAV(t)
	m.objects[uid] = mockObject{path: path, ics: researchInvitedFixture(uid)}
	m.etags[path] = `"v1"`

	title := "Occurrence changed"
	recID := time.Date(2026, time.July, 2, 10, 0, 0, 0, time.UTC)
	if err := m.client().UpdateEvent(context.Background(), testHomeCalendar, uid, &EventUpdate{
		Title:        &title,
		Scope:        ScopeOccurrence,
		RecurrenceID: &recID,
	}); err != nil {
		t.Fatalf("UpdateEvent: %v", err)
	}

	put := researchPut(t, m)
	if put.ifMatch != `"v1"` {
		t.Errorf("If-Match = %q, want a strong ETag", put.ifMatch)
	}
	cal := researchDecodedPut(t, put)
	var masters, overrides int
	for _, child := range cal.Children {
		if child.Name != ical.CompEvent {
			continue
		}
		if child.Props.Get(ical.PropRecurrenceID) == nil {
			masters++
			if child.Props.Get(ical.PropRecurrenceRule) == nil {
				t.Error("master lost RRULE")
			}
			for _, name := range []string{ical.PropOrganizer, ical.PropAttendee, "X-RESEARCH-KEEP"} {
				if child.Props.Get(name) == nil {
					t.Errorf("master lost %s", name)
				}
			}
			if got := child.Props.Get(ical.PropSequence); got == nil || got.Value != "5" {
				t.Errorf("master SEQUENCE = %+v, want 5", got)
			}
			continue
		}
		overrides++
		if got := child.Props.Get(ical.PropSummary); got == nil || got.Value != title {
			t.Errorf("override SUMMARY = %+v, want %q", got, title)
		}
		if child.Props.Get(ical.PropRecurrenceRule) != nil {
			t.Error("override gained RRULE")
		}
		for _, name := range []string{ical.PropOrganizer, ical.PropAttendee, "X-RESEARCH-KEEP"} {
			if child.Props.Get(name) != nil {
				t.Errorf("new override unexpectedly copied %s", name)
			}
		}
	}
	if masters != 1 || overrides != 1 {
		t.Fatalf("masters=%d overrides=%d, want 1 and 1", masters, overrides)
	}
}

// TestResearchRecurrenceOccurrenceDeleteAddsExdateWithoutSeriesDelete pins the
// partial-series delete: an EXDATE on the master, no HTTP DELETE, and the
// scheduling and unknown properties stay on the master.
func TestResearchRecurrenceOccurrenceDeleteAddsExdateWithoutSeriesDelete(t *testing.T) {
	uid := "research-occurrence-delete"
	path := testHomeCalendar + uid + ".ics"
	m := newMockCalDAV(t)
	m.objects[uid] = mockObject{path: path, ics: researchInvitedFixture(uid)}
	m.etags[path] = `"v1"`

	recID := time.Date(2026, time.July, 2, 10, 0, 0, 0, time.UTC)
	if _, err := m.client().DeleteEvent(context.Background(), testHomeCalendar, uid, &DeleteOptions{
		Scope:        ScopeOccurrence,
		RecurrenceID: &recID,
	}); err != nil {
		t.Fatalf("DeleteEvent: %v", err)
	}

	put := researchPut(t, m)
	cal := researchDecodedPut(t, put)
	master, err := findMasterVEvent(cal)
	if err != nil {
		t.Fatal(err)
	}
	if master.Props.Get(ical.PropRecurrenceRule) == nil {
		t.Error("master lost RRULE")
	}
	for _, name := range []string{ical.PropOrganizer, ical.PropAttendee, "X-RESEARCH-KEEP"} {
		if master.Props.Get(name) == nil {
			t.Errorf("master lost %s", name)
		}
	}
	var exdates []string
	for _, prop := range master.Props[ical.PropExceptionDates] {
		exdates = append(exdates, prop.Value)
	}
	if len(exdates) != 1 || exdates[0] != "20260702T100000Z" {
		t.Fatalf("EXDATE = %v, want [20260702T100000Z]", exdates)
	}
}

// TestResearchInvitationsSeriesUpdatePreservesSchedulingAndUnknownProperties
// proves that a series edit is a full-object re-PUT: ORGANIZER, ATTENDEE, and
// unknown properties survive. The record notes that a scheduling-capable server
// may notify attendees for such a PUT. That live behavior is unverified.
func TestResearchInvitationsSeriesUpdatePreservesSchedulingAndUnknownProperties(t *testing.T) {
	uid := "research-invited-series"
	path := testHomeCalendar + uid + ".ics"
	m := newMockCalDAV(t)
	m.objects[uid] = mockObject{path: path, ics: researchInvitedFixture(uid)}
	m.etags[path] = `"v1"`

	title := "Series renamed"
	if err := m.client().UpdateEvent(context.Background(), testHomeCalendar, uid, &EventUpdate{
		Title: &title,
	}); err != nil {
		t.Fatalf("UpdateEvent: %v", err)
	}

	put := researchPut(t, m)
	cal := researchDecodedPut(t, put)
	master, err := findMasterVEvent(cal)
	if err != nil {
		t.Fatal(err)
	}
	if got := master.Props.Get(ical.PropSummary); got == nil || got.Value != title {
		t.Errorf("SUMMARY = %+v, want %q", got, title)
	}
	for _, name := range []string{
		ical.PropOrganizer, ical.PropAttendee, "X-RESEARCH-KEEP", ical.PropRecurrenceRule,
	} {
		if master.Props.Get(name) == nil {
			t.Errorf("master lost %s", name)
		}
	}
	sequence := master.Props.Get(ical.PropSequence)
	if sequence == nil || sequence.Value != "5" {
		t.Errorf("SEQUENCE = %+v, want 5", sequence)
	}
	if attendee := master.Props.Get(ical.PropAttendee); attendee == nil ||
		attendee.Params.Get(ical.ParamParticipationStatus) != "ACCEPTED" {
		t.Errorf("ATTENDEE PARTSTAT was not preserved: %+v", attendee)
	}
}

// TestResearchRecurrenceUnknownTZIDFailsClosed covers the read and write sides
// of an unresolvable TZID. No wall time is reinterpreted as UTC.
func TestResearchRecurrenceUnknownTZIDFailsClosed(t *testing.T) {
	body := researchCalendarHead +
		"BEGIN:VEVENT\r\nUID:research-unknown-tz\r\n" +
		"DTSTART;TZID=Custom/Nowhere:20260701T100000\r\n" +
		"DTEND;TZID=Custom/Nowhere:20260701T110000\r\n" +
		"END:VEVENT\r\nEND:VCALENDAR\r\n"
	requireICloudCode(t, researchParseError(t, body), CodeProtocolError)

	vevent := ical.NewEvent()
	prop := ical.NewProp(ical.PropDateTimeStart)
	prop.Value = "20260701T100000"
	prop.Params.Set(ical.PropTimezoneID, "Custom/Nowhere")
	vevent.Props.Set(prop)
	if err := setEventDateProp(vevent, ical.PropDateTimeStart,
		time.Date(2026, time.July, 2, 10, 0, 0, 0, time.UTC)); err == nil {
		t.Fatal("unknown TZID write was accepted")
	} else {
		requireICloudCode(t, err, CodeProtocolError)
	}
}

// TestResearchRecurrenceFloatingWriteKeepsFloatingForm covers an existing
// floating DTSTART during update. The write path preserves the absence of both
// TZID and Z. The read path still interprets a floating value as UTC; the
// decision record records that limitation.
func TestResearchRecurrenceFloatingWriteKeepsFloatingForm(t *testing.T) {
	prop := ical.NewProp(ical.PropDateTimeStart)
	prop.Value = "20260701T100000"
	vevent := ical.NewEvent()
	vevent.Props.Set(prop)

	if err := setEventDateProp(vevent, ical.PropDateTimeStart,
		time.Date(2026, time.July, 2, 10, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("setEventDateProp: %v", err)
	}
	got := vevent.Props.Get(ical.PropDateTimeStart)
	if got == nil || got.Value != "20260702T100000" {
		t.Fatalf("floating DTSTART = %+v, want 20260702T100000", got)
	}
	if tzid := got.Params.Get(ical.PropTimezoneID); tzid != "" {
		t.Errorf("floating DTSTART gained TZID %q", tzid)
	}
	if strings.HasSuffix(got.Value, "Z") {
		t.Errorf("floating DTSTART gained a Z suffix: %q", got.Value)
	}
}
