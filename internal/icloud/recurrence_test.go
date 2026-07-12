package icloud

import (
	"testing"
	"time"
)

func mustParse(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return tm
}

func TestExpandOccurrences_NonRecurring(t *testing.T) {
	master := Event{
		UID:       "uid-1",
		Title:     "Simple",
		StartTime: mustParse(t, "2026-07-06T09:00:00Z"),
		EndTime:   mustParse(t, "2026-07-06T10:00:00Z"),
	}

	t.Run("within range", func(t *testing.T) {
		out, err := ExpandOccurrences(master, nil, mustParse(t, "2026-07-01T00:00:00Z"), mustParse(t, "2026-07-08T00:00:00Z"), 0)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(out) != 1 {
			t.Fatalf("len = %d, want 1", len(out))
		}
	})

	t.Run("out of range", func(t *testing.T) {
		out, err := ExpandOccurrences(master, nil, mustParse(t, "2026-08-01T00:00:00Z"), mustParse(t, "2026-08-08T00:00:00Z"), 0)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(out) != 0 {
			t.Fatalf("len = %d, want 0", len(out))
		}
	})
}

func TestExpandOccurrences_DailyWithCount(t *testing.T) {
	master := Event{
		UID:        "uid-daily",
		Title:      "Daily",
		StartTime:  mustParse(t, "2026-07-01T09:00:00Z"),
		EndTime:    mustParse(t, "2026-07-01T10:00:00Z"),
		Recurrence: "FREQ=DAILY;COUNT=5",
	}

	out, err := ExpandOccurrences(master, nil, mustParse(t, "2026-07-01T00:00:00Z"), mustParse(t, "2026-07-31T00:00:00Z"), 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 5 {
		t.Fatalf("len = %d, want 5", len(out))
	}
	for i, ev := range out {
		wantDay := 1 + i
		if ev.StartTime.Day() != wantDay {
			t.Errorf("occurrence %d: day = %d, want %d", i, ev.StartTime.Day(), wantDay)
		}
		if ev.StartTime.Hour() != 9 {
			t.Errorf("occurrence %d: hour = %d, want 9", i, ev.StartTime.Hour())
		}
	}
}

func TestExpandOccurrences_WeeklyWithExdate(t *testing.T) {
	exDate := mustParse(t, "2026-07-13T18:00:00Z")
	master := Event{
		UID:        "uid-weekly",
		Title:      "Weekly",
		StartTime:  mustParse(t, "2026-07-06T18:00:00Z"),
		EndTime:    mustParse(t, "2026-07-06T19:00:00Z"),
		Recurrence: "FREQ=WEEKLY;COUNT=5",
		exDates:    []time.Time{exDate},
	}

	out, err := ExpandOccurrences(master, nil, mustParse(t, "2026-07-01T00:00:00Z"), mustParse(t, "2026-08-15T00:00:00Z"), 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 5 occurrences minus the July 13 EXDATE = 4.
	if len(out) != 4 {
		t.Fatalf("len = %d, want 4: %+v", len(out), out)
	}
	for _, ev := range out {
		if ev.StartTime.Equal(exDate) {
			t.Errorf("excluded occurrence (EXDATE) is present: %v", ev.StartTime)
		}
	}
}

func TestExpandOccurrences_OverrideReplacesOccurrence(t *testing.T) {
	recID := mustParse(t, "2026-07-13T14:00:00Z")
	master := Event{
		UID:        "uid-override",
		Title:      "Follow-up",
		StartTime:  mustParse(t, "2026-07-06T14:00:00Z"),
		EndTime:    mustParse(t, "2026-07-06T15:00:00Z"),
		Recurrence: "FREQ=WEEKLY;COUNT=4",
	}
	override := Event{
		UID:          "uid-override",
		Title:        "Follow-up (moved)",
		StartTime:    mustParse(t, "2026-07-13T16:00:00Z"),
		EndTime:      mustParse(t, "2026-07-13T17:00:00Z"),
		recurrenceID: recID,
	}

	out, err := ExpandOccurrences(master, []Event{override}, mustParse(t, "2026-07-01T00:00:00Z"), mustParse(t, "2026-08-15T00:00:00Z"), 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 4 {
		t.Fatalf("len = %d, want 4: %+v", len(out), out)
	}

	var found bool
	for _, ev := range out {
		if ev.StartTime.Equal(mustParse(t, "2026-07-13T16:00:00Z")) {
			found = true
			if ev.Title != "Follow-up (moved)" {
				t.Errorf("Title = %q, want override title", ev.Title)
			}
		}
		if ev.StartTime.Equal(mustParse(t, "2026-07-13T14:00:00Z")) {
			t.Errorf("the original occurrence (replaced by the override) should not appear")
		}
	}
	if !found {
		t.Errorf("override not found in results: %+v", out)
	}
}

// TestExpandOccurrences_PreservesTimezoneAcrossDST: a weekly event at 10:00
// America/New_York wall clock time must stay at 10:00 wall clock across a
// DST change (US DST ends on 2026-11-01), hence at a different UTC offset
// before and after (-04:00 then -05:00, i.e. 14:00Z then 15:00Z). Forcing
// .UTC() on the Dtstart before expansion would pin the occurrence to a
// constant UTC offset, shifting it by 1h on the wall clock after the DST
// change; that would violate RFC 5545 (the recurrence time must be the
// local wall clock time, not a fixed UTC instant).
func TestExpandOccurrences_PreservesTimezoneAcrossDST(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("LoadLocation: %v", err)
	}
	start := time.Date(2026, 10, 5, 10, 0, 0, 0, loc) // Monday 10:00 NY wall clock

	// Excluded occurrence (EXDATE): simulates
	// EXDATE;TZID=America/New_York:20261019T100000 as resolved by ical.go
	// (already converted to an absolute UTC instant at this point;
	// parseExDateProp performs the TZID to UTC conversion upstream).
	excludedWallClock := time.Date(2026, 10, 19, 10, 0, 0, 0, loc)

	master := Event{
		UID:        "uid-dst",
		Title:      "Weekly NY",
		StartTime:  start,
		EndTime:    start.Add(time.Hour),
		Recurrence: "FREQ=WEEKLY;COUNT=6",
		exDates:    []time.Time{excludedWallClock.UTC()},
	}

	rangeStart := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	rangeEnd := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	out, err := ExpandOccurrences(master, nil, rangeStart, rangeEnd, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 6 weekly occurrences minus the October 19 EXDATE = 5.
	if len(out) != 5 {
		t.Fatalf("len = %d, want 5: %+v", len(out), out)
	}

	dstCutoff := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	for _, occ := range out {
		if occ.StartTime.Equal(excludedWallClock.UTC()) {
			t.Errorf("excluded occurrence (EXDATE, Oct 19 10:00) is present: %v", occ.StartTime)
		}
		wallHour := occ.StartTime.In(loc).Hour()
		if wallHour != 10 {
			t.Errorf("occurrence %v: America/New_York wall clock hour = %d, want 10 (preserved across DST)", occ.StartTime, wallHour)
		}
		wantUTCHour := 14 // EDT (-04:00) before the DST change
		if !occ.StartTime.Before(dstCutoff) {
			wantUTCHour = 15 // EST (-05:00) after the DST change
		}
		if got := occ.StartTime.UTC().Hour(); got != wantUTCHour {
			t.Errorf("occurrence %v: UTC hour = %d, want %d (correct DST offset)", occ.StartTime, got, wantUTCHour)
		}
	}
}

// TestExpandOccurrences_IncludesOccurrenceOverlappingRangeStart: an
// overnight recurring occurrence (22:00 to 02:00, crossing midnight) whose
// instance starts the day before rangeStart but spills into it (its end is
// after rangeStart) must be included, for consistency with eventOverlaps,
// which is already used for the non-recurring path.
func TestExpandOccurrences_IncludesOccurrenceOverlappingRangeStart(t *testing.T) {
	master := Event{
		UID:        "uid-overnight",
		Title:      "Night shift",
		StartTime:  mustParse(t, "2026-07-06T22:00:00Z"), // Monday 22:00
		EndTime:    mustParse(t, "2026-07-07T02:00:00Z"), // Tuesday 02:00 (4h duration)
		Recurrence: "FREQ=WEEKLY;COUNT=4",
	}

	// The range starts after the July 6 occurrence begins but before it
	// ends (it spills past midnight): the occurrence overlaps the start of
	// the range and must be included.
	rangeStart := mustParse(t, "2026-07-07T01:00:00Z")
	rangeEnd := mustParse(t, "2026-08-10T00:00:00Z")

	out, err := ExpandOccurrences(master, nil, rangeStart, rangeEnd, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var found bool
	for _, ev := range out {
		if ev.StartTime.Equal(mustParse(t, "2026-07-06T22:00:00Z")) {
			found = true
		}
	}
	if !found {
		t.Fatalf("occurrence overlapping the range start (Jul 6 22:00 to Jul 7 02:00) missing: %+v", out)
	}
}

func TestExpandOccurrences_InfiniteRRuleBoundedByRange(t *testing.T) {
	master := Event{
		UID:        "uid-infinite",
		Title:      "Endless",
		StartTime:  mustParse(t, "2026-01-01T09:00:00Z"),
		EndTime:    mustParse(t, "2026-01-01T09:30:00Z"),
		Recurrence: "FREQ=DAILY", // neither UNTIL nor COUNT
	}

	start := mustParse(t, "2026-07-01T00:00:00Z")
	end := mustParse(t, "2026-07-11T00:00:00Z") // 10 days
	out, err := ExpandOccurrences(master, nil, start, end, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 10 {
		t.Fatalf("len = %d, want 10 (bounded by the requested range)", len(out))
	}
}

func TestExpandOccurrences_MaxOccurrencesTruncates(t *testing.T) {
	master := Event{
		UID:        "uid-truncate",
		Title:      "Long daily",
		StartTime:  mustParse(t, "2026-01-01T09:00:00Z"),
		EndTime:    mustParse(t, "2026-01-01T09:30:00Z"),
		Recurrence: "FREQ=DAILY",
	}

	start := mustParse(t, "2026-01-01T00:00:00Z")
	end := mustParse(t, "2027-01-01T00:00:00Z") // 365 days
	out, err := ExpandOccurrences(master, nil, start, end, 5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 5 {
		t.Fatalf("len = %d, want 5 (truncated by maxOccurrences)", len(out))
	}
}

func TestExpandOccurrences_InvalidRRule(t *testing.T) {
	master := Event{
		UID:        "uid-invalid",
		StartTime:  mustParse(t, "2026-01-01T09:00:00Z"),
		EndTime:    mustParse(t, "2026-01-01T09:30:00Z"),
		Recurrence: "THIS-IS-NOT-A-VALID-RRULE",
	}

	_, err := ExpandOccurrences(master, nil, mustParse(t, "2026-01-01T00:00:00Z"), mustParse(t, "2026-02-01T00:00:00Z"), 0)
	if err == nil {
		t.Fatal("expected an invalid RRULE error")
	}
}
