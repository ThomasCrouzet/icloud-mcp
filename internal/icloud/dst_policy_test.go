package icloud

import (
	"testing"
	"time"
)

func mustLoadLocation(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

// TestResolveWallClockGapAndFold pins the RFC 5545 section 3.3.5 policy: a
// nonexistent spring time uses the pre-gap offset (moves forward), and an
// ambiguous autumn time uses the first occurrence.
func TestResolveWallClockGapAndFold(t *testing.T) {
	ny := mustLoadLocation(t, "America/New_York")

	gapWall := time.Date(2026, time.March, 8, 2, 30, 0, 0, time.UTC)
	gap, adjusted := ResolveWallClock(gapWall, ny)
	if !adjusted {
		t.Fatal("spring gap was not reported")
	}
	if got := gap.In(ny).Format("15:04"); got != "03:30" {
		t.Fatalf("gap resolves to %s, want 03:30", got)
	}
	if _, offset := gap.In(ny).Zone(); offset != -4*60*60 {
		t.Fatalf("gap offset = %d, want -04:00", offset)
	}

	foldWall := time.Date(2026, time.November, 1, 1, 30, 0, 0, time.UTC)
	fold, adjusted := ResolveWallClock(foldWall, ny)
	if adjusted {
		t.Fatal("fold was reported as a gap")
	}
	if got := fold.In(ny).Format("15:04"); got != "01:30" {
		t.Fatalf("fold resolves to %s, want 01:30", got)
	}
	if _, offset := fold.In(ny).Zone(); offset != -4*60*60 {
		t.Fatalf("fold chose offset %d, want the first occurrence (-04:00)", offset)
	}

	paris := mustLoadLocation(t, "Europe/Paris")
	parisGap, adjusted := ResolveWallClock(time.Date(2026, time.March, 29, 2, 30, 0, 0, time.UTC), paris)
	if !adjusted || parisGap.In(paris).Hour() != 3 || parisGap.In(paris).Minute() != 30 {
		t.Fatalf("Paris gap = %v adjusted=%v, want 03:30", parisGap, adjusted)
	}

	// A civil midnight transition must follow the same rule.
	santiago := mustLoadLocation(t, "America/Santiago")
	midnight, adjusted := ResolveWallClock(time.Date(2026, time.September, 6, 0, 0, 0, 0, time.UTC), santiago)
	if !adjusted {
		t.Fatalf("Santiago midnight gap was not reported: %v", midnight)
	}
	if midnight.In(santiago).Day() != 6 || midnight.In(santiago).Hour() != 1 {
		t.Fatalf("Santiago gap = %v, want 01:00 on the same day", midnight.In(santiago))
	}
}

// TestRecurringGapIsExplicit proves a gap occurrence is resolved forward and
// marked, never returned as a silently shifted ordinary occurrence.
func TestRecurringGapIsExplicit(t *testing.T) {
	body := researchCalendarHead +
		"BEGIN:VEVENT\r\nUID:gap-series\r\n" +
		"DTSTART;TZID=America/New_York:20260301T023000\r\n" +
		"DTEND;TZID=America/New_York:20260301T033000\r\n" +
		"RRULE:FREQ=WEEKLY;COUNT=4\r\n" +
		"END:VEVENT\r\nEND:VCALENDAR\r\n"

	ny := mustLoadLocation(t, "America/New_York")
	events := researchExpand(t, body,
		time.Date(2026, time.March, 1, 0, 0, 0, 0, ny),
		time.Date(2026, time.April, 1, 0, 0, 0, 0, ny))
	if len(events) != 4 {
		t.Fatalf("events = %d, want 4: %+v", len(events), events)
	}
	want := []struct {
		day      int
		hour     int
		adjusted bool
	}{
		{1, 2, false},
		{8, 3, true},
		{15, 2, false},
		{22, 2, false},
	}
	for i, wantEvent := range want {
		local := events[i].StartTime.In(ny)
		if local.Day() != wantEvent.day || local.Hour() != wantEvent.hour || local.Minute() != 30 {
			t.Errorf("event %d = %v, want day %d %02d:30", i, local, wantEvent.day, wantEvent.hour)
		}
		if events[i].DSTAdjusted != wantEvent.adjusted {
			t.Errorf("event %d DSTAdjusted = %v, want %v", i, events[i].DSTAdjusted, wantEvent.adjusted)
		}
		if wantEvent.adjusted && local.Hour() == 1 {
			t.Errorf("gap occurrence moved backward to %v", local)
		}
	}
}

// TestRecurringDTSTARTInGapKeepsSeriesWallClock covers a master whose own
// DTSTART is in the gap. The first instance moves forward by policy and later
// instances return to the intended wall clock.
func TestRecurringDTSTARTInGapKeepsSeriesWallClock(t *testing.T) {
	body := researchCalendarHead +
		"BEGIN:VEVENT\r\nUID:gap-start\r\n" +
		"DTSTART;TZID=America/New_York:20260308T023000\r\n" +
		"DTEND;TZID=America/New_York:20260308T033000\r\n" +
		"RRULE:FREQ=WEEKLY;COUNT=2\r\n" +
		"END:VEVENT\r\nEND:VCALENDAR\r\n"

	master, overrides := researchParse(t, body)
	if !master.DSTAdjusted {
		t.Fatal("DTSTART in the gap was not marked")
	}
	ny := mustLoadLocation(t, "America/New_York")
	events, _, err := ExpandOccurrences(*master, overrides,
		time.Date(2026, time.March, 8, 0, 0, 0, 0, ny),
		time.Date(2026, time.March, 20, 0, 0, 0, 0, ny), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("events = %d, want 2", len(events))
	}
	if got := events[0].StartTime.In(ny); got.Hour() != 3 || !events[0].DSTAdjusted {
		t.Errorf("first instance = %v adjusted=%v, want 03:30 adjusted", got, events[0].DSTAdjusted)
	}
	if got := events[1].StartTime.In(ny); got.Hour() != 2 || got.Day() != 15 || events[1].DSTAdjusted {
		t.Errorf("second instance = %v adjusted=%v, want Mar 15 02:30 ordinary", got, events[1].DSTAdjusted)
	}
}

// TestRecurringFoldUsesFirstOccurrence covers autumn. The ambiguous wall time
// must resolve deterministically to the earlier instant.
func TestRecurringFoldUsesFirstOccurrence(t *testing.T) {
	body := researchCalendarHead +
		"BEGIN:VEVENT\r\nUID:fold-series\r\n" +
		"DTSTART;TZID=America/New_York:20261025T013000\r\n" +
		"DTEND;TZID=America/New_York:20261025T023000\r\n" +
		"RRULE:FREQ=WEEKLY;COUNT=3\r\n" +
		"END:VEVENT\r\nEND:VCALENDAR\r\n"

	ny := mustLoadLocation(t, "America/New_York")
	events := researchExpand(t, body,
		time.Date(2026, time.October, 20, 0, 0, 0, 0, ny),
		time.Date(2026, time.November, 20, 0, 0, 0, 0, ny))
	if len(events) != 3 {
		t.Fatalf("events = %d, want 3: %+v", len(events), events)
	}
	if _, offset := events[0].StartTime.In(ny).Zone(); offset != -4*60*60 {
		t.Errorf("fold occurrence offset = %d, want first occurrence -04:00", offset)
	}
	if events[0].DSTAdjusted {
		t.Error("a fold is deterministic and is not a gap adjustment")
	}
	if _, offset := events[2].StartTime.In(ny).Zone(); offset != -5*60*60 {
		t.Errorf("post-fold occurrence offset = %d, want -05:00", offset)
	}
}

// TestGapEXDATEAndOverrideMatch proves the gap resolution is shared by EXDATE
// and RECURRENCE-ID, so an exclusion or override still targets its slot.
func TestGapEXDATEAndOverrideMatch(t *testing.T) {
	ny := mustLoadLocation(t, "America/New_York")
	body := researchCalendarHead +
		"BEGIN:VEVENT\r\nUID:gap-exdate\r\n" +
		"DTSTART;TZID=America/New_York:20260301T023000\r\n" +
		"DTEND;TZID=America/New_York:20260301T033000\r\n" +
		"RRULE:FREQ=WEEKLY;COUNT=3\r\n" +
		"EXDATE;TZID=America/New_York:20260308T023000\r\n" +
		"END:VEVENT\r\n" +
		"BEGIN:VEVENT\r\nUID:gap-exdate\r\n" +
		"RECURRENCE-ID;TZID=America/New_York:20260315T023000\r\n" +
		"DTSTART;TZID=America/New_York:20260315T043000\r\n" +
		"DTEND;TZID=America/New_York:20260315T053000\r\n" +
		"SUMMARY:Moved\r\n" +
		"END:VEVENT\r\nEND:VCALENDAR\r\n"

	events := researchExpand(t, body,
		time.Date(2026, time.March, 1, 0, 0, 0, 0, ny),
		time.Date(2026, time.April, 1, 0, 0, 0, 0, ny))
	if len(events) != 2 {
		t.Fatalf("events = %d, want 2: %+v", len(events), events)
	}
	for _, event := range events {
		if event.StartTime.In(ny).Day() == 8 {
			t.Errorf("EXDATE in the gap did not remove its slot: %v", event.StartTime.In(ny))
		}
	}
	if !events[1].IsOverride || events[1].StartTime.In(ny).Hour() != 4 {
		t.Errorf("override did not match its gap-adjacent slot: %+v", events[1])
	}
}

// TestAllDayAcrossDSTIsUnchanged keeps civil dates independent of the policy.
func TestAllDayAcrossDSTIsUnchanged(t *testing.T) {
	body := researchCalendarHead +
		"BEGIN:VEVENT\r\nUID:all-day-dst\r\n" +
		"DTSTART;VALUE=DATE:20260328\r\nDTEND;VALUE=DATE:20260329\r\n" +
		"RRULE:FREQ=DAILY;COUNT=3\r\n" +
		"END:VEVENT\r\nEND:VCALENDAR\r\n"

	events := researchExpand(t, body,
		time.Date(2026, time.March, 28, 0, 0, 0, 0, time.UTC),
		time.Date(2026, time.April, 2, 0, 0, 0, 0, time.UTC))
	if len(events) != 3 {
		t.Fatalf("events = %d, want 3", len(events))
	}
	for i, event := range events {
		if event.StartTime.Day() != 28+i || event.StartTime.Hour() != 0 || event.DSTAdjusted {
			t.Errorf("event %d = %v adjusted=%v", i, event.StartTime, event.DSTAdjusted)
		}
	}
}

// TestGapParsingIsIndependentOfProcessTimezone pins wall-clock handling.
func TestGapParsingIsIndependentOfProcessTimezone(t *testing.T) {
	body := researchCalendarHead +
		"BEGIN:VEVENT\r\nUID:gap-tz\r\n" +
		"DTSTART;TZID=Europe/Paris:20260329T023000\r\n" +
		"DTEND;TZID=Europe/Paris:20260329T033000\r\n" +
		"END:VEVENT\r\nEND:VCALENDAR\r\n"
	want := time.Date(2026, time.March, 29, 1, 30, 0, 0, time.UTC)
	original := time.Local
	t.Cleanup(func() { time.Local = original })
	for _, zone := range []string{"UTC", "America/New_York", "Asia/Tokyo"} {
		time.Local = mustLoadLocation(t, zone)
		master, _ := parseFloating(t, body)
		if !master.StartTime.Equal(want) || !master.DSTAdjusted {
			t.Fatalf("%s: start = %v adjusted=%v, want %v adjusted", zone, master.StartTime, master.DSTAdjusted, want)
		}
	}
}

// TestGapOccurrenceStaysBusy proves availability counts the resolved instant
// and never treats the gap as free.
func TestGapOccurrenceStaysBusy(t *testing.T) {
	ny := mustLoadLocation(t, "America/New_York")
	event := Event{
		StartTime:   time.Date(2026, time.March, 8, 3, 30, 0, 0, ny),
		EndTime:     time.Date(2026, time.March, 8, 4, 30, 0, 0, ny),
		DSTAdjusted: true,
	}
	busy := BusyFromEvents([]Event{event}, true, 0, 0, ny)
	if len(busy) != 1 || !busy[0].Start.Equal(event.StartTime) {
		t.Fatalf("busy = %+v", busy)
	}
}

// TestInputRejectsGapStart proves an agent-supplied local time that does not
// exist is rejected instead of silently normalized before any network call.
func TestInputRejectsGapStart(t *testing.T) {
	paris := mustLoadLocation(t, "Europe/Paris")
	if _, err := ParseDateTime("start", "2026-03-29T02:30:00", paris); err == nil {
		t.Fatal("gap start was accepted")
	}
	if _, err := ParseDateTime("start", "2026-03-29T03:30:00", paris); err != nil {
		t.Fatalf("valid local time was rejected: %v", err)
	}
	// An explicit offset always wins and is not reinterpreted.
	if _, err := ParseDateTime("start", "2026-03-29T02:30:00+01:00", paris); err != nil {
		t.Fatalf("explicit-offset value was rejected: %v", err)
	}
}
