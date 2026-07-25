package icloud

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-ical"
)

func TestResolvePathOnBase_RejectsHostRewrite(t *testing.T) {
	base := "https://p46-caldav.icloud.com"
	if _, err := resolvePathOnBase(base, "//evil.example.com/x"); err == nil {
		t.Fatal("expected scheme-relative path rejected")
	}
	got, err := resolvePathOnBase(base, "/121234567/calendars/home/")
	if err != nil {
		t.Fatalf("valid path: %v", err)
	}
	if !strings.HasPrefix(got, base+"/") {
		t.Fatalf("got %q, want under %s", got, base)
	}
}

func TestSetEventDateProp_PreservesTZID(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	ev := ical.NewEvent()
	start := time.Date(2026, 11, 2, 10, 0, 0, 0, loc)
	ev.Props.SetDateTime(ical.PropDateTimeStart, start)

	newStart := time.Date(2026, 11, 2, 11, 0, 0, 0, time.UTC) // 06:00 NY (EST after DST)
	if err := setEventDateProp(ev, ical.PropDateTimeStart, newStart); err != nil {
		t.Fatal(err)
	}

	p := ev.Props.Get(ical.PropDateTimeStart)
	if p == nil {
		t.Fatal("missing DTSTART")
	}
	if p.Params.Get(ical.PropTimezoneID) != "America/New_York" {
		t.Fatalf("TZID lost: %+v value=%q", p.Params, p.Value)
	}
	if strings.HasSuffix(p.Value, "Z") {
		t.Fatalf("expected local form, got Z: %q", p.Value)
	}
	// 11:00 UTC on Nov 2 2026 is 06:00 America/New_York (EST).
	if !strings.Contains(p.Value, "T060000") {
		t.Fatalf("wall clock = %q, want ...T060000", p.Value)
	}
}

func TestSetEventDateProp_PreservesAllDay(t *testing.T) {
	ev := ical.NewEvent()
	ev.Props.SetDate(ical.PropDateTimeStart, time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC))
	if err := setEventDateProp(ev, ical.PropDateTimeStart, time.Date(2026, 7, 9, 15, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	p := ev.Props.Get(ical.PropDateTimeStart)
	if p == nil || len(p.Value) != 8 {
		t.Fatalf("expected DATE, got %+v", p)
	}
	if p.Value != "20260709" {
		t.Fatalf("got %q", p.Value)
	}
}

func TestSetRecurrenceInstantProp_MatchesMasterForm(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	master := ical.NewEvent()
	master.Props.SetDateTime(ical.PropDateTimeStart, time.Date(2026, 11, 2, 10, 0, 0, 0, loc))

	rid := time.Date(2026, 11, 9, 15, 0, 0, 0, time.UTC) // 10:00 NY
	p, err := setRecurrenceInstantProp(ical.PropRecurrenceID, master, rid)
	if err != nil {
		t.Fatal(err)
	}
	if p.Params.Get(ical.PropTimezoneID) != "America/New_York" {
		t.Fatalf("TZID: %+v value=%q", p.Params, p.Value)
	}
	if strings.HasSuffix(p.Value, "Z") {
		t.Fatalf("unexpected Z form: %q", p.Value)
	}

	allDay := ical.NewEvent()
	allDay.Props.SetDate(ical.PropDateTimeStart, time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC))
	ex, err := setRecurrenceInstantProp(ical.PropExceptionDates, allDay, time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(ex.Value) != 8 || ex.Value != "20260717" {
		t.Fatalf("all-day EXDATE = %q", ex.Value)
	}
}

func TestClient_UpdateEvent_PreservesTZIDOnStartChange(t *testing.T) {
	m := newMockCalDAV(t)
	objPath := testHomeCalendar + "uid-tz-1.ics"
	m.objects["uid-tz-1"] = mockObject{path: objPath, ics: icsFullGetVersion, getIcs: icsFullGetVersion}
	c := m.client()

	// Original is 10:00-11:00 NY; move both bounds one hour later, keep TZID form.
	newStart := time.Date(2026, 11, 2, 16, 0, 0, 0, time.UTC) // 11:00 NY EST
	newEnd := time.Date(2026, 11, 2, 17, 0, 0, 0, time.UTC)   // 12:00 NY EST
	if err := c.UpdateEvent(context.Background(), testHomeCalendar, "uid-tz-1", &EventUpdate{
		StartTime: &newStart,
		EndTime:   &newEnd,
	}); err != nil {
		t.Fatalf("UpdateEvent: %v", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.puts) != 1 {
		t.Fatalf("puts=%d", len(m.puts))
	}
	body := m.puts[0].body
	if !strings.Contains(body, "DTSTART;TZID=America/New_York:20261102T110000") {
		t.Fatalf("expected TZID DTSTART preserved, body:\n%s", body)
	}
	if !strings.Contains(body, "DTEND;TZID=America/New_York:20261102T120000") {
		t.Fatalf("expected TZID DTEND preserved, body:\n%s", body)
	}
	if strings.Contains(body, "DTSTART:20261102T160000Z") {
		t.Fatalf("DTSTART was forced to Z:\n%s", body)
	}
}

func TestClient_UpdateEvent_ImportedUID_ReGETsAfterREPORT(t *testing.T) {
	m := newMockCalDAV(t)
	objPath := testHomeCalendar + "imported-name.ics"
	m.objects["uid-tz-1"] = mockObject{
		path:   objPath,
		ics:    icsFilteredReportOnly,
		getIcs: icsFullGetVersion,
	}
	c := m.client()

	newTitle := "after full get"
	if err := c.UpdateEvent(context.Background(), testHomeCalendar, "uid-tz-1", &EventUpdate{Title: &newTitle}); err != nil {
		t.Fatalf("UpdateEvent: %v", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// Direct GET on <uid>.ics 404s, REPORT finds path, then GET on imported path.
	if len(m.gets) < 2 {
		t.Fatalf("expected >=2 GETs (direct miss + re-GET), got %v", m.gets)
	}
	foundImportedGET := false
	for _, g := range m.gets {
		if g == objPath {
			foundImportedGET = true
		}
	}
	if !foundImportedGET {
		t.Fatalf("expected GET on imported path %q, got %v", objPath, m.gets)
	}
	if len(m.puts) != 1 {
		t.Fatalf("puts=%d", len(m.puts))
	}
	for _, want := range []string{"VERSION:2.0", "BEGIN:VTIMEZONE", "SUMMARY:after full get"} {
		if !strings.Contains(m.puts[0].body, want) {
			t.Errorf("PUT missing %q:\n%s", want, m.puts[0].body)
		}
	}
}

func TestClient_DeleteOccurrence_EXDATEMatchesAllDay(t *testing.T) {
	m := newMockCalDAV(t)
	// Build a simple all-day weekly series.
	ics := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//test//EN\r\nBEGIN:VEVENT\r\n" +
		"UID:uid-allday-recur\r\nSUMMARY:All day series\r\n" +
		"DTSTART;VALUE=DATE:20260710\r\nDTEND;VALUE=DATE:20260711\r\n" +
		"RRULE:FREQ=WEEKLY;COUNT=4\r\nDTSTAMP:20260701T000000Z\r\n" +
		"END:VEVENT\r\nEND:VCALENDAR\r\n"
	path := testHomeCalendar + "uid-allday-recur.ics"
	m.objects["uid-allday-recur"] = mockObject{path: path, ics: ics}
	c := m.client()

	recID := time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC)
	_, err := c.DeleteEvent(context.Background(), testHomeCalendar, "uid-allday-recur", &DeleteOptions{
		Scope:        ScopeOccurrence,
		RecurrenceID: &recID,
	})
	if err != nil {
		t.Fatalf("DeleteEvent occurrence: %v", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.puts) != 1 {
		t.Fatalf("puts=%d", len(m.puts))
	}
	body := m.puts[0].body
	if !strings.Contains(body, "EXDATE") || !strings.Contains(body, "20260717") {
		t.Fatalf("expected DATE EXDATE, body:\n%s", body)
	}
	if strings.Contains(body, "EXDATE:20260717T000000Z") {
		t.Fatalf("EXDATE forced to Z datetime:\n%s", body)
	}
}

func TestClient_UpdateEvent_OccurrenceRequiresRRULE(t *testing.T) {
	m := newMockCalDAV(t)
	path := testHomeCalendar + "uid-simple-1.ics"
	m.objects["uid-simple-1"] = mockObject{path: path, ics: icsSimpleEvent}
	c := m.client()
	recID := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	title := "x"
	err := c.UpdateEvent(context.Background(), testHomeCalendar, "uid-simple-1", &EventUpdate{
		Title:        &title,
		Scope:        ScopeOccurrence,
		RecurrenceID: &recID,
	})
	if err == nil {
		t.Fatal("expected validation error")
	}
	if !strings.Contains(err.Error(), "RRULE") {
		t.Fatalf("got %v", err)
	}
}
