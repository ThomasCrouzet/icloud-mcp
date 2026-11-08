package icloud

import (
	"testing"
	"time"
)

func mustLocation(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func TestAllDayOverlapUsesCivilDays(t *testing.T) {
	paris := mustLocation(t, "Europe/Paris")
	tokyo := mustLocation(t, "Asia/Tokyo")
	event := Event{
		UID: "all-day", AllDay: true,
		StartTime: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		EndTime:   time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
	}
	parisStart := time.Date(2025, 12, 31, 23, 0, 0, 0, time.UTC).In(paris)
	parisEnd := time.Date(2025, 12, 31, 23, 30, 0, 0, time.UTC).In(paris)
	if !eventOverlaps(event, parisStart, parisEnd) {
		t.Fatal("Paris local midnight all-day event was excluded")
	}
	if eventOverlaps(event, parisStart.Add(-24*time.Hour), parisStart) {
		t.Fatal("all-day event overlapped the previous local day")
	}
	tokyoStart := time.Date(2025, 12, 31, 15, 0, 0, 0, time.UTC).In(tokyo)
	tokyoEnd := time.Date(2025, 12, 31, 15, 30, 0, 0, time.UTC).In(tokyo)
	if !eventOverlaps(event, tokyoStart, tokyoEnd) {
		t.Fatal("Tokyo local midnight all-day event was excluded")
	}

	busy := BusyFromEvents([]Event{event}, true, 0, 0, paris)
	if len(busy) != 1 {
		t.Fatalf("busy intervals = %#v", busy)
	}
	wantStart := time.Date(2026, 1, 1, 0, 0, 0, 0, paris)
	if !busy[0].Start.Equal(wantStart) || !busy[0].End.Equal(wantStart.Add(24*time.Hour)) {
		t.Fatalf("busy interval = %#v, want civil day in Paris", busy[0])
	}
}

func TestAllDayRecurringCivilDayBoundary(t *testing.T) {
	paris := mustLocation(t, "Europe/Paris")
	master := Event{
		UID: "all-day-series", AllDay: true,
		StartTime: time.Date(2026, 3, 28, 0, 0, 0, 0, time.UTC),
		EndTime:   time.Date(2026, 3, 29, 0, 0, 0, 0, time.UTC),
		Recurrence: "FREQ=DAILY;COUNT=3",
	}
	start := time.Date(2026, 3, 28, 23, 30, 0, 0, time.UTC).In(paris)
	end := time.Date(2026, 3, 29, 0, 30, 0, 0, time.UTC).In(paris)
	occurrences, _, err := ExpandOccurrences(master, nil, start, end, 0)
	if err != nil {
		t.Fatalf("ExpandOccurrences() error = %v", err)
	}
	if len(occurrences) != 1 {
		t.Fatalf("all-day occurrences = %d, want 1", len(occurrences))
	}
}

func TestFloatingEventUsesEffectiveZone(t *testing.T) {
	paris := mustLocation(t, "Europe/Paris")
	event := Event{
		UID: "floating", IsFloating: true,
		StartTime: time.Date(2026, 7, 1, 12, 30, 0, 0, time.UTC),
		EndTime:   time.Date(2026, 7, 1, 12, 45, 0, 0, time.UTC),
	}
	rangeStart := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC).In(paris)
	rangeEnd := time.Date(2026, 7, 1, 11, 0, 0, 0, time.UTC).In(paris)
	if !eventOverlaps(event, rangeStart, rangeEnd) {
		t.Fatal("floating 12:30 Paris event was excluded from 10:00Z-11:00Z")
	}
	busy := BusyFromEvents([]Event{event}, true, 0, 0, paris)
	if len(busy) != 1 || !busy[0].Start.Equal(time.Date(2026, 7, 1, 10, 30, 0, 0, time.UTC)) {
		t.Fatalf("floating busy = %#v", busy)
	}
}

func TestFloatingWallClockGapUsesResolvePolicy(t *testing.T) {
	paris := mustLocation(t, "Europe/Paris")
	gap := time.Date(2026, 3, 29, 2, 30, 0, 0, time.UTC)
	resolved := WallClockInLocation(gap, paris)
	expected, adjusted := ResolveWallClock(gap, paris)
	if !adjusted {
		t.Fatal("expected a spring-gap adjustment")
	}
	if !resolved.Equal(expected) {
		t.Fatalf("WallClockInLocation() = %v, want %v", resolved, expected)
	}
}
