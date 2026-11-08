package icloud

import (
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-ical"
	extcaldav "github.com/emersion/go-webdav/caldav"
)

const floatingEventBody = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//icloud-mcp//EN\r\n" +
	"BEGIN:VEVENT\r\nUID:floating-1\r\nSUMMARY:Floating\r\n" +
	"DTSTART:20260701T090000\r\nDTEND:20260701T100000\r\n" +
	"DTSTAMP:20260601T000000Z\r\n" +
	"RRULE:FREQ=WEEKLY;COUNT=3\r\n" +
	"EXDATE:20260715T090000\r\n" +
	"END:VEVENT\r\nEND:VCALENDAR\r\n"

func parseFloating(t *testing.T, body string) (*Event, []Event) {
	t.Helper()
	cal, err := decodeRemoteCalendar(strings.NewReader(body))
	if err != nil {
		t.Fatalf("decodeRemoteCalendar: %v", err)
	}
	return parseCalendarAndRequire(t, cal)
}

func parseCalendarAndRequire(t *testing.T, cal *ical.Calendar) (*Event, []Event) {
	t.Helper()
	master, overrides, err := parseCalendarObject(&extcaldav.CalendarObject{Data: cal})
	if err != nil {
		t.Fatalf("parseCalendarObject: %v", err)
	}
	return master, overrides
}

// TestFloatingReadKeepsWallClock pins that a floating value is not shifted by
// the process timezone. It stores the wall components and marks the form.
func TestFloatingReadKeepsWallClock(t *testing.T) {
	original := time.Local
	t.Cleanup(func() { time.Local = original })

	for _, zone := range []string{"UTC", "Europe/Paris", "America/New_York"} {
		loc, err := time.LoadLocation(zone)
		if err != nil {
			t.Fatal(err)
		}
		time.Local = loc

		master, _ := parseFloating(t, floatingEventBody)
		if !master.IsFloating {
			t.Fatalf("%s: IsFloating = false", zone)
		}
		if master.Timezone != "" {
			t.Fatalf("%s: floating event has timezone %q", zone, master.Timezone)
		}
		if got := master.StartTime.UTC().Format("15:04"); got != "09:00" {
			t.Fatalf("%s: start wall clock = %s, want 09:00", zone, got)
		}
		if got := FormatEventTimeForm(master.StartTime, false, true, loc); got != "2026-07-01T09:00:00" {
			t.Fatalf("%s: formatted start = %q", zone, got)
		}
	}
}

// TestFloatingExpansionMatchesWallClockRange proves recurrence expansion uses
// the wall clock of the range, not a fabricated UTC instant.
func TestFloatingExpansionMatchesWallClockRange(t *testing.T) {
	master, overrides := parseFloating(t, floatingEventBody)
	paris, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Fatal(err)
	}
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	for _, loc := range []*time.Location{paris, ny, time.UTC} {
		events, truncated, err := ExpandOccurrences(*master, overrides,
			time.Date(2026, time.July, 1, 8, 0, 0, 0, loc),
			time.Date(2026, time.July, 20, 10, 0, 0, 0, loc), 0)
		if err != nil {
			t.Fatalf("%s: ExpandOccurrences: %v", loc, err)
		}
		if truncated {
			t.Fatalf("%s: unexpected truncation", loc)
		}
		if len(events) != 2 {
			t.Fatalf("%s: events = %d, want 2 (EXDATE removes one)", loc, len(events))
		}
		for _, event := range events {
			if !event.IsFloating {
				t.Fatalf("%s: expanded occurrence lost the floating form", loc)
			}
			if got := event.StartTime.UTC().Format("15:04"); got != "09:00" {
				t.Fatalf("%s: occurrence wall clock = %s, want 09:00", loc, got)
			}
		}
		if events[0].StartTime.UTC().Format("2006-01-02") != "2026-07-01" {
			t.Fatalf("%s: first occurrence = %v", loc, events[0].StartTime)
		}
		if events[1].StartTime.UTC().Format("2006-01-02") != "2026-07-08" {
			t.Fatalf("%s: EXDATE did not remove only its slot: %v", loc, events[1].StartTime)
		}
	}
}

// TestFloatingRoundTripKeepsForm proves an unaffected update re-encodes the
// original floating representation instead of inventing Z or TZID.
func TestFloatingRoundTripKeepsForm(t *testing.T) {
	cal, err := decodeRemoteCalendar(strings.NewReader(floatingEventBody))
	if err != nil {
		t.Fatal(err)
	}
	master, err := findMasterVEvent(cal)
	if err != nil {
		t.Fatal(err)
	}
	title := "Renamed"
	if err := applyFieldUpdate(master, &EventUpdate{Title: &title}); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := ical.NewEncoder(&out).Encode(cal); err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(out.String(), "\r\n") {
		if strings.HasPrefix(line, "DTSTART:") || strings.HasPrefix(line, "DTEND:") || strings.HasPrefix(line, "EXDATE:") {
			if strings.Contains(line, "Z") || strings.Contains(line, "TZID") {
				t.Fatalf("floating property gained an offset: %q", line)
			}
		}
	}
	if !strings.Contains(out.String(), "DTSTART:20260701T090000") {
		t.Fatalf("floating DTSTART was not preserved:\n%s", out.String())
	}
}

// TestFloatingAvailabilityUsesRequestedZone proves free-slot calculation
// treats a floating value as the same wall clock in the requested zone.
func TestFloatingAvailabilityUsesRequestedZone(t *testing.T) {
	paris, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Fatal(err)
	}
	event := Event{
		StartTime:  time.Date(2026, time.July, 1, 9, 0, 0, 0, time.UTC),
		EndTime:    time.Date(2026, time.July, 1, 10, 0, 0, 0, time.UTC),
		IsFloating: true,
	}
	busy := BusyFromEvents([]Event{event}, true, 0, 0, paris)
	if len(busy) != 1 {
		t.Fatalf("busy intervals = %d, want 1", len(busy))
	}
	wantStart := time.Date(2026, time.July, 1, 9, 0, 0, 0, paris)
	wantEnd := time.Date(2026, time.July, 1, 10, 0, 0, 0, paris)
	if !busy[0].Start.Equal(wantStart) || !busy[0].End.Equal(wantEnd) {
		t.Fatalf("busy = %v..%v, want %v..%v", busy[0].Start, busy[0].End, wantStart, wantEnd)
	}
}

// TestDateTimeFormMismatchFailsClosed covers a timed event whose bounds mix a
// floating value with an absolute value. Such a pair has no single meaning.
func TestDateTimeFormMismatchFailsClosed(t *testing.T) {
	body := researchCalendarHead +
		"BEGIN:VEVENT\r\nUID:floating-mismatch\r\n" +
		"DTSTART:20260701T090000\r\n" +
		"DTEND:20260701T100000Z\r\n" +
		"END:VEVENT\r\nEND:VCALENDAR\r\n"
	cal, err := decodeRemoteCalendar(strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := parseCalendarObject(&extcaldav.CalendarObject{Data: cal}); err == nil {
		t.Fatal("mixed floating and absolute bounds were accepted")
	}
}

// TestUTCAndZonedAreNotFloating confirms the other forms stay unchanged.
func TestUTCAndZonedAreNotFloating(t *testing.T) {
	for _, tt := range []struct {
		name     string
		start    string
		end      string
		timezone string
	}{
		{name: "utc", start: "DTSTART:20260701T090000Z", end: "DTEND:20260701T100000Z"},
		{name: "tzid", start: "DTSTART;TZID=Europe/Paris:20260701T090000", end: "DTEND;TZID=Europe/Paris:20260701T100000", timezone: "Europe/Paris"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			body := researchCalendarHead +
				"BEGIN:VEVENT\r\nUID:not-floating\r\n" + tt.start + "\r\n" + tt.end + "\r\n" +
				"END:VEVENT\r\nEND:VCALENDAR\r\n"
			master, _ := parseFloating(t, body)
			if master.IsFloating {
				t.Fatal("absolute value was marked floating")
			}
			if master.Timezone != tt.timezone {
				t.Fatalf("timezone = %q, want %q", master.Timezone, tt.timezone)
			}
		})
	}
}
